//go:build integration

package quota

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	db, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := pg.RunMigrations(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func join(t *testing.T, db *sql.DB, a, b string) bool {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := Join(context.Background(), pgdb.New(tx), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return changed
}

// Joining groups gathers every member of both, and each member then sees the
// same group.
func TestJoinGathersBothGroups(t *testing.T) {
	db := openTestDB(t)
	id := func(name string) string { return name + "-" + uuid.NewString()[:8] }
	a, b, c, d, e := id("a"), id("b"), id("c"), id("d"), id("e")

	if !join(t, db, a, b) || !join(t, db, c, d) {
		t.Fatal("first joins reported no change")
	}
	if !join(t, db, b, d) {
		t.Fatal("joining two groups reported no change")
	}
	if join(t, db, c, a) {
		t.Fatal("joining two members of one group reported a change")
	}

	groups := NewGroups(pgdb.New(db))
	want := []string{a, b, c, d}
	slices.Sort(want)
	for _, member := range want {
		got, err := groups.UserIDs(context.Background(), member)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("group of %s = %v, %v; want %v", member, got, err, want)
		}
	}
	if got, _ := groups.UserIDs(context.Background(), e); !slices.Equal(got, []string{e}) {
		t.Fatalf("an account in no group = %v, want itself", got)
	}
}
