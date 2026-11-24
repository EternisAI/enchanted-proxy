package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/sdk/client"
)

// The task reconciler keeps task rows in line with their Temporal schedules
// and with their owners' accounts and plans. Every hour it:
//
//   - records on the row a pause or resume made in Temporal, taking the reason
//     from the first word of the schedule's pause note;
//   - marks one-time tasks whose run has passed as completed;
//   - deletes rows whose schedule no longer exists, releases pending tasks
//     whose schedule was never created, and deletes schedules that have no row;
//   - pauses the tasks of owners who no longer exist or are disabled
//     (account_missing), and the recurring tasks of owners inactive for the
//     configured number of days (inactive);
//   - holds recurring tasks that do not fit the owner's current plan once a
//     grace period has passed (plan_ended), and lifts holds that no longer
//     apply. This is the only pause it lifts by itself.
//
// It runs on every server instance. A Postgres advisory lock and the record
// of the last sweep keep it to one sweep per interval across instances. In
// dry-run mode it reports what it would do and changes nothing.

// ReconcilerMode switches the reconciler off, to reporting only, or on.
type ReconcilerMode string

const (
	ReconcilerOff     ReconcilerMode = "off"
	ReconcilerDryRun  ReconcilerMode = "dry-run"
	ReconcilerEnforce ReconcilerMode = "enforce"
)

// ReconcilerConfig holds the reconciler's settings.
type ReconcilerConfig struct {
	Mode           ReconcilerMode
	Interval       time.Duration
	InactiveAfter  time.Duration // owners inactive this long have their recurring tasks paused
	PlanEndedGrace time.Duration // how long a task may stay over its owner's plan before it is held
	// PauseNotifications sends the owner a push when a task is paused as
	// inactive or held. Leave it off for a sweep expected to pause many tasks.
	PauseNotifications bool
}

// LoadReconcilerConfig reads the settings from the environment. An unknown
// mode gives dry-run, so a typo cannot switch enforcement on.
func LoadReconcilerConfig() ReconcilerConfig {
	mode := ReconcilerMode(strings.ToLower(strings.TrimSpace(os.Getenv("TASK_RECONCILER_MODE"))))
	switch mode {
	case ReconcilerOff, ReconcilerDryRun, ReconcilerEnforce:
	default:
		mode = ReconcilerDryRun
	}
	notify, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("TASK_PAUSE_NOTIFICATIONS")))
	return ReconcilerConfig{
		Mode:               mode,
		Interval:           time.Duration(envAtLeast("TASK_RECONCILER_INTERVAL_MINUTES", 60, 1)) * time.Minute,
		InactiveAfter:      time.Duration(envAtLeast("TASK_INACTIVE_DAYS", 90, 1)) * 24 * time.Hour,
		PlanEndedGrace:     time.Duration(envAtLeast("TASK_PLAN_ENDED_GRACE_HOURS", 168, 0)) * time.Hour,
		PauseNotifications: notify,
	}
}

// envAtLeast reads a whole number of at least least; anything else gives def.
func envAtLeast(name string, def, least int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && v >= least {
		return v
	}
	return def
}

const (
	// scheduledTaskWorkflow is the workflow every task schedule starts. Schedules
	// of other workflows in the namespace are not the reconciler's.
	scheduledTaskWorkflow = "ScheduledTaskWorkflow"
	// pendingMargin is how long a task may stay pending before the reconciler
	// settles it; creating its schedule takes seconds.
	pendingMargin   = time.Hour
	firstSweepDelay = 2 * time.Minute
	sweepTimeout    = 15 * time.Minute
	// A sweep that finds most owners missing, or most schedules without a row,
	// is more likely looking at the wrong Firebase project or a Temporal
	// namespace shared with another database than at real data. Above these
	// shares (with at least the given count) the rule is skipped and the sweep
	// reports an error.
	maxMissingOwnersShare = 0.5
	minMissingOwners      = 5
	maxOrphanShare        = 0.5
	minOrphans            = 10
)

// PauseNotifier tells an owner that tasks were paused. The notification
// service implements it.
type PauseNotifier interface {
	SendTaskPausedNotification(ctx context.Context, userID, chatID, title, body string) error
}

