//go:build integration

// The run-counting queries against Postgres. Requires a throwaway database:
//
//	docker run -d --rm --name ep-dr-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=eptest -p 55435:5432 postgres:16-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55435/eptest?sslmode=disable' \
//	    go test -tags integration ./internal/deepr/...
package deepr

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/storage/pg"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
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

// openTestDBInZone opens the test database with the session time zone set.
func openTestDBInZone(t *testing.T, zone string) *sql.DB {
	t.Helper()
	openTestDB(t) // skips without a database, and migrates
	u, err := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	params := u.Query()
	params.Set("timezone", zone)
	u.RawQuery = params.Encode()
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var got string
	if err := db.QueryRow(`SELECT current_setting('TimeZone')`).Scan(&got); err != nil || got != zone {
		t.Fatalf("session time zone = %q, %v; want %s", got, err, zone)
	}
	return db
}

// insertRun adds a run that started daysAgo days ago, lasting dur, in status.
func insertRun(t *testing.T, db *sql.DB, userID, status string, daysAgo int, dur time.Duration) {
	t.Helper()
	started := time.Now().UTC().AddDate(0, 0, -daysAgo).Add(-2 * time.Hour)
	var completed any
	if status != "active" {
		completed = started.Add(dur)
	}
	if _, err := db.Exec(`INSERT INTO deep_research_runs (user_id, chat_id, run_date, status, started_at, completed_at)
		VALUES ($1, $2, ((NOW() AT TIME ZONE 'UTC')::date - $3::int), $4, $5, $6)`,
		userID, uuid.NewString(), daysAgo, status, started, completed); err != nil {
		t.Fatal(err)
	}
}

func TestRunCountingQueries(t *testing.T) {
	db := openTestDB(t)
	q := pgdb.New(db)
	ctx := context.Background()
	a, b := "dr-a-"+uuid.NewString(), "dr-b-"+uuid.NewString()
	group := []string{a, b}

	insertRun(t, db, a, "completed", 0, 40*time.Minute)
	insertRun(t, db, a, "failed", 0, 30*time.Second) // refused or broken at once: not counted
	insertRun(t, db, b, "failed", 0, 5*time.Minute)  // spent work: counted, and b is in a's group
	insertRun(t, db, a, "completed", 8, time.Hour)   // a previous week
	insertRun(t, db, a, "completed", 400, time.Hour) // long ago

	count := func(name string, f func() (int64, error), want int64) {
		t.Helper()
		got, err := f()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	count("today", func() (int64, error) { return q.GetUserDeepResearchRunsToday(ctx, group) }, 2)
	count("today, a alone", func() (int64, error) { return q.GetUserDeepResearchRunsToday(ctx, []string{a}) }, 1)
	count("lifetime", func() (int64, error) { return q.GetUserDeepResearchRunsLifetime(ctx, group) }, 4)

	count("this week", func() (int64, error) { return q.GetUserDeepResearchRunsThisWeek(ctx, group) }, 2)

	before, err := q.CountDeepResearchRunsToday(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insertRun(t, db, "dr-c-"+uuid.NewString(), "active", 0, 0)
	count("service-wide today", func() (int64, error) { return q.CountDeepResearchRunsToday(ctx) }, before+1)

	if active, err := q.HasActiveDeepResearchRun(ctx, group); err != nil || active {
		t.Fatalf("active = %v, %v; want false", active, err)
	}
	tier := "plus"
	id, err := q.CreateDeepResearchRun(ctx, pgdb.CreateDeepResearchRunParams{UserID: b, ChatID: uuid.NewString(), Tier: &tier})
	if err != nil {
		t.Fatal(err)
	}
	if active, err := q.HasActiveDeepResearchRun(ctx, group); err != nil || !active {
		t.Fatalf("active = %v, %v; want true", active, err)
	}
	var stored sql.NullString
	if err := db.QueryRow(`SELECT tier FROM deep_research_runs WHERE id = $1`, id).Scan(&stored); err != nil || stored.String != "plus" {
		t.Fatalf("tier = %v, %v; want plus", stored, err)
	}

	// The reaper fails active runs that started before the cutoff, and only those.
	if _, err := db.Exec(`UPDATE deep_research_runs SET started_at = NOW() - INTERVAL '5 hours' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	n, err := q.FailStaleDeepResearchRuns(ctx, time.Now().Add(-4*time.Hour))
	if err != nil || n < 1 {
		t.Fatalf("reaped %d, %v; want at least 1", n, err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM deep_research_runs WHERE id = $1`, id).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("status = %q, %v; want failed", status, err)
	}
}

// soloGroup counts each account on its own.
type soloGroup struct{}

func (soloGroup) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID}, nil
}

