//go:build integration

package task

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/google/uuid"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
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
	paused := newTask(TaskStatusActive)
	if n, err := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: paused, PauseReason: reasonPtr(PauseUser)}); err != nil || n != 1 {
		t.Fatalf("pause: %d rows, %v", n, err)
	}
	open, err := q.ListOpenTasksForUsers(ctx, []string{userID})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].TaskID != active {
		t.Fatalf("open tasks %+v, want only the resumed one, not the paused or completed one", open)
	}
}

// gatedSchedules is a Temporal schedule store, safe for concurrent use, whose
// Create waits until the test releases it.
type gatedSchedules struct {
	client.Client
	mu       sync.Mutex
	created  map[string]bool
	entered  chan struct{} // closed when Create is called
	release  chan struct{} // Create returns once this is closed
	announce sync.Once
}

func (g *gatedSchedules) ScheduleClient() client.ScheduleClient { return gatedClient{g: g} }

type gatedClient struct {
	client.ScheduleClient
	g *gatedSchedules
}

func (c gatedClient) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	c.g.announce.Do(func() { close(c.g.entered) })
	<-c.g.release
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	c.g.created[o.ID] = true
	return gatedHandle{g: c.g, id: o.ID}, nil
}

func (c gatedClient) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return gatedHandle{g: c.g, id: id}
}

type gatedHandle struct {
	client.ScheduleHandle
	g  *gatedSchedules
	id string
}

func (h gatedHandle) GetID() string { return h.id }

func (h gatedHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	if !h.g.created[h.id] {
		return nil, serviceerror.NewNotFound("no schedule")
	}
	return &client.ScheduleDescription{}, nil
}

func (h gatedHandle) Delete(context.Context) error {
	h.g.mu.Lock()
	defer h.g.mu.Unlock()
	if !h.g.created[h.id] {
		return serviceerror.NewNotFound("no schedule")
	}
	delete(h.g.created, h.id)
	return nil
}

// An owner who lists a task while it is being created and deletes it must
// not leave its schedule running without a row: the deletion waits for the
// creation, under the task's lock.
func TestDeleteDuringCreateLeavesNoScheduleWithoutARow(t *testing.T) {
	db := openTestDB(t)
	gate := &gatedSchedules{created: map[string]bool{}, entered: make(chan struct{}), release: make(chan struct{})}
	s := &Service{
		temporalClient: gate,
		logger:         logger.New(logger.Config{Level: slog.LevelError + 4}),
		queries:        pgdb.New(db),
		tierLookup:     fixedTier{tier: tiers.TierPro},
		groups:         soloGroup{},
		admission:      lockedAdmission(db),
		taskLock:       lockedTasks(db, logger.New(logger.Config{Level: slog.LevelError + 4})),
		clock:          time.Now,
	}
	ctx := context.Background()
	userID := "delete-during-create-" + uuid.NewString()

	created := make(chan error, 1)
	go func() {
		_, err := s.CreateTask(ctx, userID, request("recurring", "0 9 * * *"))
		created <- err
	}()
	<-gate.entered

	tasks, err := s.GetTasksByUserID(ctx, userID)
	if err != nil || len(tasks) != 1 || tasks[0].Status != string(TaskStatusPending) {
		t.Fatalf("tasks %+v (%v), want the one pending task", tasks, err)
	}
	taskID := tasks[0].TaskID
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteTask(ctx, userID, taskID) }()
	var deleteErr error
	deleteDone := false
	select {
	case deleteErr = <-deleted:
		deleteDone = true
		t.Errorf("the deletion finished while the creation held the task (err %v)", deleteErr)
	case <-time.After(300 * time.Millisecond):
	}
	close(gate.release)
	if err := <-created; err != nil {
		t.Logf("create: %v", err)
	}
	if !deleteDone {
		deleteErr = <-deleted
	}
	if deleteErr != nil && !errors.Is(deleteErr, ErrTaskNotFound) {
		t.Fatalf("delete: %v", deleteErr)
	}

	_, rowErr := pgdb.New(db).GetTaskByID(ctx, taskID)
	gate.mu.Lock()
	scheduled := gate.created[taskID]
	gate.mu.Unlock()
	if scheduled && errors.Is(rowErr, sql.ErrNoRows) {
		t.Fatal("the schedule runs with no task row to count it")
	}
	if scheduled || rowErr == nil {
		t.Fatalf("schedule %v, row error %v; want both deleted", scheduled, rowErr)
	}
}

// Holders of task locks each keep a pool connection and need another for their
// work. Many at once on a small pool, all on one task, must still finish
// rather than wait for one another until their deadlines.
func TestTaskLocksDoNotExhaustTheConnectionPool(t *testing.T) {
	openTestDB(t) // migrations
	small, err := sql.Open("postgres", os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = small.Close() })
	small.SetMaxOpenConns(3)
	lock := lockedTasks(small, logger.New(logger.Config{Level: slog.LevelError + 4}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := make(chan error, 10)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- lock(ctx, "pool-test-task", func() error {
				var one int
				return small.QueryRowContext(ctx, "SELECT 1").Scan(&one)
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a lock holder could not get a connection: %v", err)
		}
	}
}
