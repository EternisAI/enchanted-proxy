package task

import (
	"context"
	stderrors "errors"
	"slices"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/tiers"
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

// fakeTemporal is a Temporal client whose schedule creation fails.
type fakeTemporal struct{ client.Client }

func (fakeTemporal) ScheduleClient() client.ScheduleClient { return failingSchedules{} }

type failingSchedules struct{ client.ScheduleClient }

func (failingSchedules) Create(context.Context, client.ScheduleOptions) (client.ScheduleHandle, error) {
	return nil, stderrors.New("temporal unavailable")
}

func TestCreateTaskReleasesAnUnscheduledTask(t *testing.T) {
	for _, kind := range []string{"recurring", "one_time"} {
		q := &limitQuerier{}
		s := newLimitService(tiers.TierFree, q)
		s.temporalClient = fakeTemporal{}
		if _, err := s.CreateTask(context.Background(), "user-1", request(kind, "0 9 24 12 *")); err == nil {
			t.Fatalf("%s: CreateTask succeeded with Temporal down", kind)
		}
		if len(q.deletedTasks) != 1 || !slices.Equal(q.deletedTasks, q.releasedCreation) {
			t.Fatalf("%s: deleted tasks %v, released creations %v; want the admitted task released", kind, q.deletedTasks, q.releasedCreation)
		}
	}
}
