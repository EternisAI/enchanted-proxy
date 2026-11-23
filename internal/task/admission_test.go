package task

import (
	"context"
	"slices"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

func TestAdmissionWritesTheTaskAndItsCreationUnderTheLock(t *testing.T) {
	q := &limitQuerier{}
	task, err := newLimitService(tiers.TierFree, q).admitTask(context.Background(), "user-1", "task-1", request("recurring", "0 9 * * *"), now)
	if err != nil {
		t.Fatalf("admission failed: %v", err)
	}
	if task.TaskID != "task-1" || task.Status != string(TaskStatusPending) {
		t.Fatalf("admitted task %+v, want task-1 pending", task)
	}
	if want := []string{"@task", "@creation"}; !slices.Equal(q.writes, want) {
		t.Fatalf("writes %v, want %v: the task and its creation inside the admission", q.writes, want)
	}
}

func TestRefusedAdmissionWritesNothing(t *testing.T) {
	q := &limitQuerier{created: 40}
	err := newLimitService(tiers.TierFree, q).admitErr(context.Background(), "user-1", request("recurring", "0 9 * * *"), now)
	if limitReason(err) != "tasks_created_per_day" {
		t.Fatalf("err = %v, want the daily creation limit", err)
	}
	if len(q.writes) != 0 {
		t.Fatalf("refused admission wrote %v", q.writes)
	}
}

func TestAdmissionLocksTheWholeGroup(t *testing.T) {
	q := &limitQuerier{}
	if err := newLimitService(tiers.TierFree, q).admitErr(context.Background(), "user-1", request("recurring", "0 9 * * *"), now); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(q.lockedFor, []string{"user-1", "other-account"}) {
		t.Fatalf("locked %v, want the caller's quota group", q.lockedFor)
	}
}

// scriptedTemporal is a Temporal client whose schedule calls have set outcomes.
type scriptedTemporal struct {
	client.Client
	schedules *scriptedSchedules
}

func (c scriptedTemporal) ScheduleClient() client.ScheduleClient { return c.schedules }

type scriptedSchedules struct {
	client.ScheduleClient
	createErr   error // returned by Create
	describeErr error // returned by Describe; nil means the schedule exists

	options      client.ScheduleOptions // what Create was asked for
	createCtxErr error                  // the context's state when Create was called
	deleted      bool
}

func (s *scriptedSchedules) Create(ctx context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	s.options, s.createCtxErr = o, ctx.Err()
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &scriptedHandle{schedules: s, id: o.ID}, nil
}

func (s *scriptedSchedules) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return &scriptedHandle{schedules: s, id: id}
}

type scriptedHandle struct {
	client.ScheduleHandle
	schedules *scriptedSchedules
	id        string
}

func (h *scriptedHandle) GetID() string { return h.id }

func (h *scriptedHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	if h.schedules.describeErr != nil {
		return nil, h.schedules.describeErr
	}
	return &client.ScheduleDescription{}, nil
}

func (h *scriptedHandle) Delete(context.Context) error {
	h.schedules.deleted = true
	return nil
}

func scriptedService(q *limitQuerier, schedules *scriptedSchedules) *Service {
	s := newLimitService(tiers.TierFree, q)
	s.temporalClient = scriptedTemporal{schedules: schedules}
	return s
}

func TestCreateTaskReleasesAnUnscheduledTask(t *testing.T) {
	for _, kind := range []string{"recurring", "one_time"} {
		q := &limitQuerier{}
		schedules := &scriptedSchedules{createErr: serviceerror.NewUnavailable("down"), describeErr: serviceerror.NewNotFound("no schedule")}
		if _, err := scriptedService(q, schedules).CreateTask(context.Background(), "user-1", request(kind, "0 9 24 12 *")); err == nil {
			t.Fatalf("%s: CreateTask succeeded with Temporal down", kind)
		}
		if len(q.deletedTasks) != 1 || !slices.Equal(q.deletedTasks, q.releasedCreation) {
			t.Fatalf("%s: deleted tasks %v, released creations %v; want the admitted task released", kind, q.deletedTasks, q.releasedCreation)
		}
	}
}

// A failed call does not prove that Temporal did not create the schedule.
func TestCreateTaskKeepsAScheduleCreatedDespiteAnError(t *testing.T) {
	q := &limitQuerier{}
	schedules := &scriptedSchedules{createErr: serviceerror.NewDeadlineExceeded("late")}
	task, err := scriptedService(q, schedules).CreateTask(context.Background(), "user-1", request("recurring", "0 9 * * *"))
	if err != nil {
		t.Fatalf("CreateTask failed although the schedule exists: %v", err)
	}
	if task.Status != string(TaskStatusActive) || !slices.Equal(q.statusUpdates, []string{string(TaskStatusActive)}) {
		t.Fatalf("task %+v, status updates %v; want it active", task, q.statusUpdates)
	}
	if len(q.deletedTasks) != 0 || len(q.releasedCreation) != 0 {
		t.Fatalf("released a task whose schedule exists: %v %v", q.deletedTasks, q.releasedCreation)
	}
}

func TestCreateTaskKeepsTheTaskCountedWhenTheOutcomeIsUnknown(t *testing.T) {
	q := &limitQuerier{}
	schedules := &scriptedSchedules{createErr: serviceerror.NewDeadlineExceeded("late"), describeErr: serviceerror.NewUnavailable("down")}
	if _, err := scriptedService(q, schedules).CreateTask(context.Background(), "user-1", request("recurring", "0 9 * * *")); err == nil {
		t.Fatal("CreateTask succeeded with the schedule's state unknown")
	}
	if len(q.deletedTasks) != 0 || len(q.releasedCreation) != 0 {
		t.Fatalf("released a task whose schedule may exist: %v %v", q.deletedTasks, q.releasedCreation)
	}
}

// A client that disconnects after admission interrupts neither the scheduling
// nor the cleanup.
func TestCreateTaskIgnoresAClientThatLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q := &limitQuerier{}
	schedules := &scriptedSchedules{createErr: serviceerror.NewUnavailable("down"), describeErr: serviceerror.NewNotFound("no schedule")}
	_, _ = scriptedService(q, schedules).CreateTask(ctx, "user-1", request("recurring", "0 9 * * *"))
	if schedules.createCtxErr != nil {
		t.Errorf("Create ran on a cancelled context: %v", schedules.createCtxErr)
	}
	if q.deleteCtxErr != nil || len(q.releasedCreation) != 1 {
		t.Errorf("cleanup context error %v, released %v; want the task released", q.deleteCtxErr, q.releasedCreation)
	}
}
