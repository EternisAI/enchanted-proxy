package task

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/workflow"
)

// reconWorld is task rows, their schedules, accounts and plans, logging every
// change in order.
type reconWorld struct {
	pgdb.Querier // any other query panics
	client.Client

	rows      map[string]pgdb.Task
	creations map[string]bool
	schedules map[string]*fakeSchedule
	// lateRows are rows that appear after the snapshot was taken, as a task
	// created during the sweep would.
	lateRows map[string]pgdb.Task
	// listed overrides which schedules List returns, and stale how it shows
	// them (a listing that lags behind).
	listed []string
	stale  map[string]fakeSchedule

	accounts  map[string]Account
	lookupErr error
	tiers     map[string]tiers.Tier
	pushes    []string
	events    []string
	now       time.Time

	locked      bool     // inside a task's lock
	outsideLock []string // changes made outside one

	// unpauseErr is returned by Unpause; with unpauseLate the Unpause still
	// applies later, at the schedule's next change or applyLateUnpauses.
	unpauseErr     error
	unpauseLate    bool
	pendingUnpause map[string]bool
}

type fakeSchedule struct {
	paused   bool
	note     string
	workflow string
}

func newReconWorld() *reconWorld {
	return &reconWorld{
		rows: map[string]pgdb.Task{}, creations: map[string]bool{}, schedules: map[string]*fakeSchedule{},
		lateRows: map[string]pgdb.Task{}, accounts: map[string]Account{}, tiers: map[string]tiers.Tier{}, now: now,
		pendingUnpause: map[string]bool{},
	}
}

// owner adds an account last active the given time before now.
func (w *reconWorld) owner(id string, idle time.Duration, tier tiers.Tier) {
	w.accounts[id] = Account{Exists: true, LastActive: w.now.Add(-idle)}
	w.tiers[id] = tier
}

// task adds a row and, unless schedule is nil, its schedule.
func (w *reconWorld) task(id, owner, kind, expr string, status TaskStatus, age time.Duration, schedule *fakeSchedule) {
	w.rows[id] = pgdb.Task{TaskID: id, UserID: owner, ChatID: "chat-" + id, TaskName: "Task " + id, Type: kind, Time: expr,
		Status: string(status), CreatedAt: w.now.Add(-age)}
	w.creations[id] = true
	if schedule != nil {
		if schedule.workflow == "" {
			schedule.workflow = scheduledTaskWorkflow
		}
		w.schedules[id] = schedule
	}
}

func (w *reconWorld) setRow(id string, change func(*pgdb.Task)) {
	row := w.rows[id]
	change(&row)
	w.rows[id] = row
}

func (w *reconWorld) log(format string, args ...any) {
	event := fmt.Sprintf(format, args...)
	if !w.locked {
		w.outsideLock = append(w.outsideLock, event)
	}
	w.events = append(w.events, event)
}

func (w *reconWorld) ListTasksToReconcile(context.Context) ([]pgdb.Task, error) {
	var rows []pgdb.Task
	for _, row := range w.rows {
		if row.Status == "pending" || row.Status == "active" || row.Status == "paused" {
			rows = append(rows, row)
		}
	}
	slices.SortFunc(rows, func(a, b pgdb.Task) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.TaskID, b.TaskID)
	})
	for id, row := range w.lateRows {
		w.rows[id] = row
	}
	return rows, nil
}

func (w *reconWorld) GetTaskByID(_ context.Context, id string) (pgdb.Task, error) {
	if row, ok := w.rows[id]; ok {
		return row, nil
	}
	return pgdb.Task{}, sql.ErrNoRows
}

func (w *reconWorld) DeleteTaskByID(_ context.Context, id string) (int64, error) {
	w.log("delete row %s", id)
	delete(w.rows, id)
	return 1, nil
}

func (w *reconWorld) DeleteTaskCreation(_ context.Context, id string) error {
	w.log("release creation %s", id)
	delete(w.creations, id)
	return nil
}

