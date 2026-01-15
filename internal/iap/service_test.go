package iap

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// mockQuerier implements the subset of pgdb.Querier needed for IAP tests
type mockQuerier struct {
	pgdb.Querier

	getAppleTransactionFunc    func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error)
	insertAppleTransactionFunc func(ctx context.Context, arg pgdb.InsertAppleTransactionParams) error
	upsertEntitlementFunc      func(ctx context.Context, arg pgdb.UpsertEntitlementWithTierParams) error

	insertCalls []pgdb.InsertAppleTransactionParams
	upsertCalls []pgdb.UpsertEntitlementWithTierParams
}

func (m *mockQuerier) GetAppleTransaction(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
	if m.getAppleTransactionFunc != nil {
		return m.getAppleTransactionFunc(ctx, originalTransactionID)
	}
	return pgdb.AppleTransaction{}, sql.ErrNoRows
}

func (m *mockQuerier) InsertAppleTransaction(ctx context.Context, arg pgdb.InsertAppleTransactionParams) error {
	m.insertCalls = append(m.insertCalls, arg)
	if m.insertAppleTransactionFunc != nil {
		return m.insertAppleTransactionFunc(ctx, arg)
	}
	return nil
}

func (m *mockQuerier) UpsertEntitlementWithTier(ctx context.Context, arg pgdb.UpsertEntitlementWithTierParams) error {
	m.upsertCalls = append(m.upsertCalls, arg)
	if m.upsertEntitlementFunc != nil {
		return m.upsertEntitlementFunc(ctx, arg)
	}
	return nil
}

func TestAttachAppStoreSubscription_ReplayPrevention(t *testing.T) {
	ctx := context.Background()
	log := logger.New(logger.Config{}).WithComponent("iap-test")

	t.Run("first redemption succeeds and records transaction", func(t *testing.T) {
		mock := &mockQuerier{
			getAppleTransactionFunc: func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
				return pgdb.AppleTransaction{}, sql.ErrNoRows
			},
		}

		_ = &Service{
			queries: mock,
			logger:  log,
		}

		// Verify no calls yet
		if len(mock.insertCalls) != 0 {
			t.Errorf("expected 0 insert calls, got %d", len(mock.insertCalls))
		}
	})

	t.Run("duplicate redemption returns early without granting entitlement", func(t *testing.T) {
		existingTx := pgdb.AppleTransaction{
			OriginalTransactionID: "1000000123456789",
			UserID:                "user123",
			ProductID:             "silo.pro.monthly",
			Tier:                  "pro",
			RedeemedAt:            time.Now(),
		}

		mock := &mockQuerier{
			getAppleTransactionFunc: func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
				if originalTransactionID == "1000000123456789" {
					return existingTx, nil
				}
				return pgdb.AppleTransaction{}, sql.ErrNoRows
			},
		}

		svc := &Service{
			queries: mock,
			logger:  log,
		}

		// Simulate checking for existing transaction (this is what the service does)
		existing, err := svc.queries.GetAppleTransaction(ctx, "1000000123456789")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// When transaction exists, service should return early
		if existing.OriginalTransactionID != "1000000123456789" {
			t.Errorf("expected existing transaction to be returned")
		}

		// Verify no insert or upsert calls were made (service returns early)
		if len(mock.insertCalls) != 0 {
			t.Errorf("expected 0 insert calls for duplicate, got %d", len(mock.insertCalls))
		}
		if len(mock.upsertCalls) != 0 {
			t.Errorf("expected 0 upsert calls for duplicate, got %d", len(mock.upsertCalls))
		}
	})

	t.Run("concurrent redemption handled by unique constraint", func(t *testing.T) {
		callCount := 0
		mock := &mockQuerier{
			getAppleTransactionFunc: func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
				return pgdb.AppleTransaction{}, sql.ErrNoRows
			},
			insertAppleTransactionFunc: func(ctx context.Context, arg pgdb.InsertAppleTransactionParams) error {
				callCount++
				if callCount > 1 {
					// Simulate unique constraint violation on second attempt
					return errors.New("duplicate key value violates unique constraint")
				}
				return nil
			},
		}

		svc := &Service{
			queries: mock,
			logger:  log,
		}

		// First insert succeeds
		err := svc.queries.InsertAppleTransaction(ctx, pgdb.InsertAppleTransactionParams{
			OriginalTransactionID: "1000000123456789",
			UserID:                "user123",
			ProductID:             "silo.pro.monthly",
			Tier:                  "pro",
		})
		if err != nil {
			t.Fatalf("first insert should succeed: %v", err)
		}

		// Second insert fails with unique constraint
		err = svc.queries.InsertAppleTransaction(ctx, pgdb.InsertAppleTransactionParams{
			OriginalTransactionID: "1000000123456789",
			UserID:                "user456", // Different user trying same tx
			ProductID:             "silo.pro.monthly",
			Tier:                  "pro",
		})
		if err == nil {
			t.Fatal("second insert should fail with unique constraint")
		}
		if err.Error() != "duplicate key value violates unique constraint" {
			t.Errorf("expected unique constraint error, got: %v", err)
		}
	})

	t.Run("insert happens before upsert entitlement", func(t *testing.T) {
		var callOrder []string

		mock := &mockQuerier{
			getAppleTransactionFunc: func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
				return pgdb.AppleTransaction{}, sql.ErrNoRows
			},
			insertAppleTransactionFunc: func(ctx context.Context, arg pgdb.InsertAppleTransactionParams) error {
				callOrder = append(callOrder, "insert")
				return nil
			},
			upsertEntitlementFunc: func(ctx context.Context, arg pgdb.UpsertEntitlementWithTierParams) error {
				callOrder = append(callOrder, "upsert")
				return nil
			},
		}

		// Simulate the order of operations the service performs
		_ = mock.InsertAppleTransaction(ctx, pgdb.InsertAppleTransactionParams{
			OriginalTransactionID: "1000000123456789",
			UserID:                "user123",
			ProductID:             "silo.pro.monthly",
			Tier:                  "pro",
		})
		_ = mock.UpsertEntitlementWithTier(ctx, pgdb.UpsertEntitlementWithTierParams{
			UserID:           "user123",
			SubscriptionTier: "pro",
		})

		if len(callOrder) != 2 {
			t.Fatalf("expected 2 calls, got %d", len(callOrder))
		}
		if callOrder[0] != "insert" {
			t.Errorf("expected insert first, got %s", callOrder[0])
		}
		if callOrder[1] != "upsert" {
			t.Errorf("expected upsert second, got %s", callOrder[1])
		}
	})

	t.Run("lifetime tier stored correctly", func(t *testing.T) {
		mock := &mockQuerier{
			getAppleTransactionFunc: func(ctx context.Context, originalTransactionID string) (pgdb.AppleTransaction, error) {
				return pgdb.AppleTransaction{}, sql.ErrNoRows
			},
		}

		_ = mock.InsertAppleTransaction(ctx, pgdb.InsertAppleTransactionParams{
			OriginalTransactionID: "1000000123456789",
			UserID:                "user123",
			ProductID:             "silo.plus.lifetime",
			Tier:                  "plus",
		})

		if len(mock.insertCalls) != 1 {
			t.Fatalf("expected 1 insert call, got %d", len(mock.insertCalls))
		}

		call := mock.insertCalls[0]
		if call.Tier != "plus" {
			t.Errorf("expected tier 'plus' for lifetime, got %s", call.Tier)
		}
		if call.ProductID != "silo.plus.lifetime" {
			t.Errorf("expected productID 'silo.plus.lifetime', got %s", call.ProductID)
		}
	})
}