// Reconciler runs the sweeps.
type Reconciler struct {
	s        *Service
	gate     sweepGate
	accounts AccountLookup // nil: account rules are skipped
	notifier PauseNotifier // nil: no pushes
	cfg      ReconcilerConfig
	log      *logger.Logger
}

// NewReconciler builds the reconciler over the task service's Temporal client,
// queries, plan lookup and quota groups. It refuses a connection pool too small
// for its sweeps.
func NewReconciler(s *Service, db *sql.DB, accounts AccountLookup, notifier PauseNotifier, cfg ReconcilerConfig, log *logger.Logger) (*Reconciler, error) {
	if n := db.Stats().MaxOpenConnections; cfg.Mode != ReconcilerOff && n > 0 && n < minReconcilerPool {
		return nil, fmt.Errorf("the task reconciler needs a database pool of at least %d connections (DB_MAX_OPEN_CONNS), not %d", minReconcilerPool, n)
	}
	return &Reconciler{s: s, gate: lockedSweeps(db), accounts: accounts, notifier: notifier, cfg: cfg, log: log}, nil
}

// minReconcilerPool is the smallest connection pool the reconciler works with:
// a sweep keeps one connection for the reconciler's lock while its steps hold
// a task's lock and do their work.
const minReconcilerPool = minTaskLockPool + 1

// sweepGate runs fn when this instance holds the reconciler's lock and no
// sweep started within minGap, and records the sweep and its report.
type sweepGate func(ctx context.Context, mode ReconcilerMode, minGap time.Duration, fn func(ctx context.Context) *Report) (ran bool, err error)

// lockedSweeps is the sweepGate backed by a Postgres advisory lock, held in a
// transaction for the length of the sweep. The sweep's own writes are not in
// that transaction.
func lockedSweeps(db *sql.DB) sweepGate {
	return func(ctx context.Context, mode ReconcilerMode, minGap time.Duration, fn func(ctx context.Context) *Report) (bool, error) {
		// database/sql rolls a transaction back when its context ends, so the
		// transaction outlives the sweep's deadline long enough to record it.
		txCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepTimeout+cleanupTimeout)
		defer cancel()
		tx, err := db.BeginTx(txCtx, nil)
		if err != nil {
			return false, fmt.Errorf("failed to begin reconciler transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		q := pgdb.New(tx)
		locked, err := q.TryLockTaskReconciler(txCtx)
		if err != nil || !locked {
			return false, err
		}
		recent, err := q.RecentTaskReconcilerRun(txCtx, minGap.Seconds())
		if err != nil || recent {
			return false, err
		}
		id, err := q.StartTaskReconcilerRun(txCtx, string(mode))
		if err != nil {
			return false, fmt.Errorf("failed to record reconciler run: %w", err)
		}
		report := fn(ctx)
		body, err := json.Marshal(report)
		if err != nil {
			return true, fmt.Errorf("failed to encode reconciler report: %w", err)
		}
		text := string(body)
		if err := q.FinishTaskReconcilerRun(txCtx, pgdb.FinishTaskReconcilerRunParams{ID: id, Report: &text}); err != nil {
			return true, fmt.Errorf("failed to record reconciler report: %w", err)
		}
		return true, tx.Commit()
	}
}

// Run sweeps after a short delay and then every interval, until ctx ends.
func (r *Reconciler) Run(ctx context.Context) {
	if r.cfg.Mode == ReconcilerOff {
		r.log.Info("task reconciler is off")
		return
	}
	r.log.Info("task reconciler started",
		slog.String("mode", string(r.cfg.Mode)),
		slog.Duration("interval", r.cfg.Interval),
		slog.Duration("inactive_after", r.cfg.InactiveAfter),
		slog.Duration("plan_ended_grace", r.cfg.PlanEndedGrace),
		slog.Bool("pause_notifications", r.cfg.PauseNotifications),
		slog.Bool("accounts", r.accounts != nil))
	timer := time.NewTimer(firstSweepDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		r.RunOnce(ctx)
		timer.Reset(r.cfg.Interval)
	}
}

// RunOnce makes one sweep unless another instance made one within the interval.
func (r *Reconciler) RunOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	// A little under the interval, so that a timer firing early on one instance
	// does not skip a whole interval.
	minGap := r.cfg.Interval * 9 / 10
	ran, err := r.gate(ctx, r.cfg.Mode, minGap, func(ctx context.Context) *Report {
		report := r.sweep(ctx)
		r.logReport(report)
		return report
	})
	if err != nil {
		r.log.Error("task reconciler sweep failed", slog.String("error", err.Error()))
	} else if !ran {
		r.log.Debug("task reconciler sweep skipped: another instance holds the lock or swept recently")
	}
}