func (w *reconWorld) MarkTaskPaused(_ context.Context, arg pgdb.MarkTaskPausedParams) (int64, error) {
	w.log("row %s paused:%s", arg.TaskID, *arg.PauseReason)
	w.setRow(arg.TaskID, func(r *pgdb.Task) { r.Status, r.PauseReason = "paused", arg.PauseReason })
	return 1, nil
}

func (w *reconWorld) MarkTaskResumed(_ context.Context, arg pgdb.MarkTaskResumedParams) (int64, error) {
	w.log("row %s resumed", arg.TaskID)
	w.setRow(arg.TaskID, func(r *pgdb.Task) { r.Status, r.PauseReason = "active", nil })
	return 1, nil
}

func (w *reconWorld) UpdateTaskPauseReason(_ context.Context, arg pgdb.UpdateTaskPauseReasonParams) (int64, error) {
	w.log("row %s reason:%s", arg.TaskID, *arg.PauseReason)
	w.setRow(arg.TaskID, func(r *pgdb.Task) { r.PauseReason = arg.PauseReason })
	return 1, nil
}

func (w *reconWorld) ActivatePendingTask(_ context.Context, id string) (int64, error) {
	w.log("row %s active", id)
	w.setRow(id, func(r *pgdb.Task) { r.Status = "active" })
	return 1, nil
}

func (w *reconWorld) CompleteOneTimeTask(_ context.Context, id string) (int64, error) {
	w.log("row %s completed", id)
	w.setRow(id, func(r *pgdb.Task) { r.Status = "completed" })
	return 1, nil
}

func (w *reconWorld) MarkTaskOverLimit(_ context.Context, arg pgdb.MarkTaskOverLimitParams) error {
	w.log("row %s over", arg.TaskID)
	w.setRow(arg.TaskID, func(r *pgdb.Task) {
		if !r.OverLimitSince.Valid {
			r.OverLimitSince = arg.Since
		}
	})
	return nil
}

func (w *reconWorld) ClearTaskOverLimit(_ context.Context, id string) error {
	w.log("row %s fits", id)
	w.setRow(id, func(r *pgdb.Task) { r.OverLimitSince = sql.NullTime{} })
	return nil
}

func (w *reconWorld) ScheduleClient() client.ScheduleClient { return reconSchedules{w: w} }

type reconSchedules struct {
	client.ScheduleClient
	w *reconWorld
}

