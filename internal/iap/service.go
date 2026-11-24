package iap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/quota"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	appstore "github.com/richzw/appstore"
)

// GroupCache is told when a purchase has joined quota groups, so this server
// instance applies the new groups at once.
type GroupCache interface {
	Changed()
}

// Service binds Apple purchases to accounts. A purchase grants its plan to one
// account at a time, its holder; every account it has been attached to shares
// one quota group.
type Service struct {
	db      *sql.DB
	queries pgdb.Querier
	parser  transactionParser
	apple   AppleAPI
	policy  Policy
	groups  GroupCache
	logger  *logger.Logger
	now     func() time.Time
}

// NewService returns the purchase service with App Store clients built from
// the configuration.
func NewService(db *sql.DB, queries pgdb.Querier, policy Policy, groups GroupCache, log *logger.Logger) *Service {
	// Normalize P8: support both literal newlines and \n-escaped forms.
	key := config.AppConfig.AppStoreAPIKeyP8
	if strings.Contains(key, "\\n") && !strings.Contains(key, "\n") {
		key = strings.ReplaceAll(key, "\\n", "\n")
	}
	client := func(sandbox bool) *appstore.StoreClient {
		return appstore.NewStoreClient(&appstore.StoreConfig{
			KeyContent: []byte(key),
			KeyID:      config.AppConfig.AppStoreAPIKeyID,
			BundleID:   config.AppConfig.AppStoreBundleID,
			Issuer:     config.AppConfig.AppStoreIssuerID,
			Sandbox:    sandbox,
		})
	}
	production := client(false)
	return &Service{
		db:      db,
		queries: queries,
		parser:  appleParser{store: production},
		apple:   storeAPI{production: production, sandbox: client(true)},
		policy:  policy,
		groups:  groups,
		logger:  log,
		now:     time.Now,
	}
}

// Attached is the outcome of an attach.
type Attached struct {
	Transaction *appstore.JWSTransaction
	// ExpiresAt is when the purchase's plan ends; zero when Blocked.
	ExpiresAt time.Time
	// Blocked means the purchase is blocked and granted nothing.
	Blocked bool
}

// ErrAppleUnavailable means the App Store could not be asked to confirm a
// purchase that needs confirming; nothing was written.
var ErrAppleUnavailable = errors.New("the App Store could not confirm the purchase")

// errHolderChanged means the purchase moved to another account between the
// first read and the lock, so the attach must start again and confirm it.
var errHolderChanged = errors.New("purchase holder changed")

// AttachAppStoreSubscription binds a signed transaction's purchase to the
// caller and writes the plan it grants. It returns a *Refusal for a purchase
// the policy does not accept, ErrAppleUnavailable when the App Store could not
// confirm it, and another error when the transaction is not a valid signed
// transaction or a write failed. Nothing is written in any of these cases,
// except that a revoked purchase on record is blocked.
func (s *Service) AttachAppStoreSubscription(ctx context.Context, userID string, identity *auth.Identity, jws string) (*Attached, error) {
	tx, err := s.parser.ParseTransaction(jws)
	if err != nil {
		return nil, err
	}
	if tx.OriginalTransactionId == "" || tx.TransactionID == "" {
		return nil, errors.New("signed transaction without transaction IDs")
	}
	if _, refusal := s.policy.check(tx, identity, s.now()); refusal != nil {
		if refusal.Reason == refusedRevoked {
			return nil, s.revoke(ctx, userID, tx, refusal)
		}
		return nil, refusal
	}

	stored, err := s.queries.GetApplePurchase(ctx, tx.OriginalTransactionId)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("failed to read purchase: %w", err)
	}
	// A purchase seen for the first time, or reaching another account, is
	// confirmed with the App Store first: its holder changes only on Apple's
	// word, not on a token alone.
	confirm := err != nil || stored.HolderUserID != userID
	for attempt := 0; ; attempt++ {
		current := tx
		if confirm {
			if current, err = s.confirm(ctx, userID, tx, identity); err != nil {
				return nil, err
			}
		}
		attached, err := s.attach(ctx, userID, current, identity, confirm)
		if errors.Is(err, errHolderChanged) && attempt == 0 {
			confirm = true
			continue
		}
		return attached, err
	}
}

// confirm reads the transaction from the App Store and checks the policy
// against what the App Store says. A purchase the App Store now reports
// revoked is blocked, as when the app sends a revoked transaction.
func (s *Service) confirm(ctx context.Context, userID string, tx *appstore.JWSTransaction, identity *auth.Identity) (*appstore.JWSTransaction, error) {
	signed, err := s.apple.SignedTransaction(ctx, tx.Environment, tx.TransactionID)
	if errors.Is(err, errNotAtApple) {
		return nil, refuse(refusedUnknown, "The App Store does not know this purchase.")
	}
	if err != nil {
		s.logger.WithContext(ctx).Warn("app store confirmation failed", slog.String("error", err.Error()))
		return nil, ErrAppleUnavailable
	}
	current, err := s.parser.ParseTransaction(signed)
	if err != nil {
		s.logger.WithContext(ctx).Error("app store returned an invalid transaction", slog.String("error", err.Error()))
		return nil, ErrAppleUnavailable
	}
	if current.OriginalTransactionId != tx.OriginalTransactionId || current.TransactionID != tx.TransactionID {
		return nil, refuse(refusedUnknown, "The App Store does not know this purchase.")
	}
	if _, refusal := s.policy.check(current, identity, s.now()); refusal != nil {
		if refusal.Reason == refusedRevoked {
			return nil, s.revoke(ctx, userID, current, refusal)
		}
		return nil, refusal
	}
	return current, nil
}

