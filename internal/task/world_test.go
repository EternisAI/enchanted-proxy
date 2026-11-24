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
	inAdmission bool
	events      []string

	// Schedule state and scripted outcomes.
	missing     bool // no schedule exists
	deleteErr   error
	describeErr error // overrides the state above
}

func (w *taskWorld) log(event string) {
	if w.inAdmission {
		event = "@" + event
	}
	w.events = append(w.events, event)
}

func (w *taskWorld) GetTaskForUser(_ context.Context, arg pgdb.GetTaskForUserParams) (pgdb.Task, error) {
	row, ok := w.rows[arg.TaskID]
	if !ok || row.UserID != arg.UserID {
		return pgdb.Task{}, sql.ErrNoRows
	}
	return row, nil
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
	if w.describeErr != nil {
		return nil, w.describeErr
	}
	if w.missing {
		return nil, serviceerror.NewNotFound("no schedule")
	}
	return &client.ScheduleDescription{}, nil
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
			return fn(w)
		},
	}
}

func expectEvents(t *testing.T, w *taskWorld, want ...string) {
	t.Helper()
	if !slices.Equal(w.events, want) {
		t.Fatalf("events %q, want %q", w.events, want)
	}
}