// Report says what a sweep did, or in dry-run mode would do. Actions and
// Skipped map an action label, such as "pause:inactive", to task IDs.
type Report struct {
	Mode          ReconcilerMode      `json:"mode"`
	Rows          int                 `json:"rows"`
	Schedules     int                 `json:"schedules"`
	Owners        int                 `json:"owners"`
	AccountsKnown bool                `json:"accounts_known"`
	Actions       map[string][]string `json:"actions"`
	// Skipped actions found the state changed when they came to run.
	Skipped       map[string][]string `json:"skipped,omitempty"`
	Errors        []string            `json:"errors,omitempty"`
	Notifications int                 `json:"notifications"`
}

func (rep *Report) add(label, taskID string) {
	rep.Actions[label] = append(rep.Actions[label], taskID)
}

func (rep *Report) skip(label, taskID string) {
	if rep.Skipped == nil {
		rep.Skipped = map[string][]string{}
	}
	rep.Skipped[label] = append(rep.Skipped[label], taskID)
}

func (rep *Report) fail(format string, args ...any) {
	rep.Errors = append(rep.Errors, fmt.Sprintf(format, args...))
}

func (r *Reconciler) logReport(rep *Report) {
	counts := make([]string, 0, len(rep.Actions))
	for label, ids := range rep.Actions {
		counts = append(counts, fmt.Sprintf("%s=%d", label, len(ids)))
	}
	slices.Sort(counts)
	r.log.Info("task reconciler sweep finished",
		slog.String("mode", string(rep.Mode)),
		slog.Int("rows", rep.Rows),
		slog.Int("schedules", rep.Schedules),
		slog.Int("owners", rep.Owners),
		slog.Bool("accounts_known", rep.AccountsKnown),
		slog.String("actions", strings.Join(counts, " ")),
		slog.Int("skipped", countIDs(rep.Skipped)),
		slog.Int("errors", len(rep.Errors)),
		slog.Int("notifications", rep.Notifications))
	for _, e := range rep.Errors {
		r.log.Error("task reconciler: " + e)
	}
}

func countIDs(m map[string][]string) int {
	n := 0
	for _, ids := range m {
		n += len(ids)
	}
	return n
}

// scheduleState is what the reconciler reads from a schedule.
type scheduleState struct {
	paused bool
	note   string
}

// snapshot is the state one sweep works from. Schedules are listed before
// rows: a task's row is written before its schedule is created, so every
// listed schedule whose task exists has its row in the snapshot.
type snapshot struct {
	now       time.Time
	rows      []pgdb.Task
	schedules map[string]scheduleState
	accounts  map[string]Account // nil when the accounts are unknown
}

func (r *Reconciler) sweep(ctx context.Context) *Report {
	rep := &Report{Mode: r.cfg.Mode, Actions: map[string][]string{}}
	schedules, err := r.listSchedules(ctx)
	if err != nil {
		rep.fail("listing schedules: %v", err)
		return rep
	}
	rows, err := r.s.queries.ListTasksToReconcile(ctx)
	if err != nil {
		rep.fail("listing tasks: %v", err)
		return rep
	}
	snap := snapshot{now: r.s.currentTime(), rows: rows, schedules: schedules}
	rep.Rows, rep.Schedules = len(rows), len(schedules)

	owners := ownersOf(rows)
	rep.Owners = len(owners)
	if r.accounts != nil && len(owners) > 0 {
		accounts, err := r.accounts.LookupAccounts(ctx, owners)
		switch {
		case err != nil:
			rep.fail("looking up accounts, account rules skipped: %v", err)
		case tooMany(countMissing(accounts), len(owners), maxMissingOwnersShare, minMissingOwners):
			rep.fail("%d of %d owners have no account; account rules skipped. Check the Firebase project", countMissing(accounts), len(owners))
		default:
			snap.accounts = accounts
		}
	}
	rep.AccountsKnown = snap.accounts != nil

	actions := r.plan(ctx, snap, rep)
	if orphans := countKind(actions, actDeleteSchedule); tooMany(orphans, len(schedules), maxOrphanShare, minOrphans) {
		rep.fail("%d of %d schedules have no task row; not deleting them. Is the Temporal namespace shared with another database?", orphans, len(schedules))
		actions = slices.DeleteFunc(actions, func(a action) bool { return a.kind == actDeleteSchedule })
	}
	r.execute(ctx, actions, rep)
	return rep
}

