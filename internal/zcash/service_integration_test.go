//go:build integration

// Regression coverage for payment settlement. Requires a throwaway Postgres:
//
//	docker run -d --rm --name ep-zcash-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=eptest -p 55433:5432 postgres:16-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55433/eptest?sslmode=disable' \
//	    go test -tags integration -race ./internal/zcash/...
package zcash

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	"github.com/lib/pq" // registers the driver here rather than relying on pg to do it
)

func newTestService(t *testing.T) (*Service, *sql.DB) {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := pg.RunMigrations(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// GetProducts reads the debug multiplier; NewService reads the TLS flag.
	if config.AppConfig == nil {
		config.AppConfig = &config.Config{}
	}

	return NewService(db, pgdb.New(db), nil, logger.New(logger.Config{})), db
}

// seedInvoice inserts a monthly-pro invoice created `age` ago in the given status.
func seedInvoice(t *testing.T, db *sql.DB, userID, status string, age time.Duration) (uuid.UUID, time.Time) {
	t.Helper()

	id := uuid.New()
	var createdAt time.Time
	err := db.QueryRow(`
		INSERT INTO zcash_invoices (
			id, user_id, product_id, amount_zatoshis, zec_amount,
			price_usd, receiving_address, status, created_at, updated_at
		) VALUES ($1, $2, $3, 4210372, 0.0421, 19.99, 'u1testaddress', $4, NOW() - $5::interval, NOW())
		RETURNING created_at`,
		id, userID, ProductMonthlyPro, status, age.String(),
	).Scan(&createdAt)
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	return id, createdAt
}

func readInvoice(t *testing.T, db *sql.DB, id uuid.UUID) (status string, paidAt *time.Time) {
	t.Helper()
	if err := db.QueryRow(`SELECT status, paid_at FROM zcash_invoices WHERE id = $1`, id).
		Scan(&status, &paidAt); err != nil {
		t.Fatalf("read invoice: %v", err)
	}
	return status, paidAt
}

func readExpiry(t *testing.T, db *sql.DB, userID string) time.Time {
	t.Helper()
	var expires time.Time
	if err := db.QueryRow(`SELECT subscription_expires_at FROM entitlements WHERE user_id = $1`, userID).
		Scan(&expires); err != nil {
		t.Fatalf("read entitlement: %v", err)
	}
	return expires
}

// An invoice the expiry worker already retired still has to settle, and settle once.
// Before the fix the status update matched no rows, so the row stayed "expired", the
// caller's already-paid short circuit never engaged, and every redelivered callback
// granted another 30 days.
func TestSettlePaidInvoiceExpiredGrantsExactlyOnce(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "user-expired-" + uuid.NewString()

	id, createdAt := seedInvoice(t, db, userID, "expired", 48*time.Hour)

	if err := svc.settlePaidInvoice(ctx, id); err != nil {
		t.Fatalf("first settle: %v", err)
	}

	status, paidAt := readInvoice(t, db, id)
	if status != "paid" {
		t.Fatalf("status after settle = %q, want \"paid\"", status)
	}
	if paidAt == nil {
		t.Fatal("paid_at not set")
	}

	want := createdAt.Add(30 * 24 * time.Hour)
	if got := readExpiry(t, db, userID); !got.Equal(want) {
		t.Fatalf("expiry after settle = %v, want %v", got, want)
	}

	// Redelivery: at-least-once callbacks make this the normal case, not an edge case.
	for i := range 3 {
		if err := svc.settlePaidInvoice(ctx, id); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}

	if got := readExpiry(t, db, userID); !got.Equal(want) {
		t.Fatalf("expiry after 3 redeliveries = %v, want %v (double-granted by %v)",
			got, want, got.Sub(want))
	}
}

// Two deliveries racing past the caller's status read must not both grant. The row
// lock in settlePaidInvoice is what serializes them.
func TestSettlePaidInvoiceConcurrentDeliveriesGrantOnce(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "user-concurrent-" + uuid.NewString()

	id, createdAt := seedInvoice(t, db, userID, "processing", time.Hour)

	const deliveries = 4
	var wg sync.WaitGroup
	errs := make([]error, deliveries)
	start := make(chan struct{})

	for i := range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = svc.settlePaidInvoice(ctx, id)
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}

	want := createdAt.Add(30 * 24 * time.Hour)
	if got := readExpiry(t, db, userID); !got.Equal(want) {
		t.Fatalf("expiry after %d concurrent deliveries = %v, want %v (over-granted by %v)",
			deliveries, got, want, got.Sub(want))
	}
}

