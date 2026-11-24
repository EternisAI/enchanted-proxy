package task

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	schedulepb "go.temporal.io/api/schedule/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/grpc"
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

	// Schedule state and scripted outcomes. As in Temporal, every change to the
	// schedule bumps its conflict token, and a resume sent with an out-of-date
	// token is dropped.
	missing                        bool // no schedule exists
	paused                         bool
	note                           string
	token                          int64
	pauseErr, resumeErr, deleteErr error
	describeErr                    error   // overrides the state above
	describeErrs                   []error // one per Describe call, in order, before describeErr
	resumeApplies                  bool    // a resume applies even when it returns resumeErr
	resumeLate                     bool    // a resume returns resumeErr and reaches the schedule later

	pendingResume *workflowservice.UpdateScheduleRequest // a late resume yet to arrive
	beforePause   func()                                 // runs when a pause is sent, before it applies

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

// describeOutcome is the scripted error of the next Describe, if any.
func (w *taskWorld) describeOutcome() error {
	if len(w.describeErrs) > 0 {
		err := w.describeErrs[0]
		w.describeErrs = w.describeErrs[1:]
		return err
	}
	if w.describeErr != nil {
		return w.describeErr
	}
	if w.missing {
		return serviceerror.NewNotFound("no schedule")
	}
	return nil
}

func (h *taskHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	w := h.world
	if err := w.describeOutcome(); err != nil {
		return nil, err
	}
	return &client.ScheduleDescription{Schedule: client.Schedule{State: &client.ScheduleState{Paused: w.paused, Note: w.note}}}, nil
}

func (h *taskHandle) Pause(_ context.Context, o client.SchedulePauseOptions) error {
	w := h.world
	if w.pauseErr != nil {
		return w.pauseErr
	}
	if w.beforePause != nil {
		w.beforePause()
	}
	w.log("schedule paused:" + o.Note)
	w.paused, w.note = true, o.Note
	w.token++
	return nil
}

func (w *taskWorld) WorkflowService() workflowservice.WorkflowServiceClient {
	return taskService{world: w}
}

// taskService serves the schedule calls a resume makes directly.
type taskService struct {
	workflowservice.WorkflowServiceClient
	world *taskWorld
}

func (w *taskWorld) tokenBytes() []byte { return binary.BigEndian.AppendUint64(nil, uint64(w.token)) }

func (s taskService) DescribeSchedule(context.Context, *workflowservice.DescribeScheduleRequest, ...grpc.CallOption) (*workflowservice.DescribeScheduleResponse, error) {
	w := s.world
	if err := w.describeOutcome(); err != nil {
		return nil, err
	}
	return &workflowservice.DescribeScheduleResponse{
		Schedule:      &schedulepb.Schedule{State: &schedulepb.ScheduleState{Paused: w.paused, Notes: w.note}},
		ConflictToken: w.tokenBytes(),
	}, nil
}

func (s taskService) UpdateSchedule(_ context.Context, req *workflowservice.UpdateScheduleRequest, _ ...grpc.CallOption) (*workflowservice.UpdateScheduleResponse, error) {
	w := s.world
	if w.resumeErr != nil {
		if w.resumeApplies {
			w.applyUpdate(req)
		}
		if w.resumeLate {
			w.pendingResume = req
		}
		return nil, w.resumeErr
	}
	if w.applyUpdate(req) {
		w.log("schedule resumed")
	}
	return &workflowservice.UpdateScheduleResponse{}, nil
}

// applyUpdate applies an update unless its conflict token is out of date, and
// reports whether it did.
func (w *taskWorld) applyUpdate(req *workflowservice.UpdateScheduleRequest) bool {
	if req.ConflictToken != nil && !bytes.Equal(req.ConflictToken, w.tokenBytes()) {
		return false
	}
	w.paused, w.note = req.Schedule.GetState().GetPaused(), req.Schedule.GetState().GetNotes()
	w.token++
	return true
}

// deliverLateResume lets a resume whose call failed reach the schedule now.
func (w *taskWorld) deliverLateResume() {
	req := w.pendingResume
	if req == nil {
		return
	}
	w.pendingResume = nil
	if w.applyUpdate(req) {
		w.log("schedule resumed late")
	} else {
		w.log("late resume dropped")
	}
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

// newTaskWorld has one active recurring task of user-1's, task-1, daily at
// 09:00, created two days before now.
func newTaskWorld() *taskWorld {
	return &taskWorld{rows: map[string]pgdb.Task{"task-1": {TaskID: "task-1", UserID: "user-1", ChatID: "chat-1",
		Type: string(TaskTypeRecurring), Time: "0 9 * * *", Status: string(TaskStatusActive), CreatedAt: now.Add(-48 * time.Hour)}}}
}

// setTime changes task-1's schedule.
func (w *taskWorld) setTime(expr string) {
	row := w.rows["task-1"]
	row.Time = expr
	w.rows["task-1"] = row
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