func tooMany(n, of int, share float64, least int) bool {
	return n >= least && float64(n) > share*float64(of)
}

func ownersOf(rows []pgdb.Task) []string {
	seen := map[string]bool{}
	var owners []string
	for _, row := range rows {
		if !seen[row.UserID] {
			seen[row.UserID] = true
			owners = append(owners, row.UserID)
		}
	}
	slices.Sort(owners)
	return owners
}

func countMissing(accounts map[string]Account) int {
	n := 0
	for _, a := range accounts {
		if !a.Exists {
			n++
		}
	}
	return n
}

// listSchedules lists the namespace's task schedules.
func (r *Reconciler) listSchedules(ctx context.Context) (map[string]scheduleState, error) {
	iter, err := r.s.temporalClient.ScheduleClient().List(ctx, client.ScheduleListOptions{PageSize: 1000})
	if err != nil {
		return nil, err
	}
	schedules := map[string]scheduleState{}
	for iter.HasNext() {
		entry, err := iter.Next()
		if err != nil {
			return nil, err
		}
		if entry.WorkflowType.Name != scheduledTaskWorkflow {
			continue
		}
		schedules[entry.ID] = scheduleState{paused: entry.Paused, note: entry.Note}
	}
	return schedules, nil
}

type actionKind string

const (
	actMarkPaused     actionKind = "mark_paused"     // the row follows a schedule paused in Temporal
	actUpdateReason   actionKind = "update_reason"   // the row takes the reason of the schedule's pause note
	actMarkResumed    actionKind = "mark_resumed"    // the row follows a schedule running in Temporal
	actActivate       actionKind = "activate"        // a pending row whose schedule exists
	actComplete       actionKind = "complete"        // a one-time task whose run has passed
	actDeleteRow      actionKind = "delete_row"      // a row whose schedule no longer exists
	actReleasePending actionKind = "release_pending" // a pending row whose schedule was never created
	actDeleteSchedule actionKind = "delete_schedule" // a schedule with no row
	actPause          actionKind = "pause"           // pause the schedule and the row
	actLiftHold       actionKind = "lift_hold"       // a plan_ended hold that no longer applies
	actMarkOver       actionKind = "mark_over"       // first found over the owner's plan
	actClearOver      actionKind = "clear_over"      // fits the owner's plan again
)

type action struct {
	kind   actionKind
	reason PauseReason // for pauses and reason updates
	row    pgdb.Task   // empty for delete_schedule
	taskID string
	plan   string // the plan's display name, for a plan_ended hold's push
}

func (a action) label() string {
	if a.reason != "" {
		return string(a.kind) + ":" + string(a.reason)
	}
	return string(a.kind)
}

func countKind(actions []action, kind actionKind) int {
	n := 0
	for _, a := range actions {
		if a.kind == kind {
			n++
		}
	}
	return n
}

// holdCandidate is a recurring task the plan_ended rule looks at: one that is
// running, or one held already.
type holdCandidate struct {
	row  pgdb.Task
	held bool
}

