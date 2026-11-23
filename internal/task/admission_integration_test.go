//go:build integration

// Task admission against Postgres. Requires a throwaway database:
//
//	docker run -d --rm --name ep-task-test -e POSTGRES_PASSWORD=test \
//	    -e POSTGRES_DB=eptest -p 55436:5432 postgres:16-alpine
//	TEST_DATABASE_URL='postgres://postgres:test@localhost:55436/eptest?sslmode=disable' \
//	    go test -tags integration ./internal/task/...
package task

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/logger"
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

type soloGroup struct{}

func (soloGroup) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID}, nil
}

// A burst of parallel requests must not get past the limits: each admission
// sees the tasks admitted before it.
func TestConcurrentAdmissionsKeepToTheLimit(t *testing.T) {
	db := openTestDB(t)
	s := &Service{
		logger:     logger.New(logger.Config{Level: slog.LevelError + 4}),
		queries:    pgdb.New(db),
		tierLookup: fixedTier{tier: tiers.TierFree},
		groups:     soloGroup{},
		admission:  lockedAdmission(db),
	}
	user := "adm-" + uuid.NewString()
	limit := tiers.Configs[tiers.TierFree].TaskMaxActiveRecurring

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		admitted int
	)
	for i := 0; i < 3*limit; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := request("recurring", fmt.Sprintf("%d 9 * * *", i))
			if _, err := s.admitTask(context.Background(), user, uuid.NewString(), req, now); err == nil {
				mu.Lock()
				admitted++
				mu.Unlock()
			} else if limitReason(err) == "" {
				t.Errorf("admission %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if admitted != limit {
		t.Fatalf("admitted %d recurring tasks in parallel, want the limit of %d", admitted, limit)
	}
	var tasks, creations int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE user_id = $1`, user).Scan(&tasks); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_creations WHERE user_id = $1`, user).Scan(&creations); err != nil {
		t.Fatal(err)
	}
	if tasks != limit || creations != limit {
		t.Fatalf("%d task rows and %d creations, want %d of each", tasks, creations, limit)
	}
}

// A released task no longer counts toward the daily creation limit.
func TestReleasedTaskFreesItsCreation(t *testing.T) {
	db := openTestDB(t)
	s := &Service{
		logger:     logger.New(logger.Config{Level: slog.LevelError + 4}),
		queries:    pgdb.New(db),
		tierLookup: fixedTier{tier: tiers.TierFree},
		groups:     soloGroup{},
		admission:  lockedAdmission(db),
	}
	user := "rel-" + uuid.NewString()
	taskID := uuid.NewString()
	if _, err := s.admitTask(context.Background(), user, taskID, request("recurring", "0 9 * * *"), now); err != nil {
		t.Fatal(err)
	}
	s.releaseTask(context.Background(), taskID, user)
	var rows int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM tasks WHERE user_id = $1) + (SELECT COUNT(*) FROM task_creations WHERE user_id = $1)`, user).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("%d rows left after release, want none", rows)
	}
}