func (s reconSchedules) List(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error) {
	ids := s.w.listed
	if ids == nil {
		for id := range s.w.schedules {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var entries []*client.ScheduleListEntry
	for _, id := range ids {
		sch := s.w.schedules[id]
		if old, ok := s.w.stale[id]; ok {
			sch = &old
		}
		entries = append(entries, &client.ScheduleListEntry{ID: id, Paused: sch.paused, Note: sch.note, WorkflowType: workflow.Type{Name: sch.workflow}})
	}
	return &listIterator{entries: entries}, nil
}

func (s reconSchedules) GetHandle(_ context.Context, id string) client.ScheduleHandle {
	return reconHandle{w: s.w, id: id}
}

type listIterator struct{ entries []*client.ScheduleListEntry }

func (it *listIterator) HasNext() bool { return len(it.entries) > 0 }
func (it *listIterator) Next() (*client.ScheduleListEntry, error) {
	e := it.entries[0]
	it.entries = it.entries[1:]
	return e, nil
}

type reconHandle struct {
	client.ScheduleHandle
	w  *reconWorld
	id string
}

func (h reconHandle) Describe(context.Context) (*client.ScheduleDescription, error) {
	sch, ok := h.w.schedules[h.id]
	if !ok {
		return nil, serviceerror.NewNotFound("no schedule")
	}
	return &client.ScheduleDescription{Schedule: client.Schedule{State: &client.ScheduleState{Paused: sch.paused, Note: sch.note}}}, nil
}

func (h reconHandle) Pause(_ context.Context, o client.SchedulePauseOptions) error {
	h.w.applyLateUnpause(h.id) // Temporal applies a schedule's changes in order
	h.w.log("schedule %s paused:%s", h.id, strings.SplitN(o.Note, ":", 2)[0])
	h.w.schedules[h.id].paused, h.w.schedules[h.id].note = true, o.Note
	return nil
}

func (h reconHandle) Unpause(context.Context, client.ScheduleUnpauseOptions) error {
	if h.w.unpauseErr != nil {
		if h.w.unpauseLate {
			h.w.pendingUnpause[h.id] = true
		}
		return h.w.unpauseErr
	}
	h.w.log("schedule %s resumed", h.id)
	h.w.schedules[h.id].paused = false
	return nil
}

func (w *reconWorld) applyLateUnpause(id string) {
	if w.pendingUnpause[id] {
		delete(w.pendingUnpause, id)
		w.log("schedule %s resumed late", id)
		w.schedules[id].paused = false
	}
}

func (w *reconWorld) applyLateUnpauses() {
	for id := range w.pendingUnpause {
		w.applyLateUnpause(id)
	}
}

func (h reconHandle) Delete(context.Context) error {
	if _, ok := h.w.schedules[h.id]; !ok {
		return serviceerror.NewNotFound("no schedule")
	}
	h.w.log("delete schedule %s", h.id)
	delete(h.w.schedules, h.id)
	return nil
}

func (w *reconWorld) LookupAccounts(_ context.Context, ids []string) (map[string]Account, error) {
	if w.lookupErr != nil {
		return nil, w.lookupErr
	}
	out := map[string]Account{}
	for _, id := range ids {
		out[id] = w.accounts[id] // absent: no account
	}
	return out, nil
}

func (w *reconWorld) GetUserTierConfig(_ context.Context, userID string) (tiers.Config, *time.Time, error) {
	tier, ok := w.tiers[userID]
	if !ok {
		tier = tiers.TierFree // as for an account with no plan
	}
	return tiers.Configs[tier], nil, nil
}

func (w *reconWorld) UserIDs(_ context.Context, userID string) ([]string, error) {
	return []string{userID}, nil
}

func (w *reconWorld) SendTaskPausedNotification(_ context.Context, userID, chatID, title, body string) error {
	w.pushes = append(w.pushes, fmt.Sprintf("%s|%s|%s|%s", userID, chatID, title, body))
	return nil
}

// reconciler builds a reconciler over the world, with the default settings.
func (w *reconWorld) reconciler(mode ReconcilerMode, notify bool) *Reconciler {
	s := &Service{
		temporalClient: w,
		queries:        w,
		tierLookup:     w,
		groups:         w,
		logger:         logger.New(logger.Config{Level: slog.LevelError + 4}),
		clock:          func() time.Time { return w.now },
		taskLock: func(_ context.Context, _ string, fn func() error) error {
			w.locked = true
			defer func() { w.locked = false }()
			return fn()
		},
	}
	return &Reconciler{
		s: s, accounts: w, notifier: w, log: s.logger,
		cfg: ReconcilerConfig{Mode: mode, Interval: time.Hour, InactiveAfter: 90 * 24 * time.Hour, PlanEndedGrace: 7 * 24 * time.Hour, PauseNotifications: notify},
	}
}

func (w *reconWorld) sweep(t *testing.T, mode ReconcilerMode) *Report {
	t.Helper()
	w.events = nil
	return w.reconciler(mode, false).sweep(context.Background())
}

func expectChanges(t *testing.T, w *reconWorld, want ...string) {
	t.Helper()
	got := slices.Clone(w.events)
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("changes\n  %q\nwant\n  %q", got, want)
	}
}

const oneDay = 24 * time.Hour

func TestReconcilerRecordsPausesAndResumesMadeInTemporal(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("admin-paused", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{paused: true, note: "admin: incident 2026-11"})
	w.task("reason-changed", "u", "recurring", "0 10 * * *", TaskStatusPaused, 30*oneDay, &fakeSchedule{paused: true, note: "account_missing: owner deleted"})
	w.setRow("reason-changed", func(r *pgdb.Task) { r.PauseReason = reasonPtr(PauseUser) })
	w.task("unpaused", "u", "recurring", "0 11 * * *", TaskStatusPaused, 30*oneDay, &fakeSchedule{})
	w.setRow("unpaused", func(r *pgdb.Task) { r.PauseReason = reasonPtr(PauseUser) })

	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, "schedule admin-paused paused:admin", "row admin-paused paused:admin", "row reason-changed reason:account_missing", "row unpaused resumed")
}