// plan decides what to do, from the snapshot alone, apart from the plan and
// quota group lookups of the plan_ended rule.
func (r *Reconciler) plan(ctx context.Context, snap snapshot, rep *Report) []action {
	var actions []action
	add := func(kind actionKind, reason PauseReason, row pgdb.Task) {
		actions = append(actions, action{kind: kind, reason: reason, row: row, taskID: row.TaskID})
	}

	rowIDs := make(map[string]bool, len(snap.rows))
	for _, row := range snap.rows {
		rowIDs[row.TaskID] = true
	}
	orphans := make([]string, 0)
	for id := range snap.schedules {
		if !rowIDs[id] {
			orphans = append(orphans, id)
		}
	}
	slices.Sort(orphans)
	for _, id := range orphans {
		actions = append(actions, action{kind: actDeleteSchedule, taskID: id})
	}

	candidates := map[string][]holdCandidate{} // by owner
	for _, row := range snap.rows {
		status := TaskStatus(row.Status)
		sched, scheduled := snap.schedules[row.TaskID]
		if !scheduled {
			if kind := planUnscheduled(row, snap.now); kind != "" {
				add(kind, "", row)
			}
			continue
		}

		noteReason := pauseReasonFromNote(sched.note)
		switch {
		case status == TaskStatusPending:
			// Its creation did not finish, or is finishing now.
			if snap.now.Sub(row.CreatedAt) >= pendingMargin {
				add(actActivate, "", row)
			}
			continue
		case sched.paused && status == TaskStatusActive:
			add(actMarkPaused, noteReason, row)
			continue
		case sched.paused && status == TaskStatusPaused && rowReason(row) != noteReason:
			add(actUpdateReason, noteReason, row)
			continue
		case !sched.paused && status == TaskStatusPaused:
			add(actMarkResumed, "", row)
			continue
		}

		account, known := snap.accounts[row.UserID]
		missing := known && (!account.Exists || account.Disabled)
		inactive := known && account.Exists && !account.LastActive.IsZero() && snap.now.Sub(account.LastActive) >= r.cfg.InactiveAfter

		switch {
		case status == TaskStatusActive && row.Type == string(TaskTypeOneTime):
			if oneTimeRunPassed(row, snap.now) {
				add(actComplete, "", row)
			} else if missing {
				add(actPause, PauseAccountMissing, row)
			}
		case status == TaskStatusActive:
			switch {
			case missing:
				add(actPause, PauseAccountMissing, row)
			case inactive:
				add(actPause, PauseInactive, row)
			default:
				candidates[row.UserID] = append(candidates[row.UserID], holdCandidate{row: row})
			}
		case status == TaskStatusPaused && rowReason(row) == PausePlanEnded && row.Type == string(TaskTypeRecurring):
			// A held task joins the walk in its place among the owner's tasks, so
			// the hold lifts once the plan allows the task again. The holds of an
			// owner who is missing or inactive stay.
			if !missing && !inactive {
				candidates[row.UserID] = append(candidates[row.UserID], holdCandidate{row: row, held: true})
			}
		}
	}
	return append(actions, r.planHolds(ctx, snap, candidates, rep)...)
}

// planUnscheduled decides about a row whose schedule is not listed.
func planUnscheduled(row pgdb.Task, now time.Time) actionKind {
	switch {
	case row.Status == string(TaskStatusPending):
		// Temporal may still create the schedule for a while after a failed call.
		if now.Sub(row.CreatedAt) >= pendingMargin {
			return actReleasePending
		}
		return ""
	case row.Type == string(TaskTypeOneTime) && row.Status == string(TaskStatusActive) && oneTimeRunPassed(row, now):
		// Temporal removes a one-time schedule some time after its run. Marking
		// the row completed keeps the task's history.
		return actComplete
	default:
		// A recurring task, or a one-time task whose run is ahead, cannot run
		// without a schedule; a paused one-time task whose run passed never will.
		return actDeleteRow
	}
}

// oneTimeRunPassed reports whether a one-time task's run time has passed. A
// task whose time cannot be read has no run ahead of it.
func oneTimeRunPassed(row pgdb.Task, now time.Time) bool {
	schedule, err := parseStoredSchedule(row.Time)
	if err != nil {
		return false
	}
	next := schedule.Next(row.CreatedAt)
	return !next.IsZero() && !next.After(now)
}

