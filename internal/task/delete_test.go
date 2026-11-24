package task

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/api/serviceerror"
)

func TestDeleteTaskDeletesTheScheduleBeforeTheRow(t *testing.T) {
	w := newTaskWorld()
	if err := w.service(tiers.TierFree).DeleteTask(context.Background(), "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, w, "schedule deleted", "row deleted")
}

// While the schedule may still run, the row stays and keeps it counted.
func TestDeleteTaskKeepsTheRowWhileTheScheduleMayExist(t *testing.T) {
	for name, describeErr := range map[string]error{"schedule exists": nil, "unknown": serviceerror.NewUnavailable("down")} {
		w := newTaskWorld()
		w.deleteErr, w.describeErr = serviceerror.NewUnavailable("down"), describeErr
		if err := w.service(tiers.TierFree).DeleteTask(context.Background(), "user-1", "task-1"); err == nil {
			t.Fatalf("%s: DeleteTask succeeded although the schedule may still run", name)
		}
		expectEvents(t, w)
	}
}

func TestDeleteTaskRemovesTheRowOnceTheScheduleIsGone(t *testing.T) {
	// No schedule at all, and a failed call after which the schedule is gone.
	for name, deleteErr := range map[string]error{"not found": nil, "failed call": serviceerror.NewUnavailable("down")} {
		w := newTaskWorld()
		w.missing, w.deleteErr = true, deleteErr
		if err := w.service(tiers.TierFree).DeleteTask(context.Background(), "user-1", "task-1"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		expectEvents(t, w, "row deleted")
	}
}

func TestDeleteTaskOfAnotherAccountIsNotFound(t *testing.T) {
	w := newTaskWorld()
	if err := w.service(tiers.TierFree).DeleteTask(context.Background(), "user-2", "task-1"); !stderrors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err = %v, want ErrTaskNotFound", err)
	}
	expectEvents(t, w)
}
