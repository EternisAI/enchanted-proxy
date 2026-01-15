package iap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	appstore "github.com/richzw/appstore"
)

type Service struct {
	queries      pgdb.Querier
	logger       *logger.Logger
	storeProd    *appstore.StoreClient
	storeSandbox *appstore.StoreClient
}

func NewService(queries pgdb.Querier, log *logger.Logger) *Service {
	// Normalize P8: support both literal newlines and \n-escaped forms.
	key := config.AppConfig.AppStoreAPIKeyP8
	if strings.Contains(key, "\\n") && !strings.Contains(key, "\n") {
		key = strings.ReplaceAll(key, "\\n", "\n")
	}

	prodClient := appstore.NewStoreClient(&appstore.StoreConfig{
		KeyContent: []byte(key),
		KeyID:      config.AppConfig.AppStoreAPIKeyID,
		BundleID:   config.AppConfig.AppStoreBundleID,
		Issuer:     config.AppConfig.AppStoreIssuerID,
		Sandbox:    false,
	})

	sandboxClient := appstore.NewStoreClient(&appstore.StoreConfig{
		KeyContent: []byte(key),
		KeyID:      config.AppConfig.AppStoreAPIKeyID,
		BundleID:   config.AppConfig.AppStoreBundleID,
		Issuer:     config.AppConfig.AppStoreIssuerID,
		Sandbox:    true,
	})

	return &Service{queries: queries, logger: log, storeProd: prodClient, storeSandbox: sandboxClient}
}

// AttachAppStoreSubscription verifies the JWS and upserts entitlement.
func (s *Service) AttachAppStoreSubscription(ctx context.Context, userID string, jwsTransactionInfo string) (payload *appstore.JWSTransaction, proUntil time.Time, err error) {
	p, err := s.storeProd.ParseNotificationV2TransactionInfo(jwsTransactionInfo)
	if err != nil {
		p, err = s.storeSandbox.ParseNotificationV2TransactionInfo(jwsTransactionInfo)
		if err != nil {
			return nil, time.Time{}, err
		}
	}

	// Check if transaction was already redeemed (idempotency check)
	existing, err := s.queries.GetAppleTransaction(ctx, p.OriginalTransactionId)
	if err == nil {
		// Transaction already redeemed - return success (idempotent)
		s.logger.Info("apple transaction already redeemed (idempotent)",
			"user_id", userID,
			"original_transaction_id", p.OriginalTransactionId,
		)
		// Return the previously granted expiration based on tier
		var expiresAt time.Time
		if existing.Tier == string(tiers.TierPlus) {
			expiresAt = time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)
		} else if p.ExpiresDate > 0 {
			expiresAt = time.UnixMilli(p.ExpiresDate)
		}
		return p, expiresAt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, time.Time{}, fmt.Errorf("failed to check transaction redemption: %w", err)
	}

	// Determine tier based on product ID
	// Use HasPrefix to handle environment suffixes (e.g., silo.plus.lifetime.development)
	tier := string(tiers.TierPro)
	if strings.HasPrefix(p.ProductID, "silo.plus.lifetime") {
		tier = string(tiers.TierPlus)
	}

	var expiresAt sql.NullTime
	if p.ExpiresDate > 0 {
		expiresAt = sql.NullTime{Time: time.UnixMilli(p.ExpiresDate), Valid: true}
	} else if tier == string(tiers.TierPlus) {
		// Lifetime purchases don't expire - set far future date
		expiresAt = sql.NullTime{Time: time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC), Valid: true}
	} else {
		return nil, time.Time{}, fmt.Errorf("missing expiresDate for non-lifetime product")
	}

	// Record the redemption first (prevents replay attacks)
	err = s.queries.InsertAppleTransaction(ctx, pgdb.InsertAppleTransactionParams{
		OriginalTransactionID: p.OriginalTransactionId,
		UserID:                userID,
		ProductID:             p.ProductID,
		Tier:                  tier,
	})
	if err != nil {
		// Check for unique constraint violation (concurrent redemption)
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "unique constraint") {
			s.logger.Info("apple transaction already redeemed (concurrent)",
				"user_id", userID,
				"original_transaction_id", p.OriginalTransactionId,
			)
			return p, expiresAt.Time, nil
		}
		return nil, time.Time{}, fmt.Errorf("failed to record transaction: %w", err)
	}

	provider := "apple"
	if err := s.queries.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
		UserID:                userID,
		SubscriptionTier:      tier,
		SubscriptionExpiresAt: expiresAt,
		SubscriptionProvider:  provider,
		StripeCustomerID:      nil, // Don't set for Apple subscriptions
	}); err != nil {
		return nil, time.Time{}, err
	}

	return p, expiresAt.Time, nil
}