// planHolds applies the plan_ended rule to each quota group: its running and
// held recurring tasks are walked oldest first against the plan's limits. A
// task is over the plan when it is past the plan's task count, runs more often
// than the plan's floor, or would take the group past the plan's runs per day.
func (r *Reconciler) planHolds(ctx context.Context, snap snapshot, byOwner map[string][]holdCandidate, rep *Report) []action {
	type group struct {
		owner      string // whose plan applies
		candidates []holdCandidate
	}
	groups := map[string]*group{}
	var keys []string
	owners := make([]string, 0, len(byOwner))
	for owner := range byOwner {
		owners = append(owners, owner)
	}
	slices.Sort(owners)
	for _, owner := range owners {
		members, err := r.s.groups.UserIDs(ctx, owner)
		if err != nil {
			rep.fail("looking up the quota group of %s: %v", owner, err)
			continue
		}
		members = slices.Clone(members)
		slices.Sort(members)
		key := strings.Join(members, ",")
		g, ok := groups[key]
		if !ok {
			g = &group{owner: owner}
			groups[key] = g
			keys = append(keys, key)
		}
		g.candidates = append(g.candidates, byOwner[owner]...)
	}

	var actions []action
	for _, key := range keys {
		g := groups[key]
		tier, _, err := r.s.tierLookup.GetUserTierConfig(ctx, g.owner)
		if err != nil {
			rep.fail("looking up the plan of %s: %v", g.owner, err)
			continue
		}
		slices.SortFunc(g.candidates, func(a, b holdCandidate) int {
			if c := a.row.CreatedAt.Compare(b.row.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(a.row.TaskID, b.row.TaskID)
		})
		actions = append(actions, r.walkGroup(snap.now, tier, g.candidates)...)
	}
	return actions
}

func (r *Reconciler) walkGroup(now time.Time, tier tiers.Config, candidates []holdCandidate) []action {
	var actions []action
	add := func(kind actionKind, reason PauseReason, row pgdb.Task) {
		actions = append(actions, action{kind: kind, reason: reason, row: row, taskID: row.TaskID, plan: tier.DisplayName})
	}
	load := map[int64]int{} // runs per UTC day of the tasks that fit so far
	fitting := 0
	for _, c := range candidates {
		schedule, err := parseStoredSchedule(c.row.Time)
		if err != nil {
			continue // its runs cannot be counted; creation refuses new tasks beside it
		}
		profile := profileSchedule(schedule, now)
		over := fitting >= tier.TaskMaxActiveRecurring
		if gap := minInterval(schedule, now); gap > 0 && gap < tier.TaskMinInterval {
			over = true
		}
		for _, day := range profile.days {
			if load[day.Unix()]+len(profile.timesOfDay) > tier.TaskMaxRunsPerDay {
				over = true
				break
			}
		}
		if !over {
			fitting++
			for _, day := range profile.days {
				load[day.Unix()] += len(profile.timesOfDay)
			}
		}

		since := c.row.OverLimitSince
		switch {
		case c.held && !over:
			add(actLiftHold, PausePlanEnded, c.row)
		case c.held:
			// Stays on hold.
		case over && !since.Valid:
			add(actMarkOver, "", c.row)
		case over && now.Sub(since.Time) >= r.cfg.PlanEndedGrace:
			add(actPause, PausePlanEnded, c.row)
		case over:
			// Within the grace period.
		case since.Valid:
			add(actClearOver, "", c.row)
		}
	}
	return actions
}

// execute carries out the actions in enforce mode. In dry-run mode it makes
// only the reads that decide whether an action would still apply.
func (r *Reconciler) execute(ctx context.Context, actions []action, rep *Report) {
	enforce := r.cfg.Mode == ReconcilerEnforce
	type noticeKey struct {
		owner  string
		reason PauseReason
	}
	notices := map[noticeKey][]action{}
	var noticeOrder []noticeKey
	for i, a := range actions {
		if err := ctx.Err(); err != nil {
			// The server is shutting down, or the sweep ran out of time.
			rep.fail("sweep stopped with %d actions left: %v", len(actions)-i, err)
			break
		}
		label := a.label()
		applied, err := r.applyOne(ctx, a, enforce)
		attrs := []any{
			slog.String("mode", string(r.cfg.Mode)),
			slog.String("action", label),
			slog.String("task_id", a.taskID),
		}
		if a.row.UserID != "" {
			attrs = append(attrs, slog.String("user_id", a.row.UserID))
		}
		switch {
		case err != nil:
			rep.fail("%s %s: %v", label, a.taskID, err)
		case !applied:
			rep.skip(label, a.taskID)
			r.log.Info("task reconciler: skipped, state changed", attrs...)
		default:
			rep.add(label, a.taskID)
			if enforce {
				r.log.Info("task reconciler: applied", attrs...)
			} else {
				r.log.Info("task reconciler: would apply", attrs...)
			}
			if a.kind == actPause && (a.reason == PauseInactive || a.reason == PausePlanEnded) {
				key := noticeKey{owner: a.row.UserID, reason: a.reason}
				if _, ok := notices[key]; !ok {
					noticeOrder = append(noticeOrder, key)
				}
				notices[key] = append(notices[key], a)
			}
		}
	}
	if !enforce || !r.cfg.PauseNotifications || r.notifier == nil || ctx.Err() != nil {
		return
	}
	for _, key := range noticeOrder {
		title, body := pauseNotice(key.reason, notices[key], r.cfg.InactiveAfter)
		first := notices[key][0].row
		if err := r.notifier.SendTaskPausedNotification(ctx, key.owner, first.ChatID, title, body); err != nil {
			rep.fail("notifying %s: %v", key.owner, err)
			continue
		}
		rep.Notifications++
	}
}

// applyOne runs one action. In enforce mode it holds the task's lock, the one
// creation, pause, resume and deletion hold, so the check and the change
// cannot interleave with an owner's change to the same task.
func (r *Reconciler) applyOne(ctx context.Context, a action, enforce bool) (bool, error) {
	if !enforce {
		stepCtx, cancel := context.WithTimeout(ctx, scheduleSetupTimeout)
		defer cancel()
		return r.apply(stepCtx, a, false)
	}
	var applied bool
	err := r.s.withTaskLock(ctx, a.taskID, func() error {
		// The step's time starts once the lock is held.
		stepCtx, cancel := context.WithTimeout(ctx, scheduleSetupTimeout)
		defer cancel()
		var err error
		applied, err = r.apply(stepCtx, a, true)
		return err
	})
	return applied, err
}

// apply carries out one action, after checking with Temporal or the database
// that it still applies. It reports false when the state changed since the
// snapshot. With enforce false it only checks.
func (r *Reconciler) apply(ctx context.Context, a action, enforce bool) (bool, error) {
	q := r.s.queries
	handle := func() client.ScheduleHandle { return r.s.temporalClient.ScheduleClient().GetHandle(ctx, a.taskID) }
	// describe reads the schedule's state: nil when it does not exist.
	describe := func() (*client.ScheduleState, error) { return r.s.scheduleState(ctx, a.taskID) }
	changed := func(n int64, err error) (bool, error) { return err == nil && n > 0, err }

	switch a.kind {
	case actDeleteSchedule:
		if _, err := q.GetTaskByID(ctx, a.taskID); !errors.Is(err, sql.ErrNoRows) {
			return false, err // the row exists after all, or the read failed
		}
		if !enforce {
			return true, nil
		}
		return true, r.s.deleteSchedule(ctx, a.taskID)

	case actDeleteRow, actReleasePending:
		state, err := describe()
		if err != nil || state != nil {
			return false, err
		}
		if !enforce {
			return true, nil
		}
		if a.kind == actReleasePending {
			// The attempt stops counting toward the owner's daily creations.
			if err := q.DeleteTaskCreation(ctx, a.taskID); err != nil {
				return false, err
			}
		}
		return changed(q.DeleteTaskByID(ctx, a.taskID))

	case actMarkPaused, actUpdateReason, actMarkResumed:
		state, err := describe()
		if err != nil || state == nil {
			return false, err
		}
		if state.Paused != (a.kind != actMarkResumed) {
			return false, nil
		}
		if a.kind != actMarkResumed {
			a.reason = pauseReasonFromNote(state.Note)
		}
		if !enforce {
			return true, nil
		}
		reason := string(a.reason)
		switch a.kind {
		case actMarkPaused:
			// The schedule may read paused while a resume whose call failed can
			// still reach Temporal, as after a failed resume, whose row stays
			// active. The pause is made again with its own note before the row
			// stops counting the task: a resume applies only to the schedule as it
			// was read (resumeAsRead), so this pause voids it.
			if err := handle().Pause(ctx, client.SchedulePauseOptions{Note: state.Note}); err != nil {
				return false, err
			}
			return changed(q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: a.taskID, PauseReason: &reason}))
		case actUpdateReason:
			return changed(q.UpdateTaskPauseReason(ctx, pgdb.UpdateTaskPauseReasonParams{TaskID: a.taskID, PauseReason: &reason}))
		default:
			return changed(q.MarkTaskResumed(ctx, pgdb.MarkTaskResumedParams{TaskID: a.taskID, PauseReason: a.row.PauseReason}))
		}

	case actPause:
		state, err := describe()
		if err != nil || state == nil || state.Paused {
			// Gone, or paused meanwhile: a pause already made keeps its note.
			return false, err
		}
		if !enforce {
			return true, nil
		}
		if err := handle().Pause(ctx, client.SchedulePauseOptions{Note: pauseNote(a.reason, r.pauseDetail(a.reason))}); err != nil {
			return false, err
		}
		reason := string(a.reason)
		if _, err := q.MarkTaskPaused(ctx, pgdb.MarkTaskPausedParams{TaskID: a.taskID, PauseReason: &reason}); err != nil {
			// The schedule is paused; the next sweep records it from the note.
			return true, err
		}
		return true, nil

	case actLiftHold:
		read, err := r.s.readForResume(ctx, a.taskID)
		if err != nil || read == nil || !read.paused() || pauseReasonFromNote(read.note()) != PausePlanEnded {
			return false, err
		}
		if !enforce {
			return true, nil
		}
		// The row counts the task before its schedule resumes, and keeps counting
		// it after a failed resume, which may still apply. If the schedule stays
		// paused, the next sweep puts the hold back on the row, and the pause it
		// makes again voids the resume.
		held := string(PausePlanEnded)
		if resumed, err := changed(q.MarkTaskResumed(ctx, pgdb.MarkTaskResumedParams{TaskID: a.taskID, PauseReason: &held})); !resumed {
			return false, err // the row changed since the check, or the write failed
		}
		if err := r.s.resumeAsRead(ctx, a.taskID, read, "plan_ended hold lifted by the task reconciler"); err != nil {
			return true, err
		}
		return true, q.ClearTaskOverLimit(ctx, a.taskID)

	case actActivate:
		if !enforce {
			return true, nil
		}
		return changed(q.ActivatePendingTask(ctx, a.taskID))

	case actComplete:
		if !enforce {
			return true, nil
		}
		return changed(q.CompleteOneTimeTask(ctx, a.taskID))

	case actMarkOver:
		if !enforce {
			return true, nil
		}
		return true, q.MarkTaskOverLimit(ctx, pgdb.MarkTaskOverLimitParams{TaskID: a.taskID, Since: sql.NullTime{Time: r.s.currentTime(), Valid: true}})

	case actClearOver:
		if !enforce {
			return true, nil
		}
		return true, q.ClearTaskOverLimit(ctx, a.taskID)
	}
	return false, fmt.Errorf("unknown action %q", a.kind)
}