// After a failed resume the row stays active while the schedule reads paused,
// and the failed Unpause can still apply. Recording the pause must not leave
// the schedule running once it does, with a row that no longer counts it.
func TestReconcilerOutlastsAFailedUnpauseThatAppliesLate(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("resuming", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{paused: true, note: "user: paused by the owner"})
	w.pendingUnpause["resuming"] = true

	w.sweep(t, ReconcilerEnforce)
	w.applyLateUnpauses()
	if !w.schedules["resuming"].paused || w.rows["resuming"].Status != string(TaskStatusPaused) {
		t.Fatalf("schedule paused %v, row %s; want both paused", w.schedules["resuming"].paused, w.rows["resuming"].Status)
	}
}

// Lifting a hold counts the task before its schedule resumes, and a failed
// Unpause, which may still apply, keeps it counted.
func TestReconcilerKeepsALiftedHoldCountedWhileItsUnpauseMayApply(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierPro)
	w.task("held", "u", "recurring", "*/10 * * * *", TaskStatusPaused, 60*oneDay, &fakeSchedule{paused: true, note: "plan_ended: over the limits"})
	w.setRow("held", func(r *pgdb.Task) { r.PauseReason = reasonPtr(PausePlanEnded) })
	w.unpauseErr, w.unpauseLate = serviceerror.NewDeadlineExceeded("late"), true

	rep := w.sweep(t, ReconcilerEnforce)
	if len(rep.Errors) == 0 {
		t.Fatal("the failed Unpause was not reported")
	}
	if row := w.rows["held"]; row.Status != string(TaskStatusActive) {
		t.Fatalf("row %s, want active and counted while the Unpause may apply", row.Status)
	}

	// It applies: the next sweep finds nothing to change in the row.
	w.applyLateUnpauses()
	w.unpauseErr = nil
	w.sweep(t, ReconcilerEnforce)
	if row := w.rows["held"]; row.Status != string(TaskStatusActive) || w.schedules["held"].paused {
		t.Fatalf("row %s, schedule paused %v; want both running", row.Status, w.schedules["held"].paused)
	}
}

// A sweep keeps a connection for the reconciler's lock while its steps hold a
// task's lock and do their work.
func TestNewReconcilerRefusesAPoolTooSmallForItsSweeps(t *testing.T) {
	for _, c := range []struct {
		size    int
		mode    ReconcilerMode
		wantErr bool
	}{
		{0, ReconcilerEnforce, false}, // no limit
		{2, ReconcilerEnforce, true},
		{2, ReconcilerDryRun, true},
		{2, ReconcilerOff, false},
		{3, ReconcilerEnforce, false},
	} {
		db, err := sql.Open("postgres", "postgres://unused") // does not connect
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(c.size)
		_, err = NewReconciler(&Service{}, db, nil, nil, ReconcilerConfig{Mode: c.mode}, logger.New(logger.Config{Level: slog.LevelError + 4}))
		if (err != nil) != c.wantErr {
			t.Errorf("pool of %d, %s: err = %v, want an error %v", c.size, c.mode, err, c.wantErr)
		}
		_ = db.Close()
	}
}