// attach makes the binding in one transaction, with the purchase's row
// locked. confirmed says the App Store confirmed the transaction, which a
// change of holder requires.
func (s *Service) attach(ctx context.Context, userID string, tx *appstore.JWSTransaction, identity *auth.Identity, confirmed bool) (*Attached, error) {
	prod, refusal := s.policy.check(tx, identity, s.now())
	if refusal != nil {
		return nil, refusal
	}
	purchaseID := tx.OriginalTransactionId
	expiresAt := prod.expiry(tx)
	var purchaseExpiry sql.NullTime
	if !prod.lifetime {
		purchaseExpiry = sql.NullTime{Time: expiresAt, Valid: true}
	}

	var result *Attached
	joined := false
	err := s.inTx(ctx, func(q pgdb.Querier) error {
		inserted, err := q.InsertApplePurchase(ctx, pgdb.InsertApplePurchaseParams{
			OriginalTransactionID: purchaseID,
			ProductID:             tx.ProductID,
			Environment:           string(tx.Environment),
			Tier:                  string(prod.tier),
			ExpiresAt:             purchaseExpiry,
			HolderUserID:          userID,
			LastTransactionID:     tx.TransactionID,
		})
		if err != nil {
			return fmt.Errorf("failed to record purchase: %w", err)
		}
		row, err := q.LockApplePurchase(ctx, purchaseID)
		if err != nil {
			return fmt.Errorf("failed to lock purchase: %w", err)
		}
		if row.HolderUserID != userID && !confirmed {
			return errHolderChanged
		}
		if row.BlockedAt.Valid {
			// Its accounts were set to Free when it was blocked; this keeps
			// any that got a plan from it since then Free too.
			if _, err := q.ResetApplePurchaseEntitlements(ctx, &purchaseID); err != nil {
				return fmt.Errorf("failed to reset entitlements: %w", err)
			}
			result = &Attached{Transaction: tx, Blocked: true}
			return nil
		}

		reached := false
		switch {
		case inserted > 0:
			reached = true
			if err := recordEvent(ctx, q, purchaseID, "attach", userID, nil, tx.TransactionID, nil); err != nil {
				return err
			}
		case row.HolderUserID != userID:
			reached = true
			previous := row.HolderUserID
			// The groups lock comes before any entitlement row, so two moves
			// between the same accounts cannot deadlock.
			if joined, err = quota.Join(ctx, q, previous, userID); err != nil {
				return err
			}
			if _, err := q.ResetApplePurchaseEntitlement(ctx, pgdb.ResetApplePurchaseEntitlementParams{
				UserID: previous, AppleOriginalTransactionID: &purchaseID,
			}); err != nil {
				return fmt.Errorf("failed to reset the previous holder: %w", err)
			}
			if err := recordEvent(ctx, q, purchaseID, "move", userID, &previous, tx.TransactionID, nil); err != nil {
				return err
			}
		}
		if err := q.UpdateApplePurchase(ctx, pgdb.UpdateApplePurchaseParams{
			OriginalTransactionID: purchaseID,
			ProductID:             tx.ProductID,
			Environment:           string(tx.Environment),
			Tier:                  string(prod.tier),
			ExpiresAt:             purchaseExpiry,
			HolderUserID:          userID,
			LastTransactionID:     tx.TransactionID,
		}); err != nil {
			return fmt.Errorf("failed to update purchase: %w", err)
		}

		if reached {
			accounts, err := q.CountApplePurchaseAccountsSince(ctx, pgdb.CountApplePurchaseAccountsSinceParams{
				PurchaseID: purchaseID, Since: s.now().Add(-accountWindow),
			})
			if err != nil {
				return fmt.Errorf("failed to count the purchase's accounts: %w", err)
			}
			switch {
			case int(accounts) > s.policy.MaxAccounts:
				if err := block(ctx, q, purchaseID, userID, "too_many_accounts", sql.NullTime{}); err != nil {
					return err
				}
				s.logger.WithContext(ctx).Warn("apple purchase blocked",
					slog.String("original_transaction_id", purchaseID),
					slog.String("reason", "too_many_accounts"),
					slog.Int("accounts_in_30_days", int(accounts)))
				result = &Attached{Transaction: tx, Blocked: true}
				return nil
			case int(accounts) > signalAccounts:
				s.logger.WithContext(ctx).Warn("apple purchase reached many accounts",
					slog.String("original_transaction_id", purchaseID),
					slog.Int("accounts_in_30_days", int(accounts)))
			}
		}

		if err := grant(ctx, q, userID, purchaseID, prod.tier, expiresAt, s.now()); err != nil {
			return err
		}
		result = &Attached{Transaction: tx, ExpiresAt: expiresAt}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if joined && s.groups != nil {
		s.groups.Changed()
	}
	return result, nil
}

// revoke blocks a purchase Apple reports refunded or revoked, if it is on
// record, and returns the refusal.
func (s *Service) revoke(ctx context.Context, userID string, tx *appstore.JWSTransaction, refusal *Refusal) error {
	purchaseID := tx.OriginalTransactionId
	if purchaseID == "" {
		return refusal
	}
	blocked := false
	err := s.inTx(ctx, func(q pgdb.Querier) error {
		row, err := q.LockApplePurchase(ctx, purchaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // nothing was granted from it here
		}
		if err != nil {
			return fmt.Errorf("failed to lock purchase: %w", err)
		}
		if row.BlockedAt.Valid {
			return nil
		}
		blocked = true
		return block(ctx, q, purchaseID, userID, "revoked", sql.NullTime{Time: time.UnixMilli(tx.RevocationDate).UTC(), Valid: true})
	})
	if err != nil {
		return err
	}
	if blocked {
		s.logger.WithContext(ctx).Warn("apple purchase blocked",
			slog.String("original_transaction_id", purchaseID),
			slog.String("reason", "revoked"))
	}
	return refusal
}

// block blocks a purchase and sets every account whose plan came from it to
// Free. revokedAt is when Apple revoked it, if that is the reason.
func block(ctx context.Context, q pgdb.Querier, purchaseID, userID, reason string, revokedAt sql.NullTime) error {
	if err := q.BlockApplePurchase(ctx, pgdb.BlockApplePurchaseParams{OriginalTransactionID: purchaseID, Reason: &reason, RevokedAt: revokedAt}); err != nil {
		return fmt.Errorf("failed to block purchase: %w", err)
	}
	if _, err := q.ResetApplePurchaseEntitlements(ctx, &purchaseID); err != nil {
		return fmt.Errorf("failed to reset entitlements: %w", err)
	}
	return recordEvent(ctx, q, purchaseID, "block", userID, nil, "", &reason)
}

func recordEvent(ctx context.Context, q pgdb.Querier, purchaseID, kind, userID string, previous *string, transactionID string, reason *string) error {
	var txID *string
	if transactionID != "" {
		txID = &transactionID
	}
	if err := q.RecordApplePurchaseEvent(ctx, pgdb.RecordApplePurchaseEventParams{
		OriginalTransactionID: purchaseID, Kind: kind, UserID: userID, PreviousUserID: previous, TransactionID: txID, Reason: reason,
	}); err != nil {
		return fmt.Errorf("failed to record purchase event: %w", err)
	}
	return nil
}

// grant writes the purchase's plan to the caller's entitlement, unless the
// caller holds a better plan, or as good a plan from another provider or from
// another Apple purchase. An Apple entitlement with no purchase on record is
// replaced by an equal plan from this purchase, which records it. The app
// sends every purchase at each launch, so the order of the calls must not
// decide between plans.
func grant(ctx context.Context, q pgdb.Querier, userID, purchaseID string, tier tiers.Tier, expiresAt, now time.Time) error {
	// Another purchase's grant for the same account waits here and then reads
	// this one's committed row; without it, two grants to an account with no
	// row would both write, and the later commit would win.
	if err := q.LockAccountEntitlement(ctx, userID); err != nil {
		return fmt.Errorf("failed to lock entitlement: %w", err)
	}
	current, err := q.LockEntitlement(ctx, userID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("failed to read entitlement: %w", err)
	default:
		held := tiers.Rank(tiers.Tier(current.SubscriptionTier))
		if current.SubscriptionExpiresAt.Valid && !current.SubscriptionExpiresAt.Time.After(now) {
			held = tiers.Rank(tiers.TierFree)
		}
		offered := tiers.Rank(tier)
		sameSource := current.SubscriptionProvider == "apple" &&
			(current.AppleOriginalTransactionID == nil || *current.AppleOriginalTransactionID == purchaseID)
		if offered < held || (offered == held && !sameSource) {
			return nil
		}
	}
	if err := q.SetAppleEntitlement(ctx, pgdb.SetAppleEntitlementParams{
		UserID:                     userID,
		SubscriptionTier:           string(tier),
		SubscriptionExpiresAt:      sql.NullTime{Time: expiresAt, Valid: true},
		AppleOriginalTransactionID: &purchaseID,
	}); err != nil {
		return fmt.Errorf("failed to write entitlement: %w", err)
	}
	return nil
}

// inTx runs fn in a database transaction.
func (s *Service) inTx(ctx context.Context, fn func(q pgdb.Querier) error) error {
	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = dbTx.Rollback() }()
	if err := fn(pgdb.New(dbTx)); err != nil {
		return err
	}
	if err := dbTx.Commit(); err != nil {
		return fmt.Errorf("failed to commit: %w", err)
	}
	return nil
}
