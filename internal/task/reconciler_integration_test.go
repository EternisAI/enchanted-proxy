//go:build integration

package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
)

// Instances that sweep at the same time make one sweep between them, and one
// that comes back within the interval finds the sweep recorded.
func TestReconcilerSweepsOncePerIntervalAcrossInstances(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.Exec(`DELETE FROM task_reconciler_runs`); err != nil {
		t.Fatal(err)
	}
	gate := lockedSweeps(db)
	var sweeps atomic.Int32
	sweep := func(ctx context.Context) *Report {
		sweeps.Add(1)
		time.Sleep(200 * time.Millisecond) // held while the other instances try
		return &Report{Mode: ReconcilerDryRun, Actions: map[string][]string{"complete": {"t1"}}}
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := gate(context.Background(), ReconcilerDryRun, time.Hour, sweep); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := sweeps.Load(); n != 1 {
		t.Fatalf("%d sweeps, want 1", n)
	}
	if ran, err := gate(context.Background(), ReconcilerDryRun, time.Hour, sweep); err != nil || ran {
		t.Fatalf("ran %v, err %v: a sweep within the interval must be skipped", ran, err)
	}
	if ran, err := gate(context.Background(), ReconcilerDryRun, 0, sweep); err != nil || !ran {
		t.Fatalf("ran %v, err %v: once the interval has passed, the next sweep runs", ran, err)
	}

	var mode, report string
	var took float64
	if err := db.QueryRow(`SELECT mode, report::text, EXTRACT(EPOCH FROM finished_at - started_at)::float8
		FROM task_reconciler_runs WHERE finished_at IS NOT NULL ORDER BY id LIMIT 1`).Scan(&mode, &report, &took); err != nil {
		t.Fatal(err)
	}
	var stored Report
	if err := json.Unmarshal([]byte(report), &stored); err != nil || mode != "dry-run" || len(stored.Actions["complete"]) != 1 {
		t.Fatalf("stored mode %q report %s (%v)", mode, report, err)
	}
	if took < 0.2 {
		t.Fatalf("the run took %.3fs by its record; the sweep slept 0.2s", took)
	}
}

// A reconciler being stopped does not wait for a database connection held by
// other work, and starts no sweep.
func TestReconcilerGateFollowsCancellationWhileWaitingForAConnection(t *testing.T) {
	openTestDB(t) // migrations
	small, err := sql.Open("postgres", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = small.Close() })
	small.SetMaxOpenConns(1)
	other, err := small.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The other work ends after a while, so a gate that waits for it returns
	// late rather than never.
	freed := time.AfterFunc(2*time.Second, func() { _ = other.Close() })
	t.Cleanup(func() { freed.Stop(); _ = other.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	swept := false
	started := time.Now()
	ran, err := lockedSweeps(small)(ctx, ReconcilerDryRun, 0, func(context.Context) *Report {
		swept = true
		return &Report{Mode: ReconcilerDryRun, Actions: map[string][]string{}}
	})
	if waited := time.Since(started); ran || swept || err == nil || waited > time.Second {
		t.Fatalf("ran %v, swept %v, err %v after %s; want no sweep, soon after the cancellation", ran, swept, err, waited)
	}
}

func TestReconcilerWritesAgainstPostgres(t *testing.T) {
	db := openTestDB(t)
	q := pgdb.New(db)
	ctx := context.Background()
	userID := "reconciler-test-" + uuid.NewString()
	newTask := func(kind string, status TaskStatus) string {
		id := uuid.NewString()
		if _, err := q.CreateTask(ctx, pgdb.CreateTaskParams{TaskID: id, UserID: userID, ChatID: "c", TaskName: "n", TaskText: "t",
			Type: kind, Time: "0 9 * * *", Status: string(status)}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	read := func(id string) pgdb.Task {
		row, err := q.GetTaskByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}

	recurring := newTask("recurring", TaskStatusActive)
	if n, err := q.CompleteOneTimeTask(ctx, recurring); err != nil || n != 0 {
		t.Fatalf("complete a recurring task: %d rows, %v; want none and no error", n, err)
	}
	oneTime := newTask("one_time", TaskStatusActive)
	if n, err := q.CompleteOneTimeTask(ctx, oneTime); err != nil || n != 1 || read(oneTime).Status != "completed" {
		t.Fatalf("complete: %d rows, %v", n, err)
	}

	pending := newTask("recurring", TaskStatusPending)
	if n, err := q.ActivatePendingTask(ctx, pending); err != nil || n != 1 || read(pending).Status != "active" {
		t.Fatalf("activate: %d rows, %v", n, err)
	}

	first := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, at := range []time.Time{first, first.Add(48 * time.Hour)} {
		if err := q.MarkTaskOverLimit(ctx, pgdb.MarkTaskOverLimitParams{TaskID: recurring, Since: sql.NullTime{Time: at, Valid: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(recurring).OverLimitSince; !got.Valid || !got.Time.Equal(first) {
		t.Fatalf("over since %v, want the first time kept", got)
	}
	if err := q.ClearTaskOverLimit(ctx, recurring); err != nil || read(recurring).OverLimitSince.Valid {
		t.Fatalf("clear: %v", err)
	}

	if _, err := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: recurring, PauseReason: reasonPtr(PauseUser)}); err != nil {
		t.Fatal(err)
	}
	if n, err := q.UpdateTaskPauseReason(ctx, pgdb.UpdateTaskPauseReasonParams{TaskID: recurring, PauseReason: reasonPtr(PauseAdmin)}); err != nil || n != 1 {
		t.Fatalf("update reason: %d rows, %v", n, err)
	}
	if got := read(recurring).PauseReason; got == nil || *got != string(PauseAdmin) {
		t.Fatalf("reason %v, want admin", got)
	}

	if n, err := q.DeleteTaskByID(ctx, recurring); err != nil || n != 1 {
		t.Fatalf("delete: %d rows, %v", n, err)
	}
	rows, err := q.ListTasksToReconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.TaskID == recurring || row.TaskID == oneTime {
			t.Fatalf("listed %s (%s), want deleted and completed tasks left out", row.TaskID, row.Status)
		}
	}
}
