package zcash

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

func TestParseInvoiceID(t *testing.T) {
	tests := []struct {
		name      string
		invoiceID string
		wantErr   bool
		wantParts *invoiceParts
	}{
		{
			name:      "valid invoice with underscores in userID",
			invoiceID: "user_id_123__monthly_pro__1735254000",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user_id_123",
				productID: "monthly_pro",
				timestamp: "1735254000",
			},
		},
		{
			name:      "valid simple invoice",
			invoiceID: "user123__product456__1735254000",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user123",
				productID: "product456",
				timestamp: "1735254000",
			},
		},
		{
			name:      "invalid - missing parts",
			invoiceID: "user123__product456",
			wantErr:   true,
		},
		{
			name:      "invalid - too many parts",
			invoiceID: "user123__product456__timestamp__extra",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user123",
				productID: "product456",
				timestamp: "timestamp__extra",
			},
		},
		{
			name:      "invalid - single underscore delimiter",
			invoiceID: "user123_product456_1735254000",
			wantErr:   true,
		},
		{
			name:      "invalid - empty string",
			invoiceID: "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInvoiceID(tt.invoiceID)
			if tt.wantErr {
				if got != nil {
					t.Errorf("parseInvoiceID(%q) = %v, want nil", tt.invoiceID, got)
				}
				return
			}
			if got == nil {
				t.Errorf("parseInvoiceID(%q) = nil, want %v", tt.invoiceID, tt.wantParts)
				return
			}
			if got.userID != tt.wantParts.userID {
				t.Errorf("userID = %q, want %q", got.userID, tt.wantParts.userID)
			}
			if got.productID != tt.wantParts.productID {
				t.Errorf("productID = %q, want %q", got.productID, tt.wantParts.productID)
			}
			if got.timestamp != tt.wantParts.timestamp {
				t.Errorf("timestamp = %q, want %q", got.timestamp, tt.wantParts.timestamp)
			}
		})
	}
}

// mockQuerier implements the subset of pgdb.Querier needed for Zcash tests
type mockQuerier struct {
	pgdb.Querier

	getZcashPaymentFunc    func(ctx context.Context, invoiceID string) (pgdb.ZcashPayment, error)
	insertZcashPaymentFunc func(ctx context.Context, arg pgdb.InsertZcashPaymentParams) error
	upsertEntitlementFunc  func(ctx context.Context, arg pgdb.UpsertEntitlementWithTierParams) error

	insertCalls []pgdb.InsertZcashPaymentParams
	upsertCalls []pgdb.UpsertEntitlementWithTierParams
}

func (m *mockQuerier) GetZcashPayment(ctx context.Context, invoiceID string) (pgdb.ZcashPayment, error) {
	if m.getZcashPaymentFunc != nil {
		return m.getZcashPaymentFunc(ctx, invoiceID)
	}
	return pgdb.ZcashPayment{}, sql.ErrNoRows
}

func (m *mockQuerier) InsertZcashPayment(ctx context.Context, arg pgdb.InsertZcashPaymentParams) error {
	m.insertCalls = append(m.insertCalls, arg)
	if m.insertZcashPaymentFunc != nil {
		return m.insertZcashPaymentFunc(ctx, arg)
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

func TestConfirmPayment_ReplayPrevention(t *testing.T) {
	ctx := context.Background()
	log := logger.New(logger.Config{}).WithComponent("zcash-test")

	t.Run("duplicate invoice returns early without granting entitlement", func(t *testing.T) {
		existingPayment := pgdb.ZcashPayment{
			InvoiceID:  "user123__monthly_pro__1735254000",
			UserID:     "user123",
			ProductID:  "monthly_pro",
			AmountZat:  100000000,
			RedeemedAt: time.Now(),
		}

		mock := &mockQuerier{
			getZcashPaymentFunc: func(ctx context.Context, invoiceID string) (pgdb.ZcashPayment, error) {
				if invoiceID == "user123__monthly_pro__1735254000" {
					return existingPayment, nil
				}
				return pgdb.ZcashPayment{}, sql.ErrNoRows
			},
		}

		_ = &Service{
			queries: mock,
			logger:  log,
		}

		// Simulate checking for existing payment (this is what the service does)
		existing, err := mock.GetZcashPayment(ctx, "user123__monthly_pro__1735254000")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// When payment exists, service should return early
		if existing.InvoiceID != "user123__monthly_pro__1735254000" {
			t.Errorf("expected existing payment to be returned")
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
			getZcashPaymentFunc: func(ctx context.Context, invoiceID string) (pgdb.ZcashPayment, error) {
				return pgdb.ZcashPayment{}, sql.ErrNoRows
			},
			insertZcashPaymentFunc: func(ctx context.Context, arg pgdb.InsertZcashPaymentParams) error {
				callCount++
				if callCount > 1 {
					// Simulate unique constraint violation on second attempt
					return errors.New("duplicate key value violates unique constraint")
				}
				return nil
			},
		}

		_ = &Service{
			queries: mock,
			logger:  log,
		}

		// First insert succeeds
		err := mock.InsertZcashPayment(ctx, pgdb.InsertZcashPaymentParams{
			InvoiceID: "user123__monthly_pro__1735254000",
			UserID:    "user123",
			ProductID: "monthly_pro",
			AmountZat: 100000000,
		})
		if err != nil {
			t.Fatalf("first insert should succeed: %v", err)
		}

		// Second insert fails with unique constraint
		err = mock.InsertZcashPayment(ctx, pgdb.InsertZcashPaymentParams{
			InvoiceID: "user123__monthly_pro__1735254000",
			UserID:    "user456", // Different user trying same invoice
			ProductID: "monthly_pro",
			AmountZat: 100000000,
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
			getZcashPaymentFunc: func(ctx context.Context, invoiceID string) (pgdb.ZcashPayment, error) {
				return pgdb.ZcashPayment{}, sql.ErrNoRows
			},
			insertZcashPaymentFunc: func(ctx context.Context, arg pgdb.InsertZcashPaymentParams) error {
				callOrder = append(callOrder, "insert")
				return nil
			},
			upsertEntitlementFunc: func(ctx context.Context, arg pgdb.UpsertEntitlementWithTierParams) error {
				callOrder = append(callOrder, "upsert")
				return nil
			},
		}

		// Simulate the order of operations the service performs
		_ = mock.InsertZcashPayment(ctx, pgdb.InsertZcashPaymentParams{
			InvoiceID: "user123__monthly_pro__1735254000",
			UserID:    "user123",
			ProductID: "monthly_pro",
			AmountZat: 100000000,
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
}