// startInParallel starts n runs at once, the i-th for userOf(i), and returns
// how many were admitted.
func startInParallel(t *testing.T, s *Service, n int, tier tiers.Tier, userOf func(i int) string) int {
	t.Helper()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		admitted int
	)
	apple := &auth.Identity{SignInProvider: auth.ProviderApple}
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fe, err := s.startRun(context.Background(), userOf(i), uuid.NewString(), apple, tiers.Configs[tier])
			if err != nil {
				t.Errorf("start %d: %v", i, err)
				return
			}
			if fe == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return admitted
}

// Parallel starts are admitted one at a time, so they cannot all pass the same
// check: one account's free run, and the service-wide ceiling.
func TestParallelStartsAreAdmittedOneAtATime(t *testing.T) {
	db := openTestDB(t)
	q := pgdb.New(db)
	s := &Service{
		logger:                       testLogger(),
		queries:                      q,
		deepResearchRateLimitEnabled: true,
		cfg: &Config{
			Groups:           soloGroup{},
			Admission:        LockedAdmission(db),
			FreeRunProviders: []string{auth.ProviderApple},
		},
	}

	free := "dr-free-" + uuid.NewString()
	if got := startInParallel(t, s, 30, tiers.TierFree, func(int) string { return free }); got != 1 {
		t.Errorf("free account: %d of 30 parallel starts admitted, want 1", got)
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deep_research_runs WHERE user_id = $1`, free).Scan(&runs); err != nil || runs != 1 {
		t.Errorf("free account has %d runs, %v; want 1", runs, err)
	}

	before, err := q.CountDeepResearchRunsToday(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.MaxRunsPerDay = int(before) + 5
	prefix := "dr-pro-" + uuid.NewString() + "-"
	if got := startInParallel(t, s, 30, tiers.TierPro, func(i int) string { return prefix + string(rune('a'+i)) }); got != 5 {
		t.Errorf("30 accounts with room for 5 more runs: %d admitted, want 5", got)
	}
}

// Runs are dated and counted by the UTC date, whatever the session's time zone.
// At any time of day, one of these two zones has a date other than UTC's.
func TestRunDatesAreUTC(t *testing.T) {
	for _, zone := range []string{"Pacific/Kiritimati", "Pacific/Pago_Pago"} { // UTC+14, UTC-11
		t.Run(zone, func(t *testing.T) {
			db := openTestDBInZone(t, zone)
			q := pgdb.New(db)
			ctx := context.Background()
			user := "dr-utc-" + uuid.NewString()
			id, err := q.CreateDeepResearchRun(ctx, pgdb.CreateDeepResearchRunParams{UserID: user, ChatID: uuid.NewString()})
			if err != nil {
				t.Fatal(err)
			}
			var runDate string
			if err := db.QueryRow(`SELECT run_date::text FROM deep_research_runs WHERE id = $1`, id).Scan(&runDate); err != nil {
				t.Fatal(err)
			}
			if today := time.Now().UTC().Format("2006-01-02"); runDate != today {
				t.Errorf("run_date = %s, want the UTC date %s", runDate, today)
			}
			for name, count := range map[string]func(context.Context, []string) (int64, error){
				"today":     q.GetUserDeepResearchRunsToday,
				"this week": q.GetUserDeepResearchRunsThisWeek,
			} {
				if n, err := count(ctx, []string{user}); err != nil || n != 1 {
					t.Errorf("%s = %d, %v; want 1", name, n, err)
				}
			}
			if n, err := q.CountDeepResearchRunsToday(ctx); err != nil || n < 1 {
				t.Errorf("service-wide today = %d, %v; want at least 1", n, err)
			}
		})
	}
}
