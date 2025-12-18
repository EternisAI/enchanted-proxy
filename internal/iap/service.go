package iap

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	appstore "github.com/richzw/appstore"
)

type Service struct {
	queries      pgdb.Querier
	storeProd    *appstore.StoreClient
	storeSandbox *appstore.StoreClient
}

func NewService(queries pgdb.Querier) *Service {
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

	return &Service{queries: queries, storeProd: prodClient, storeSandbox: sandboxClient}
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

	// Determine tier based on product ID
	tier := "pro"
	if strings.Contains(p.ProductID, "plus.lifetime") {
		tier = "plus"
	}

	var expiresAt sql.NullTime
	if p.ExpiresDate > 0 {
		expiresAt = sql.NullTime{Time: time.UnixMilli(p.ExpiresDate), Valid: true}
	} else if tier == "plus" {
		// Lifetime purchases don't expire - set far future date
		expiresAt = sql.NullTime{Time: time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC), Valid: true}
	} else {
		return nil, time.Time{}, fmt.Errorf("missing expiresDate for non-lifetime product")
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