func TestReconcilerSettlesRowsWithoutSchedules(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	// One-time tasks for Nov 21, 09:00, the day before now.
	w.task("one-time-ran", "u", "one_time", "0 9 21 11 *", TaskStatusActive, 3*oneDay, nil)
	w.task("one-time-ahead", "u", "one_time", "0 9 24 12 *", TaskStatusActive, 3*oneDay, nil)
	w.task("one-time-unreadable", "u", "one_time", "2025-08-20T15:00:00Z", TaskStatusActive, 400*oneDay, nil)
	w.task("recurring-gone", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, nil)
	w.task("paused-gone", "u", "recurring", "0 9 * * *", TaskStatusPaused, 30*oneDay, nil)
	w.task("pending-stale", "u", "recurring", "0 9 * * *", TaskStatusPending, 2*time.Hour, nil)
	w.task("pending-new", "u", "recurring", "0 9 * * *", TaskStatusPending, 10*time.Minute, nil)
	w.task("pending-scheduled", "u", "recurring", "0 9 * * *", TaskStatusPending, 2*time.Hour, &fakeSchedule{})

	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w,
		"row one-time-ran completed",
		"delete row one-time-ahead", "delete row one-time-unreadable", "delete row recurring-gone", "delete row paused-gone",
		"release creation pending-stale", "delete row pending-stale",
		"row pending-scheduled active")
	if !w.creations["recurring-gone"] {
		t.Error("deleting a row also released its creation; only pending tasks that never ran are released")
	}
}

// A row deleted only on Temporal's own word: a stale listing that misses the
// schedule changes nothing.
func TestReconcilerKeepsARowWhoseScheduleTheListingMissed(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("t", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{})
	w.listed = []string{}
	rep := w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)
	if len(rep.Skipped["delete_row"]) != 1 {
		t.Fatalf("skipped %v, want the deletion skipped", rep.Skipped)
	}
}

// A pause made since the listing (an admin's, here) is not overwritten: its
// note keeps the reason.
func TestReconcilerDoesNotOverwriteAPauseMadeSinceTheListing(t *testing.T) {
	w := newReconWorld()
	w.owner("idle", 100*oneDay, tiers.TierFree)
	w.task("t", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{paused: true, note: "admin: incident 2026-11"})
	w.stale = map[string]fakeSchedule{"t": {workflow: scheduledTaskWorkflow}}
	rep := w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)
	if w.schedules["t"].note != "admin: incident 2026-11" || len(rep.Skipped["pause:inactive"]) != 1 {
		t.Fatalf("note %q, skipped %v; want the admin pause kept", w.schedules["t"].note, rep.Skipped)
	}
}

func TestReconcilerDeletesSchedulesWithoutRows(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("kept", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{})
	w.schedules["orphan"] = &fakeSchedule{workflow: scheduledTaskWorkflow}
	w.schedules["created-meanwhile"] = &fakeSchedule{workflow: scheduledTaskWorkflow}
	w.lateRows["created-meanwhile"] = pgdb.Task{TaskID: "created-meanwhile", UserID: "u", Status: "pending"}
	w.schedules["not-a-task"] = &fakeSchedule{workflow: "SomeOtherWorkflow"}

	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, "delete schedule orphan")
}

func TestReconcilerKeepsSchedulesWhenMostHaveNoRow(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("kept", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{})
	for i := range minOrphans {
		w.schedules[fmt.Sprintf("other-db-%d", i)] = &fakeSchedule{workflow: scheduledTaskWorkflow}
	}
	rep := w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)
	if len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "shared") {
		t.Fatalf("errors %q, want the shared-namespace warning", rep.Errors)
	}
}

func TestReconcilerPausesTasksOfMissingAndInactiveOwners(t *testing.T) {
	w := newReconWorld()
	w.owner("active", 10*oneDay, tiers.TierFree)
	w.owner("idle", 100*oneDay, tiers.TierPlus)
	w.owner("disabled", oneDay, tiers.TierFree)
	w.accounts["disabled"] = Account{Exists: true, Disabled: true, LastActive: w.now.Add(-oneDay)}
	for _, owner := range []string{"active", "idle", "disabled", "deleted"} {
		w.task(owner+"-daily", owner, "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
		w.task(owner+"-once", owner, "one_time", "0 9 24 12 *", TaskStatusActive, oneDay, &fakeSchedule{})
	}

	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w,
		"schedule idle-daily paused:inactive", "row idle-daily paused:inactive",
		"schedule disabled-daily paused:account_missing", "row disabled-daily paused:account_missing",
		"schedule disabled-once paused:account_missing", "row disabled-once paused:account_missing",
		"schedule deleted-daily paused:account_missing", "row deleted-daily paused:account_missing",
		"schedule deleted-once paused:account_missing", "row deleted-once paused:account_missing")
	if w.schedules["idle-daily"].note != "inactive: owner inactive for 90 days (task reconciler)" {
		t.Errorf("pause note %q", w.schedules["idle-daily"].note)
	}
}