func (r *Reconciler) pauseDetail(reason PauseReason) string {
	switch reason {
	case PauseAccountMissing:
		return "owner not found or disabled (task reconciler)"
	case PauseInactive:
		return fmt.Sprintf("owner inactive for %d days (task reconciler)", int(r.cfg.InactiveAfter/(24*time.Hour)))
	case PausePlanEnded:
		return "over the limits of the owner's plan (task reconciler)"
	default:
		return "task reconciler"
	}
}

// pauseNotice is the push for an owner's tasks paused in one sweep for one reason.
func pauseNotice(reason PauseReason, actions []action, inactiveAfter time.Duration) (title, body string) {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, fmt.Sprintf("%q", a.row.TaskName))
	}
	var subject string
	switch len(names) {
	case 1:
		subject = names[0]
	case 2:
		subject = names[0] + " and " + names[1]
	default:
		subject = fmt.Sprintf("%s, %s and %d more", names[0], names[1], len(names)-2)
	}
	one := len(names) == 1
	if reason == PausePlanEnded {
		if one {
			return "Task on hold", fmt.Sprintf("%s is on hold because the %s plan does not allow it. It resumes by itself when the plan allows it again.", subject, actions[0].plan)
		}
		return "Tasks on hold", fmt.Sprintf("%s are on hold because the %s plan does not allow them. They resume by themselves when the plan allows them again.", subject, actions[0].plan)
	}
	days := int(inactiveAfter / (24 * time.Hour))
	if one {
		return "Task paused", fmt.Sprintf("%s was paused because the app was not opened for %d days. Ask in chat to resume it.", subject, days)
	}
	return "Tasks paused", fmt.Sprintf("%s were paused because the app was not opened for %d days. Ask in chat to resume them.", subject, days)
}
