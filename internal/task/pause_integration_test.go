//go:build integration

package task

import (
	"context"
	"testing"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/google/uuid"
)

// The pause and resume writes are compare-and-set: a pause keeps the reason of
// a task already paused and leaves finished tasks alone, and a resume changes
// only a task still paused for the reason the caller checked.
func TestPauseAndResumeWritesAgainstPostgres(t *testing.T) {
	db := openTestDB(t)
	q := pgdb.New(db)
	ctx := context.Background()
	userID := "pause-test-" + uuid.NewString()
	newTask := func(status TaskStatus) string {
		id := uuid.NewString()
		if _, err := q.CreateTask(ctx, pgdb.CreateTaskParams{TaskID: id, UserID: userID, ChatID: "c", TaskName: "n", TaskText: "t",
			Type: string(TaskTypeRecurring), Time: "0 9 * * *", Status: string(status)}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	read := func(id string) pgdb.Task {
		row, err := q.GetTaskForUser(ctx, pgdb.GetTaskForUserParams{TaskID: id, UserID: userID})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	reason := func(row pgdb.Task) string {
		if row.PauseReason == nil {
			return "<nil>"
		}
		return *row.PauseReason
	}

	active := newTask(TaskStatusActive)
	if n, err := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: active, PauseReason: reasonPtr(PauseInactive)}); err != nil || n != 1 {
		t.Fatalf("pause: %d rows, %v", n, err)
	}
	if n, _ := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: active, PauseReason: reasonPtr(PauseUser)}); n != 0 {
		t.Fatal("a second pause replaced the first one's reason")
	}
	if row := read(active); row.Status != string(TaskStatusPaused) || reason(row) != string(PauseInactive) {
		t.Fatalf("row %s/%s, want paused/inactive", row.Status, reason(row))
	}
	if n, _ := q.MarkTaskResumed(ctx, pgdb.MarkTaskResumedParams{TaskID: active, PauseReason: reasonPtr(PauseUser)}); n != 0 {
		t.Fatal("resumed a task paused for another reason than the one checked")
	}
	if n, err := q.MarkTaskResumed(ctx, pgdb.MarkTaskResumedParams{TaskID: active, PauseReason: reasonPtr(PauseInactive)}); err != nil || n != 1 {
		t.Fatalf("resume: %d rows, %v", n, err)
	}
	if row := read(active); row.Status != string(TaskStatusActive) || row.PauseReason != nil {
		t.Fatalf("row %s/%s, want active with no reason", row.Status, reason(row))
	}

	completed := newTask(TaskStatusCompleted)
	if n, _ := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: completed, PauseReason: reasonPtr(PauseUser)}); n != 0 {
		t.Fatal("paused a finished task")
	}

	// A paused task does not count toward the limits on open tasks.
	open, err := q.ListOpenTasksForUsers(ctx, []string{userID})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].TaskID != active {
		t.Fatalf("open tasks %+v, want only the resumed one", open)
	}
}