// Without accounts, nothing is judged missing or inactive.
func TestReconcilerSkipsAccountRulesWithoutAccounts(t *testing.T) {
	w := newReconWorld()
	w.owner("idle", 100*oneDay, tiers.TierFree)
	w.task("t", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
	w.lookupErr = errors.New("unavailable")
	rep := w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)
	if rep.AccountsKnown || len(rep.Errors) != 1 {
		t.Fatalf("report %+v, want accounts unknown and one error", rep)
	}
}

// Most owners missing at once points at the wrong Firebase project.
func TestReconcilerSkipsAccountRulesWhenMostOwnersAreMissing(t *testing.T) {
	w := newReconWorld()
	w.owner("present", oneDay, tiers.TierFree)
	w.task("present-t", "present", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{})
	for i := range minMissingOwners {
		id := fmt.Sprintf("missing-%d", i)
		w.task(id+"-t", id, "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, &fakeSchedule{})
	}
	rep := w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)
	if rep.AccountsKnown || len(rep.Errors) != 1 || !strings.Contains(rep.Errors[0], "Firebase project") {
		t.Fatalf("report %+v, want account rules skipped with a warning", rep)
	}
}

// A Free owner with a task every ten minutes (under Free's one-hour floor)
// and a daily task: the frequent one is marked over, held once the grace has
// passed, and released when the plan allows it again (Pro: a five-minute
// floor, 200 runs a day); the daily one runs on.
func TestReconcilerHoldsTasksOverThePlanAndLiftsTheHold(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	w.task("frequent", "u", "recurring", "*/10 * * * *", TaskStatusActive, 60*oneDay, &fakeSchedule{})
	w.task("daily", "u", "recurring", "0 9 * * *", TaskStatusActive, 50*oneDay, &fakeSchedule{})

	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, "row frequent over")

	w.now = w.now.Add(6 * oneDay) // within the grace
	w.owner("u", oneDay, tiers.TierFree)
	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w)

	w.now = w.now.Add(2 * oneDay)
	w.owner("u", oneDay, tiers.TierFree)
	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, "schedule frequent paused:plan_ended", "row frequent paused:plan_ended")

	w.tiers["u"] = tiers.TierPro // renewed
	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, "schedule frequent resumed", "row frequent resumed", "row frequent fits")
}

// Oldest first: past the plan's task count, the newest tasks are over.
func TestReconcilerHoldsTheNewestTasksPastThePlansCount(t *testing.T) {
	w := newReconWorld()
	w.owner("u", oneDay, tiers.TierFree)
	limit := tiers.Configs[tiers.TierFree].TaskMaxActiveRecurring
	for i := range limit + 2 {
		w.task(fmt.Sprintf("t%02d", i), "u", "recurring", fmt.Sprintf("%d 9 * * 1", i), TaskStatusActive, time.Duration(100-i)*oneDay, &fakeSchedule{})
	}
	w.sweep(t, ReconcilerEnforce)
	expectChanges(t, w, fmt.Sprintf("row t%02d over", limit), fmt.Sprintf("row t%02d over", limit+1))
}

