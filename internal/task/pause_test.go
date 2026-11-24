package task

import (
	"context"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/gin-gonic/gin"
	"go.temporal.io/api/serviceerror"
)

// pausedWorld is newTaskWorld with task-1 paused for reason, in the row and
// in its schedule.
func pausedWorld(reason PauseReason) *taskWorld {
	w := newTaskWorld()
	row := w.rows["task-1"]
	row.Status, row.PauseReason = string(TaskStatusPaused), reasonPtr(reason)
	w.rows["task-1"] = row
	w.paused, w.note = true, pauseNote(reason, "test")
	return w
}

func reasonPtr(r PauseReason) *string {
	s := string(r)
	return &s
}

func TestPauseTaskPausesTheScheduleThenTheRow(t *testing.T) {
	w := newTaskWorld()
	got, err := w.service(tiers.TierFree).PauseTask(context.Background(), "user-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	expectEvents(t, w, "schedule paused:user: paused by the owner", "row paused:user")
	if got.Status != string(TaskStatusPaused) || got.PauseReason != string(PauseUser) {
		t.Fatalf("task %+v, want paused by the user", got)
	}
}

// A pause made in Temporal before the row caught up (an admin's, here) is kept
// with its note, so the owner cannot turn it into a pause they may lift.
func TestPauseTaskKeepsAPauseMadeInTemporal(t *testing.T) {
	w := newTaskWorld()
	w.paused, w.note = true, "admin: incident 2026-11"
	s := w.service(tiers.TierFree)
	got, err := s.PauseTask(context.Background(), "user-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	expectEvents(t, w, "row paused:admin")
	if got.PauseReason != string(PauseAdmin) || w.note != "admin: incident 2026-11" {
		t.Fatalf("task %+v, note %q; want the admin pause kept", got, w.note)
	}
	if _, err := s.ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "paused_by_admin" {
		t.Fatalf("resume err = %v, want refused as an admin pause", err)
	}
}

func TestPauseTaskOfAnotherAccountIsNotFound(t *testing.T) {
	w := newTaskWorld()
	s := w.service(tiers.TierFree)
	for _, change := range []func(context.Context, string, string) (*Task, error){s.PauseTask, s.ResumeTask} {
		if _, err := change(context.Background(), "user-2", "task-1"); !stderrors.Is(err, ErrTaskNotFound) {
			t.Fatalf("err = %v, want ErrTaskNotFound", err)
		}
	}
	if err := s.DeleteTask(context.Background(), "user-2", "task-1"); !stderrors.Is(err, ErrTaskNotFound) {
		t.Fatalf("delete err = %v, want ErrTaskNotFound", err)
	}
	expectEvents(t, w)
}

func TestResumeTaskRefusesAdminAndPlanEndedPauses(t *testing.T) {
	for reason, want := range map[PauseReason]string{PauseAdmin: "paused_by_admin", PausePlanEnded: "plan_ended"} {
		w := pausedWorld(reason)
		if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != want {
			t.Fatalf("%s: err = %v, want refused as %s", reason, err, want)
		}
		expectEvents(t, w)
	}
}

// The schedule's note decides when the row has not caught up with a pause made
// in Temporal.
func TestResumeTaskRefusesWhenTheScheduleWasPausedByAnAdmin(t *testing.T) {
	w := pausedWorld(PauseUser)
	w.note = "admin: incident 2026-11"
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "paused_by_admin" {
		t.Fatalf("err = %v, want refused as an admin pause", err)
	}
	expectEvents(t, w)
}

// Resuming does not apply the creation limits again: this every-5-minutes task
// is under Free's one-hour floor, but it existed before, so pausing it stays
// reversible. The row is set active under the admission lock, then the schedule
// resumes.
func TestResumeTaskDoesNotApplyTheScheduleLimitsAgain(t *testing.T) {
	for _, reason := range []PauseReason{PauseUser, PauseInactive, PauseAccountMissing} {
		w := pausedWorld(reason)
		got, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1")
		if err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
		expectEvents(t, w, "@row resumed", "schedule resumed")
		if got.Status != string(TaskStatusActive) || got.PauseReason != "" {
			t.Fatalf("%s: task %+v, want active", reason, got)
		}
	}
}

func TestResumeTaskRefusesPastThePlansTaskCount(t *testing.T) {
	w := pausedWorld(PauseUser)
	for range tiers.Configs[tiers.TierFree].TaskMaxActiveRecurring {
		w.open = append(w.open, pgdb.ListOpenTasksForUsersRow{Type: string(TaskTypeRecurring), Time: "0 9 * * *", Status: string(TaskStatusActive)})
	}
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "active_recurring_tasks" {
		t.Fatalf("err = %v, want refused at the task count", err)
	}
	expectEvents(t, w)
}

func TestResumeTaskRefusesAOneTimeTaskWhoseTimePassed(t *testing.T) {
	w := pausedWorld(PauseUser)
	row := w.rows["task-1"]
	row.Type, row.Time = string(TaskTypeOneTime), "0 9 21 11 *" // the day before now
	w.rows["task-1"] = row
	var invalid *ValidationError
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); !stderrors.As(err, &invalid) {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	expectEvents(t, w)
}

// When the schedule cannot be resumed, the row is paused again so it does not
// count as active.
func TestResumeTaskRestoresThePauseWhenTheScheduleDoesNotResume(t *testing.T) {
	w := pausedWorld(PauseInactive)
	w.unpauseErr = serviceerror.NewUnavailable("down")
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); err == nil {
		t.Fatal("ResumeTask succeeded although the schedule did not resume")
	}
	expectEvents(t, w, "@row resumed", "row paused:inactive")
	if row := w.rows["task-1"]; row.Status != string(TaskStatusPaused) || *row.PauseReason != string(PauseInactive) {
		t.Fatalf("row %+v, want paused as before", row)
	}
}

func TestPauseReasonFromNote(t *testing.T) {
	for note, want := range map[string]PauseReason{
		"admin: incident 2026-11":            PauseAdmin,
		"account_missing: owner deleted":     PauseAccountMissing,
		"inactive: owner inactive 90+ days":  PauseInactive,
		"Inactive owner":                     PauseInactive,
		"user: paused by the owner":          PauseUser,
		"plan_ended: over the plan's limits": PausePlanEnded,
		"Paused via Go SDK":                  PauseAdmin,
		"":                                   PauseAdmin,
	} {
		if got := pauseReasonFromNote(note); got != want {
			t.Errorf("pauseReasonFromNote(%q) = %q, want %q", note, got, want)
		}
	}
}

func TestPauseAndResumeHandlersAnswerWithReadableRefusals(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := pausedWorld(PauseAdmin)
	s := w.service(tiers.TierFree)
	h := NewHandler(s, s.logger)
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set(string(auth.UserIDKey), "user-1") })
	engine.POST("/api/v1/tasks/:taskId/pause", h.PauseTask)
	engine.POST("/api/v1/tasks/:taskId/resume", h.ResumeTask)

	for _, tc := range []struct {
		path string
		code int
		body string
	}{
		{"/api/v1/tasks/task-1/resume", http.StatusForbidden, "cannot be resumed"},
		{"/api/v1/tasks/task-1/pause", http.StatusOK, `"pause_reason":"admin"`},
		{"/api/v1/tasks/other/pause", http.StatusNotFound, "task not found"},
	} {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))
		if rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%s: %d %s, want %d with %q", tc.path, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
}