func countEntitlements(t *testing.T, db *sql.DB, userID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM entitlements WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count entitlements: %v", err)
	}
	return n
}

// A grant that fails must not leave the invoice marked paid, so the next delivery
// retries it. An unknown product fails grantEntitlement before it issues any
// statement, so this covers the ordering only - see the test below for atomicity.
func TestSettlePaidInvoiceLeavesInvoiceUnpaidWhenGrantFails(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "user-grantfail-" + uuid.NewString()

	id, _ := seedInvoice(t, db, userID, "processing", time.Hour)
	if _, err := db.Exec(`UPDATE zcash_invoices SET product_id = 'silo.nonexistent' WHERE id = $1`, id); err != nil {
		t.Fatalf("set bogus product: %v", err)
	}

	if err := svc.settlePaidInvoice(ctx, id); err == nil {
		t.Fatal("expected settle to fail for unknown product")
	}

	if status, paidAt := readInvoice(t, db, id); status != "processing" || paidAt != nil {
		t.Fatalf("invoice = (%q, %v) after failed settle, want (\"processing\", <nil>)", status, paidAt)
	}
	if n := countEntitlements(t, db, userID); n != 0 {
		t.Fatalf("entitlement rows = %d after failed settle, want 0", n)
	}
}

// The atomicity guarantee: an entitlement upsert that has already succeeded must be
// undone when a later statement in the settlement fails. Without the transaction the
// user would keep a subscription period bought by an invoice that never settled.
//
// A trigger scoped to this user's invoice makes the status update fail after the
// upsert has landed - the one ordering that cannot be produced from the Go side.
func TestSettlePaidInvoiceRollsBackGrantWhenStatusUpdateFails(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "user-rollback-" + uuid.NewString()

	id, _ := seedInvoice(t, db, userID, "processing", time.Hour)

	if _, err := db.Exec(`
		CREATE OR REPLACE FUNCTION test_fail_paid_update() RETURNS trigger AS $$
		BEGIN RAISE EXCEPTION 'forced failure after grant'; END;
		$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	// DDL takes no bind parameters, so the user id is quoted into the WHEN clause.
	if _, err := db.Exec(fmt.Sprintf(`
		CREATE TRIGGER test_fail_paid_update BEFORE UPDATE ON zcash_invoices
		FOR EACH ROW WHEN (NEW.user_id = %s) EXECUTE FUNCTION test_fail_paid_update()`,
		pq.QuoteLiteral(userID))); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS test_fail_paid_update ON zcash_invoices`)
		_, _ = db.Exec(`DROP FUNCTION IF EXISTS test_fail_paid_update()`)
	})

	if err := svc.settlePaidInvoice(ctx, id); err == nil {
		t.Fatal("expected settle to fail when the status update fails")
	}

	// grantEntitlement ran and its upsert succeeded; only the rollback removes it.
	if n := countEntitlements(t, db, userID); n != 0 {
		t.Fatalf("entitlement rows = %d after rollback, want 0 (grant was not undone)", n)
	}
	if status, paidAt := readInvoice(t, db, id); status != "processing" || paidAt != nil {
		t.Fatalf("invoice = (%q, %v) after rollback, want (\"processing\", <nil>)", status, paidAt)
	}
}