func TestReconcilerDryRunChangesNothing(t *testing.T) {
	w := newReconWorld()
	w.owner("idle", 100*oneDay, tiers.TierFree)
	w.task("idle-t", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
	w.task("gone", "idle", "recurring", "0 9 * * *", TaskStatusPaused, 200*oneDay, nil)
	w.task("ran", "idle", "one_time", "0 9 21 11 *", TaskStatusActive, 3*oneDay, nil)
	w.schedules["orphan"] = &fakeSchedule{workflow: scheduledTaskWorkflow}

	rep := w.sweep(t, ReconcilerDryRun)
	expectChanges(t, w)
	want := map[string][]string{"pause:inactive": {"idle-t"}, "delete_row": {"gone"}, "complete": {"ran"}, "delete_schedule": {"orphan"}}
	for label, ids := range want {
		if !slices.Equal(rep.Actions[label], ids) {
			t.Errorf("%s: %v, want %v", label, rep.Actions[label], ids)
		}
	}
}

func TestReconcilerSendsOnePushPerOwnerAndReason(t *testing.T) {
	w := newReconWorld()
	w.owner("idle", 100*oneDay, tiers.TierFree)
	w.task("a", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
	w.task("b", "idle", "recurring", "0 10 * * *", TaskStatusActive, 199*oneDay, &fakeSchedule{})
	w.owner("gone", 0, tiers.TierFree)
	delete(w.accounts, "gone")
	w.task("c", "gone", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})

	rep := w.reconciler(ReconcilerEnforce, true).sweep(context.Background())
	want := `idle|chat-a|Tasks paused|"Task a" and "Task b" were paused because the app was not opened for 90 days. Ask in chat to resume them.`
	if len(w.pushes) != 1 || w.pushes[0] != want || rep.Notifications != 1 {
		t.Fatalf("pushes %q, want only %q", w.pushes, want)
	}

	// With the switch off, the same pauses send nothing.
	w2 := newReconWorld()
	w2.owner("idle", 100*oneDay, tiers.TierFree)
	w2.task("a", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
	w2.reconciler(ReconcilerEnforce, false).sweep(context.Background())
	if len(w2.pushes) != 0 {
		t.Fatalf("pushes %q with notifications off", w2.pushes)
	}
}

func TestPauseNoticeForOneHeldTask(t *testing.T) {
	title, body := pauseNotice(PausePlanEnded, []action{{row: pgdb.Task{TaskName: "News"}, plan: "Free"}}, 90*oneDay)
	if title != "Task on hold" || body != `"News" is on hold because the Free plan does not allow it. It resumes by itself when the plan allows it again.` {
		t.Fatalf("%q / %q", title, body)
	}
}

func TestLoadReconcilerConfig(t *testing.T) {
	t.Setenv("TASK_RECONCILER_MODE", "enforced") // a typo
	t.Setenv("TASK_RECONCILER_INTERVAL_MINUTES", "0")
	t.Setenv("TASK_INACTIVE_DAYS", "")
	t.Setenv("TASK_PLAN_ENDED_GRACE_HOURS", "0")
	t.Setenv("TASK_PAUSE_NOTIFICATIONS", "true")
	cfg := LoadReconcilerConfig()
	want := ReconcilerConfig{Mode: ReconcilerDryRun, Interval: time.Hour, InactiveAfter: 90 * oneDay, PlanEndedGrace: 0, PauseNotifications: true}
	if cfg != want {
		t.Fatalf("config %+v, want %+v", cfg, want)
	}
}

// Every change holds the task's lock, like an owner's pause, resume or delete.
func TestReconcilerChangesTasksUnderTheirLock(t *testing.T) {
	w := newReconWorld()
	w.owner("idle", 100*oneDay, tiers.TierFree)
	w.owner("u", oneDay, tiers.TierFree)
	w.task("idle-t", "idle", "recurring", "0 9 * * *", TaskStatusActive, 200*oneDay, &fakeSchedule{})
	w.task("gone", "u", "recurring", "0 9 * * *", TaskStatusActive, 30*oneDay, nil)
	w.task("ran", "u", "one_time", "0 9 21 11 *", TaskStatusActive, 3*oneDay, nil)
	w.task("frequent", "u", "recurring", "*/10 * * * *", TaskStatusActive, 60*oneDay, &fakeSchedule{})
	w.schedules["orphan"] = &fakeSchedule{workflow: scheduledTaskWorkflow}
	w.sweep(t, ReconcilerEnforce)
	if len(w.events) < 5 || len(w.outsideLock) != 0 {
		t.Fatalf("changes %q; outside a task lock %q", w.events, w.outsideLock)
	}
}
