package iap

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	appstore "github.com/richzw/appstore"

	"github.com/eternisai/enchanted-proxy/internal/config"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
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

	var expiresAt sql.NullTime
	if p.ExpiresDate > 0 {
		expiresAt = sql.NullTime{Time: time.UnixMilli(p.ExpiresDate), Valid: true}
	} else {
		return nil, time.Time{}, fmt.Errorf("missing expiresDate")
	}

	if err := s.queries.UpsertEntitlement(ctx, pgdb.UpsertEntitlementParams{
		UserID:       userID,
		ProExpiresAt: expiresAt,
	}); err != nil {
		return nil, time.Time{}, err
	}

	return p, expiresAt.Time, nil
}
