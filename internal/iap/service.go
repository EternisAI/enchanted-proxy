package iap

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	appstore "github.com/richzw/appstore"
)

type Service struct {
	queries pgdb.Querier
	parser  transactionParser
	policy  Policy
	now     func() time.Time
}

func NewService(queries pgdb.Querier, policy Policy) *Service {
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

	return &Service{queries: queries, parser: appleParser{store: prodClient}, policy: policy, now: time.Now}
}

// AttachAppStoreSubscription verifies a signed transaction, checks it against
// the policy and writes the plan it grants. A purchase the policy does not
// accept returns a *Refusal and writes nothing.
func (s *Service) AttachAppStoreSubscription(ctx context.Context, userID string, identity *auth.Identity, jwsTransactionInfo string) (payload *appstore.JWSTransaction, expiresAt time.Time, err error) {
	p, err := s.parser.ParseTransaction(jwsTransactionInfo)
	if err != nil {
		return nil, time.Time{}, err
	}
	prod, refusal := s.policy.check(p, identity, s.now())
	if refusal != nil {
		return nil, time.Time{}, refusal
	}
	expiresAt = prod.expiry(p)

	if err := s.queries.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
		UserID:                userID,
		SubscriptionTier:      string(prod.tier),
		SubscriptionExpiresAt: sql.NullTime{Time: expiresAt, Valid: true},
		SubscriptionProvider:  "apple",
		StripeCustomerID:      nil, // Don't set for Apple subscriptions
	}); err != nil {
		return nil, time.Time{}, err
	}

	return p, expiresAt, nil
}
