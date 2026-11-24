package task

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

// taskWorld holds one owner's task rows and their schedules, and logs every
// write to either in order. "@" marks a write made under the admission lock.
type taskWorld struct {
	pgdb.Querier // any other query panics
	client.Client

	rows        map[string]pgdb.Task
	open        []pgdb.ListOpenTasksForUsersRow // other open tasks of the group
	inAdmission bool
	events      []string

	// Schedule state and scripted outcomes.
	missing                         bool // no schedule exists
	paused                          bool
	note                            string
	pauseErr, unpauseErr, deleteErr error
	describeErr                     error   // overrides the state above
	describeErrs                    []error // one per Describe call, in order, before describeErr
	unpauseApplies                  bool    // Unpause resumes the schedule even when it returns unpauseErr

	duringAdmission func() // runs inside the admission lock
	locked          bool   // inside the task's lock
	outsideLock     []string
}

func (w *taskWorld) log(event string) {
	if !w.locked {
		w.outsideLock = append(w.outsideLock, event)
	}
	if w.inAdmission {
		event = "@" + event
	}
	w.events = append(w.events, event)
}

func (w *taskWorld) GetTaskForUser(_ context.Context, arg pgdb.GetTaskForUserParams) (pgdb.Task, error) {
	if !w.locked {
		w.outsideLock = append(w.outsideLock, "read row")
	}
	row, ok := w.rows[arg.TaskID]
	if !ok || row.UserID != arg.UserID {
		return pgdb.Task{}, sql.ErrNoRows
	}
	return row, nil
}

func (w *taskWorld) MarkTaskPaused(_ context.Context, arg pgdb.MarkTaskPausedParams) (int64, error) {
	w.log("row paused:" + *arg.PauseReason)
	row := w.rows[arg.TaskID]
	if row.Status != string(TaskStatusActive) && row.Status != string(TaskStatusPending) {
		return 0, nil
	}
	row.Status, row.PauseReason = string(TaskStatusPaused), arg.PauseReason
	w.rows[arg.TaskID] = row
	return 1, nil
}

func (w *taskWorld) MarkTaskResumed(_ context.Context, arg pgdb.MarkTaskResumedParams) (int64, error) {
	w.log("row resumed")
	row := w.rows[arg.TaskID]
	if row.Status != string(TaskStatusPaused) || (row.PauseReason == nil) != (arg.PauseReason == nil) ||
		(row.PauseReason != nil && *row.PauseReason != *arg.PauseReason) {
		return 0, nil
	}
	row.Status, row.PauseReason = string(TaskStatusActive), nil
	w.rows[arg.TaskID] = row
	return 1, nil
}

func (w *taskWorld) ListOpenTasksForUsers(context.Context, []string) ([]pgdb.ListOpenTasksForUsersRow, error) {
	return w.open, nil
}

func (w *taskWorld) DeleteTask(_ context.Context, arg pgdb.DeleteTaskParams) (sql.Result, error) {
	w.log("row deleted")
	delete(w.rows, arg.TaskID)
	return driver.RowsAffected(1), nil
}

func (w *taskWorld) ScheduleClient() client.ScheduleClient { return taskSchedules{world: w} }

type taskSchedules struct {
	client.ScheduleClient
	world *taskWorld
}

func (s taskSchedules) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return &taskHandle{world: s.world, id: id}
}

type taskHandle struct {
	client.ScheduleHandle
	world *taskWorld
	id    string
}

func (h *taskHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	w := h.world
	if len(w.describeErrs) > 0 {
		err := w.describeErrs[0]
		w.describeErrs = w.describeErrs[1:]
		if err != nil {
			return nil, err
		}
	} else if w.describeErr != nil {
		return nil, w.describeErr
	}
	if w.missing {
		return nil, serviceerror.NewNotFound("no schedule")
	}
	return &client.ScheduleDescription{Schedule: client.Schedule{State: &client.ScheduleState{Paused: w.paused, Note: w.note}}}, nil
}

func (h *taskHandle) Pause(_ context.Context, o client.SchedulePauseOptions) error {
	w := h.world
	if w.pauseErr != nil {
		return w.pauseErr
	}
	w.log("schedule paused:" + o.Note)
	w.paused, w.note = true, o.Note
	return nil
}

func (h *taskHandle) Unpause(context.Context, client.ScheduleUnpauseOptions) error {
	w := h.world
	if w.unpauseErr != nil {
		if w.unpauseApplies {
			w.paused = false
		}
		return w.unpauseErr
	}
	w.log("schedule resumed")
	w.paused = false
	return nil
}

func (h *taskHandle) Delete(context.Context) error {
	w := h.world
	if w.deleteErr != nil {
		return w.deleteErr
	}
	if w.missing {
		return serviceerror.NewNotFound("no schedule")
	}
	w.log("schedule deleted")
	w.missing = true
	return nil
}

// newTaskWorld has one active recurring task of user-1's, task-1, every five
// minutes, created two days before now.
func newTaskWorld() *taskWorld {
	return &taskWorld{rows: map[string]pgdb.Task{"task-1": {TaskID: "task-1", UserID: "user-1", ChatID: "chat-1",
		Type: string(TaskTypeRecurring), Time: "*/5 * * * *", Status: string(TaskStatusActive), CreatedAt: now.Add(-48 * time.Hour)}}}
}

func (w *taskWorld) service(tier tiers.Tier) *Service {
	return &Service{
		temporalClient: w,
		logger:         logger.New(logger.Config{Level: slog.LevelError + 4}),
		queries:        w,
		tierLookup:     fixedTier{tier: tier},
		groups:         twoMemberGroup{},
		clock:          func() time.Time { return now },
		admission: func(_ context.Context, _ []string, fn func(pgdb.Querier) error) error {
			w.inAdmission = true
			defer func() { w.inAdmission = false }()
			if w.duringAdmission != nil {
				w.duringAdmission()
			}
			return fn(w)
		},
		taskLock: func(_ context.Context, _ string, fn func() error) error {
			w.locked = true
			defer func() { w.locked = false }()
			return fn()
		},
	}
}

func expectEvents(t *testing.T, w *taskWorld, want ...string) {
	t.Helper()
	if !slices.Equal(w.events, want) {
		t.Fatalf("events %q, want %q", w.events, want)
	}
}
