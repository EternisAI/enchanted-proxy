//go:build integration

// Regression coverage for FAI payment settlement. Requires a throwaway Postgres:
//
//	docker run -d --rm --name ep-fai-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=eptest -p 55434:5432 postgres:16-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55434/eptest?sslmode=disable' \
//	    go test -tags integration -race ./internal/fai/...
package fai

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
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

	// GetProducts reads the debug multiplier from the app config.
	if config.AppConfig == nil {
		config.AppConfig = &config.Config{}
	}

	return NewService(db, pgdb.New(db), logger.New(logger.Config{})), db
}

// seedIntent inserts a pending weekly-pro intent and returns it as the event handler reads it.
func seedIntent(t *testing.T, db *sql.DB, userID string) pgdb.FaiPaymentIntent {
	t.Helper()
	paymentID := uuid.NewString()
	if _, err := db.Exec(`
		INSERT INTO fai_payment_intents (id, user_id, payment_id, product_id, price_usd, fai_price, status)
		VALUES ($1, $2, $3, $4, 4.99, 0.01, 'pending')`,
		uuid.NewString(), userID, paymentID, ProductWeeklyPro,
	); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	intent, err := pgdb.New(db).GetFaiPaymentIntentByPaymentID(context.Background(), paymentID)
	if err != nil {
		t.Fatalf("read intent: %v", err)
	}
	return intent
}

func claimParams(intent pgdb.FaiPaymentIntent) pgdb.UpdateFaiPaymentIntentToCompletedParams {
	token, tx := "0xtoken", "0xtx"
	return pgdb.UpdateFaiPaymentIntentToCompletedParams{
		PaymentID:    intent.PaymentID,
		TokenAddress: &token,
		TokenAmount:  sql.NullFloat64{Float64: 499, Valid: true},
		PaidBlock:    sql.NullInt64{Int64: 1, Valid: true},
		TxHash:       &tx,
	}
}

func readExpiry(t *testing.T, db *sql.DB, userID string) (time.Time, bool) {
	t.Helper()
	var expires time.Time
	err := db.QueryRow(`SELECT subscription_expires_at FROM entitlements WHERE user_id = $1`, userID).Scan(&expires)
	if err == sql.ErrNoRows {
		return time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read entitlement: %v", err)
	}
	return expires, true
}

func readStatus(t *testing.T, db *sql.DB, paymentID string) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM fai_payment_intents WHERE payment_id = $1`, paymentID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

// The same event reaches every replica and the historical sync. Three deliveries racing
// for one intent must grant one week, not two or three.
func TestSettlePaymentConcurrentDeliveriesGrantOnce(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "fai-concurrent-" + uuid.NewString()
	intent := seedIntent(t, db, userID)
	product := svc.GetProduct(ProductWeeklyPro)

	const deliveries = 3
	var wg sync.WaitGroup
	claimed := make([]bool, deliveries)
	errs := make([]error, deliveries)
	start := make(chan struct{})
	before := time.Now()
	for i := range deliveries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed[i], errs[i] = svc.settlePayment(ctx, intent, product, claimParams(intent))
		}()
	}
	close(start)
	wg.Wait()
	after := time.Now()

	grants := 0
	for i := range deliveries {
		if errs[i] != nil {
			t.Fatalf("delivery %d: %v", i, errs[i])
		}
		if claimed[i] {
			grants++
		}
	}
	if grants != 1 {
		t.Fatalf("grants = %d, want 1", grants)
	}

	got, ok := readExpiry(t, db, userID)
	if !ok {
		t.Fatal("no entitlement written")
	}
	week := 7 * 24 * time.Hour
	if got.Before(before.Add(week).Add(-time.Second)) || got.After(after.Add(week).Add(time.Second)) {
		t.Fatalf("expiry = %v, want one week from settlement (%v to %v)", got, before.Add(week), after.Add(week))
	}
	if s := readStatus(t, db, intent.PaymentID); s != "completed" {
		t.Fatalf("status = %q, want completed", s)
	}

	// A later redelivery changes nothing.
	again, err := svc.settlePayment(ctx, intent, product, claimParams(intent))
	if err != nil || again {
		t.Fatalf("redelivery: claimed=%v err=%v, want false, nil", again, err)
	}
	if later, _ := readExpiry(t, db, userID); !later.Equal(got) {
		t.Fatalf("expiry moved on redelivery: %v -> %v", got, later)
	}
}

// When the grant fails, the claim is rolled back so a later delivery can settle the intent.
func TestSettlePaymentReleasesClaimWhenGrantFails(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	userID := "fai-grantfail-" + uuid.NewString()
	intent := seedIntent(t, db, userID)
	product := svc.GetProduct(ProductWeeklyPro)

	// Make the entitlement write fail for this user only.
	if _, err := db.Exec(`
		CREATE OR REPLACE FUNCTION fai_test_fail_grant() RETURNS trigger AS $$
		BEGIN
			IF NEW.user_id LIKE 'fai-grantfail-%' THEN RAISE EXCEPTION 'grant refused by test'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		DROP TRIGGER IF EXISTS fai_test_fail_grant ON entitlements;
		CREATE TRIGGER fai_test_fail_grant BEFORE INSERT OR UPDATE ON entitlements
			FOR EACH ROW EXECUTE FUNCTION fai_test_fail_grant();`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DROP TRIGGER IF EXISTS fai_test_fail_grant ON entitlements; DROP FUNCTION IF EXISTS fai_test_fail_grant();`)
	})

	if _, err := svc.settlePayment(ctx, intent, product, claimParams(intent)); err == nil {
		t.Fatal("expected settlement to fail")
	}
	if s := readStatus(t, db, intent.PaymentID); s != "pending" {
		t.Fatalf("status after failed grant = %q, want pending", s)
	}
	if _, ok := readExpiry(t, db, userID); ok {
		t.Fatal("entitlement written despite failed grant")
	}

	if _, err := db.Exec(`DROP TRIGGER fai_test_fail_grant ON entitlements`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	claimed, err := svc.settlePayment(ctx, intent, product, claimParams(intent))
	if err != nil || !claimed {
		t.Fatalf("retry: claimed=%v err=%v, want true, nil", claimed, err)
	}
}
