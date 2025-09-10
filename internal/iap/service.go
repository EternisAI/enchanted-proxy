package iap

import (
	"context"
	"database/sql"
	"time"

	appstore "github.com/richzw/appstore"

	"github.com/eternisai/enchanted-proxy/internal/config"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

type Service struct {
	queries pgdb.Querier
	store   *appstore.StoreClient
}

func NewService(queries pgdb.Querier) *Service {
	client := appstore.NewStoreClient(&appstore.StoreConfig{
		KeyContent: []byte(config.AppConfig.AppStoreAPIKeyP8),
		KeyID:      config.AppConfig.AppStoreAPIKeyID,
		BundleID:   config.AppConfig.AppStoreBundleID,
		Issuer:     config.AppConfig.AppStoreIssuerID,
		Sandbox:    config.AppConfig.AppStoreSandbox,
	})
	return &Service{queries: queries, store: client}
}

// AttachAppStoreSubscription verifies the JWS and upserts entitlement.
func (s *Service) AttachAppStoreSubscription(ctx context.Context, userID string, signedTransactionInfo string) (payload *appstore.JWSTransaction, proUntil time.Time, err error) {
	p, err := s.store.ParseNotificationV2TransactionInfo(signedTransactionInfo)
	if err != nil {
		return nil, time.Time{}, err
	}

	var expiresAt sql.NullTime
	if p.ExpiresDate > 0 {
		expiresAt = sql.NullTime{Time: time.UnixMilli(p.ExpiresDate), Valid: true}
	} else {
		expiresAt = sql.NullTime{Time: time.Now().AddDate(10, 0, 0), Valid: true}
	}

	if err := s.queries.UpsertEntitlement(ctx, pgdb.UpsertEntitlementParams{
		UserID:       userID,
		ProExpiresAt: expiresAt,
	}); err != nil {
		return nil, time.Time{}, err
	}

	return p, expiresAt.Time, nil
}
