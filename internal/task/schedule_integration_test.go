//go:build integration

package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	"github.com/google/uuid"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	sdklog "go.temporal.io/sdk/log"
	"google.golang.org/protobuf/proto"
)

// openTestTemporal connects to the Temporal server at TEST_TEMPORAL_ADDRESS,
// such as one from `temporal server start-dev`, in its default namespace.
func openTestTemporal(t *testing.T) *Service {
	t.Helper()
	address := os.Getenv("TEST_TEMPORAL_ADDRESS")
	if address == "" {
		t.Skip("TEST_TEMPORAL_ADDRESS not set")
	}
	c, err := client.Dial(client.Options{HostPort: address, Namespace: "default",
		Logger: sdklog.NewStructuredLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &Service{temporalClient: c, namespace: "default", logger: logger.New(logger.Config{Level: slog.LevelError + 4})}
}

// A resume read before an owner's pause is void once the pause is made, also
// when it reaches Temporal after the pause. A resume read after the pause
// applies, and changes nothing but the schedule's state.
func TestAResumeAppliesOnlyToTheScheduleAsRead(t *testing.T) {
	s := openTestTemporal(t)
	ctx := context.Background()
	id := "resume-test-" + uuid.NewString()
	run := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Minute)
	handle, err := s.temporalClient.ScheduleClient().Create(ctx, client.ScheduleOptions{
		ID: id,
		Spec: client.ScheduleSpec{
			CronExpressions: []string{fmt.Sprintf("%d %d %d %d *", run.Minute(), run.Hour(), run.Day(), int(run.Month()))},
			StartAt:         run,
			EndAt:           run,
		},
		Action: &client.ScheduleWorkflowAction{ID: id + "-workflow", Workflow: "ScheduledTaskWorkflow",
			Args: []interface{}{map[string]interface{}{"task_id": id}}, TaskQueue: "task-queue"},
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: scheduleCatchupWindow,
		Memo:          map[string]interface{}{"user_id": "user-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Delete(context.Background()) })
	note := pauseNote(PauseUser, "paused by the owner")
	if err := handle.Pause(ctx, client.SchedulePauseOptions{Note: note}); err != nil {
		t.Fatal(err)
	}
	read := func() *scheduleRead {
		t.Helper()
		r, err := s.readForResume(ctx, id)
		if err != nil || r == nil {
			t.Fatalf("read: %v, %v", r, err)
		}
		return r
	}

	stale := read()
	if err := handle.Pause(ctx, client.SchedulePauseOptions{Note: note}); err != nil {
		t.Fatal(err)
	}
	// One implementation of schedules refuses the outdated resume; the other
	// accepts the call and drops the resume when it processes it.
	var refused *serviceerror.FailedPrecondition
	if err := s.resumeAsRead(ctx, id, stale, "resumed by the owner"); err != nil && !errors.As(err, &refused) {
		t.Fatal(err)
	}
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(100 * time.Millisecond) {
		if !read().paused() {
			t.Fatal("a resume read before the pause applied after it")
		}
	}

	current := read()
	if err := s.resumeAsRead(ctx, id, current, "resumed by the owner"); err != nil {
		t.Fatal(err)
	}
	resumed := read()
	for until := time.Now().Add(5 * time.Second); resumed.paused(); resumed = read() {
		if time.Now().After(until) {
			t.Fatal("a resume read after the pause did not apply")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if resumed.note() != "resumed by the owner" ||
		!proto.Equal(resumed.schedule.GetSpec(), current.schedule.GetSpec()) ||
		!proto.Equal(resumed.schedule.GetAction(), current.schedule.GetAction()) ||
		!proto.Equal(resumed.schedule.GetPolicies(), current.schedule.GetPolicies()) {
		t.Fatalf("the resume changed more than the state:\nbefore %v\nafter  %v", current.schedule, resumed.schedule)
	}
	desc, err := handle.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := desc.Memo.GetFields()["user_id"]; !ok {
		t.Fatal("the resume dropped the schedule's memo")
	}
}
