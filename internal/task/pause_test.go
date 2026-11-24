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
	expectEvents(t, w, "schedule paused:admin: incident 2026-11", "row paused:admin")
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
	// The row takes the admin pause; the schedule stays paused.
	expectEvents(t, w, "@row resumed", "row paused:admin")
	if row := w.rows["task-1"]; row.Status != string(TaskStatusPaused) || *row.PauseReason != string(PauseAdmin) || !w.paused {
		t.Fatalf("row %s/%s, schedule paused %v; want both paused by the admin", row.Status, *row.PauseReason, w.paused)
	}
}

// An admin pause that lands while the row is being resumed is still seen: the
// schedule is read right before it is resumed.
func TestResumeTaskRefusesAnAdminPauseMadeDuringTheResume(t *testing.T) {
	w := pausedWorld(PauseUser)
	w.duringAdmission = func() { w.note = "admin: incident 2026-11" }
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "paused_by_admin" {
		t.Fatalf("err = %v, want refused as an admin pause", err)
	}
	expectEvents(t, w, "@row resumed", "row paused:admin")
	if !w.paused || w.note != "admin: incident 2026-11" {
		t.Fatalf("schedule paused %v with note %q; want the admin pause kept", w.paused, w.note)
	}
}

// Resuming sets the row active under the admission lock, then resumes the
// schedule, for each reason an owner may lift.
func TestResumeTaskResumesTheRowThenTheSchedule(t *testing.T) {
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

// A resumed recurring task must fit the current plan as a new one would:
// otherwise pausing one task to make room for another and resuming the first
// gets past the limits.
func TestResumeTaskAppliesThePlansScheduleLimits(t *testing.T) {
	// Every five minutes is under Free's one-hour floor (a task from a paid plan,
	// or from before the limits).
	w := pausedWorld(PauseUser)
	w.setTime("*/5 * * * *")
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "min_interval" {
		t.Fatalf("err = %v, want refused under the plan's shortest interval", err)
	}
	expectEvents(t, w)

	// Hourly fits Free on its own (24 runs), but not beside another hourly task
	// created while this one was paused.
	w = pausedWorld(PauseUser)
	w.setTime("0 * * * *")
	w.open = []pgdb.ListOpenTasksForUsersRow{{Type: string(TaskTypeRecurring), Time: "30 * * * *", Status: string(TaskStatusActive)}}
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "runs_per_day" {
		t.Fatalf("err = %v, want refused past the plan's runs per day", err)
	}
	expectEvents(t, w)

	// The same pair fits Plus (100 runs a day).
	w = pausedWorld(PauseUser)
	w.setTime("0 * * * *")
	w.open = []pgdb.ListOpenTasksForUsersRow{{Type: string(TaskTypeRecurring), Time: "30 * * * *", Status: string(TaskStatusActive)}}
	if _, err := w.service(tiers.TierPlus).ResumeTask(context.Background(), "user-1", "task-1"); err != nil {
		t.Fatalf("Plus: %v", err)
	}
}
func TestResumeTaskResumesAScheduleWhoseRowStayedActive(t *testing.T) {
	w := newTaskWorld()
	w.paused, w.note = true, pauseNote(PauseUser, "paused by the owner")
	got, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1")
	if err != nil {
		t.Fatal(err)
	}
	expectEvents(t, w, "schedule resumed")
	if got.Status != string(TaskStatusActive) || w.paused {
		t.Fatalf("task %+v, schedule paused %v; want both running", got, w.paused)
	}

	w = newTaskWorld()
	w.paused, w.note = true, "admin: incident 2026-11"
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); limitReason(err) != "paused_by_admin" {
		t.Fatalf("err = %v, want refused as an admin pause", err)
	}
	expectEvents(t, w, "row paused:admin") // the row follows the schedule

	w = newTaskWorld() // running: nothing to do
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	expectEvents(t, w)
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

// A failed Unpause may still take effect after a lookup shows the schedule
// paused, so the row stays active and counted; only a schedule that is gone
// puts the pause back on the row.
func TestResumeTaskKeepsTheTaskCountedAfterAFailedUnpause(t *testing.T) {
	w := pausedWorld(PauseInactive)
	w.unpauseErr = serviceerror.NewUnavailable("down")
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); err == nil {
		t.Fatal("ResumeTask succeeded although the schedule did not resume")
	}
	expectEvents(t, w, "@row resumed")
	if row := w.rows["task-1"]; row.Status != string(TaskStatusActive) {
		t.Fatalf("row %s, want active and counted", row.Status)
	}

	// The retry resumes the schedule: the row says active, the schedule paused.
	w.unpauseErr = nil
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if w.paused {
		t.Fatal("the retry left the schedule paused")
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

// A failed Unpause can still apply after a lookup shows the schedule paused.
// An owner's pause made before then must stay in force once it applies, or the
// schedule would run while its row, paused, no longer counts.
func TestPauseTaskOutlastsAFailedUnpauseThatAppliesLate(t *testing.T) {
	w := pausedWorld(PauseUser)
	w.unpauseErr, w.unpauseLate = serviceerror.NewDeadlineExceeded("late"), true
	s := w.service(tiers.TierFree)
	if _, err := s.ResumeTask(context.Background(), "user-1", "task-1"); err == nil {
		t.Fatal("ResumeTask succeeded although Unpause failed")
	}
	if _, err := s.PauseTask(context.Background(), "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	w.applyLateUnpause()
	if row := w.rows["task-1"]; !w.paused || row.Status != string(TaskStatusPaused) {
		t.Fatalf("schedule paused %v, row %s; want both paused", w.paused, row.Status)
	}
	expectEvents(t, w, "@row resumed", "schedule resumed late", "schedule paused:user: test", "row paused:user")
}

// After a failed Unpause the schedule may have resumed: while its state is
// unknown the row stays active, so the task keeps counting.
func TestResumeTaskKeepsTheTaskCountedWhenTheOutcomeIsUnknown(t *testing.T) {
	w := pausedWorld(PauseUser)
	w.unpauseErr = serviceerror.NewDeadlineExceeded("late")
	w.describeErrs = []error{nil, serviceerror.NewUnavailable("down")} // the check before, then the one after
	if _, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1"); err == nil {
		t.Fatal("ResumeTask succeeded with the schedule's state unknown")
	}
	expectEvents(t, w, "@row resumed")
	if row := w.rows["task-1"]; row.Status != string(TaskStatusActive) {
		t.Fatalf("row %s, want active and counted", row.Status)
	}
}

func TestResumeTaskSucceedsWhenTheScheduleResumedDespiteAnError(t *testing.T) {
	w := pausedWorld(PauseUser)
	w.unpauseErr, w.unpauseApplies = serviceerror.NewDeadlineExceeded("late"), true
	got, err := w.service(tiers.TierFree).ResumeTask(context.Background(), "user-1", "task-1")
	if err != nil {
		t.Fatalf("ResumeTask failed although the schedule resumed: %v", err)
	}
	expectEvents(t, w, "@row resumed")
	if got.Status != string(TaskStatusActive) || w.paused {
		t.Fatalf("task %+v, schedule paused %v; want both running", got, w.paused)
	}
}

// Pause, resume and delete read and write a task only under its lock, so they
// cannot interleave with each other or with the task's creation.
func TestTaskChangesRunUnderTheTaskLock(t *testing.T) {
	w := newTaskWorld()
	s := w.service(tiers.TierFree)
	ctx := context.Background()
	if _, err := s.PauseTask(ctx, "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResumeTask(ctx, "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTask(ctx, "user-1", "task-1"); err != nil {
		t.Fatal(err)
	}
	if len(w.events) == 0 || len(w.outsideLock) != 0 {
		t.Fatalf("events %q; outside the lock %q", w.events, w.outsideLock)
	}
}
