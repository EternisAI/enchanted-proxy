//go:build integration

package iap

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
)

var reviewCutoff = time.Date(2026, 2, 16, 0, 0, 0, 0, time.UTC)

// reviewFixture empties the entitlements, the review tables and fills the
// entitlements with one row of each kind. The review reads the whole table,
// so it needs a table of its own.
func reviewFixture(t *testing.T, db *sql.DB) map[string]Account {
	t.Helper()
	for _, table := range []string{"entitlements", "entitlement_review_runs", "entitlements_review_backup"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	rows := []struct {
		id, tier, provider string
		purchase           *string
		expires            time.Time
	}{
		{"deleted", "plus", "apple", nil, lifetimeExpiry},
		{"later", "plus", "apple", nil, lifetimeExpiry},
		{"on-the-cutoff-day", "plus", "apple", nil, lifetimeExpiry},
		{"at-the-cutoff", "plus", "apple", nil, lifetimeExpiry},
		{"kept", "plus", "apple", nil, lifetimeExpiry},
		{"unsettled", "plus", "apple", nil, lifetimeExpiry},
		{"recorded", "plus", "apple", ptr("1000"), lifetimeExpiry},
		{"stripe", "plus", "stripe", nil, testNow.Add(24 * time.Hour)},
		{"pro", "pro", "apple", nil, testNow.Add(24 * time.Hour)},
		{"expired", "plus", "apple", nil, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO entitlements (user_id, subscription_tier, subscription_expires_at, subscription_provider, apple_original_transaction_id)
			VALUES ($1, $2, $3, $4, $5)`, r.id, r.tier, r.expires, r.provider, r.purchase); err != nil {
			t.Fatal(err)
		}
	}
	early := time.Date(2025, 12, 29, 10, 0, 0, 0, time.UTC)
	return map[string]Account{
		"deleted":           {Exists: false},
		"later":             {Exists: true, Created: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		"on-the-cutoff-day": {Exists: true, Created: time.Date(2026, 2, 15, 23, 0, 0, 0, time.UTC)},
		"at-the-cutoff":     {Exists: true, Created: reviewCutoff},
		"kept":              {Exists: true, Created: early},
		"recorded":          {Exists: true, Created: reviewCutoff.Add(time.Hour)},
		"stripe":            {Exists: true, Created: reviewCutoff.Add(time.Hour)},
		"pro":               {Exists: true, Created: reviewCutoff.Add(time.Hour)},
		"expired":           {Exists: true, Created: reviewCutoff.Add(time.Hour)},
	}
}

func ptr(s string) *string { return &s }

func reviewer(db *sql.DB, mode ReviewMode, accounts map[string]Account) *Reviewer {
	lookup := func(_ context.Context, ids []string) (map[string]Account, error) {
		out := map[string]Account{}
		for _, id := range ids {
			if a, ok := accounts[id]; ok {
				out[id] = a
			}
		}
		return out, nil
	}
	return NewReviewer(db, lookup, ReviewConfig{Mode: mode, Cutoff: reviewCutoff}, logger.New(logger.Config{Level: slog.LevelError + 4}))
}

func tiersByUser(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT user_id, subscription_tier FROM entitlements`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, tier string
		if err := rows.Scan(&id, &tier); err != nil {
			t.Fatal(err)
		}
		out[id] = tier
	}
	return out
}

// Accounts deleted or created after the cutoff lose an unrecorded Apple Plus
// plan; accounts created by the end of the cutoff day keep it; nothing else is
// touched. The dry run reports the same without changing anything.
func TestTheReviewResetsLateAndDeletedAccounts(t *testing.T) {
	db := openTestDB(t)
	accounts := reviewFixture(t, db)
	before := tiersByUser(t, db)

	rep, err := reviewer(db, ReviewDryRun, accounts).Run(context.Background())
	if err != nil || rep == nil {
		t.Fatalf("dry run: %+v, %v", rep, err)
	}
	if rep.Candidates != 6 || rep.Kept != 2 || !slices.Equal(rep.Deleted, []string{"deleted"}) ||
		!slices.Equal(rep.Later, []string{"at-the-cutoff", "later"}) || !slices.Equal(rep.Unsettled, []string{"unsettled"}) || len(rep.Errors) > 0 {
		t.Fatalf("dry run report %+v", rep)
	}
	if got := tiersByUser(t, db); !mapsEqual(got, before) {
		t.Fatalf("the dry run changed entitlements: %v", got)
	}

	rep, err = reviewer(db, ReviewEnforce, accounts).Run(context.Background())
	if err != nil || rep == nil || rep.Reset != 3 || rep.BackedUp != 10 || len(rep.Errors) > 0 {
		t.Fatalf("enforced: %+v, %v; want 3 reset and 10 rows backed up", rep, err)
	}
	want := map[string]string{}
	for id, tier := range before {
		want[id] = tier
	}
	for _, id := range []string{"deleted", "later", "at-the-cutoff"} {
		want[id] = "free"
	}
	if got := tiersByUser(t, db); !mapsEqual(got, want) {
		t.Fatalf("after enforcing: %v, want %v", got, want)
	}
	var backup int
	if err := db.QueryRow(`SELECT COUNT(*) FROM entitlements_review_backup WHERE subscription_tier = 'plus' AND user_id = 'later'`).Scan(&backup); err != nil || backup != 1 {
		t.Fatalf("backup of a reset row: %d, %v", backup, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT report FROM entitlement_review_runs WHERE mode = 'enforce'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var fromDB ReviewReport
	if err := json.Unmarshal([]byte(stored), &fromDB); err != nil || fromDB.Reset != 3 {
		t.Fatalf("stored report %s, %v", stored, err)
	}

	if rep, err := reviewer(db, ReviewEnforce, accounts).Run(context.Background()); err != nil || rep != nil {
		t.Fatalf("second enforced run: %+v, %v; want it skipped", rep, err)
	}
}

// A lookup that finds most accounts missing points at the wrong Firebase
// project: nothing is reset, and the review may run again once it is fixed.
func TestTheReviewResetsNothingWhenMostAccountsAreMissing(t *testing.T) {
	db := openTestDB(t)
	accounts := reviewFixture(t, db)
	before := tiersByUser(t, db)

	rep, err := reviewer(db, ReviewEnforce, map[string]Account{}).Run(context.Background())
	if err != nil || rep == nil || len(rep.Errors) == 0 || rep.Reset != 0 {
		t.Fatalf("%+v, %v; want an error and nothing reset", rep, err)
	}
	if got := tiersByUser(t, db); !mapsEqual(got, before) {
		t.Fatalf("entitlements changed: %v", got)
	}
	// Above, the lookup answered for no account; here it finds them all gone.
	missing := map[string]Account{}
	for _, id := range []string{"deleted", "later", "on-the-cutoff-day", "at-the-cutoff", "kept", "unsettled"} {
		missing[id] = Account{}
	}
	rep, err = reviewer(db, ReviewEnforce, missing).Run(context.Background())
	if err != nil || rep == nil || len(rep.Errors) == 0 || rep.Reset != 0 {
		t.Fatalf("all missing: %+v, %v; want the safeguard's error", rep, err)
	}
	// Most owners unresolved, but neither missing nor unsettled alone a majority.
	mixed := map[string]Account{"deleted": {}, "later": {}, "on-the-cutoff-day": accounts["on-the-cutoff-day"]}
	rep, err = reviewer(db, ReviewEnforce, mixed).Run(context.Background())
	if err != nil || rep == nil || len(rep.Errors) == 0 || rep.Reset != 0 {
		t.Fatalf("2 missing and 3 unsettled of 6: %+v, %v; want the safeguard's error", rep, err)
	}
	if got := tiersByUser(t, db); !mapsEqual(got, before) {
		t.Fatalf("entitlements changed: %v", got)
	}
}

// Only one server instance runs the review at a time.
func TestTheReviewRunsOnOneInstanceAtATime(t *testing.T) {
	db := openTestDB(t)
	accounts := reviewFixture(t, db)
	holder, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.Exec(`SELECT pg_advisory_xact_lock(hashtextextended('entitlement_review', 0))`); err != nil {
		t.Fatal(err)
	}
	if rep, err := reviewer(db, ReviewEnforce, accounts).Run(context.Background()); err != nil || rep != nil {
		t.Fatalf("%+v, %v; want it skipped while another instance holds the lock", rep, err)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
