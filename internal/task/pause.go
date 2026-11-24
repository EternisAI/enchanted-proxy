package task

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/sdk/client"
)

// PauseReason says why a task is paused. It is stored on the task row and
// written as the first word of its Temporal schedule's pause note, so pauses
// made outside the proxy (with the Temporal CLI) carry a reason too.
type PauseReason string

const (
	PauseInactive       PauseReason = "inactive"        // the owner has not used the app for a long time
	PauseAccountMissing PauseReason = "account_missing" // the owner no longer exists or is disabled
	PausePlanEnded      PauseReason = "plan_ended"      // over the limits of the owner's current plan
	PauseOverAllowance  PauseReason = "over_allowance"
	PauseUser           PauseReason = "user"  // the owner paused it
	PauseAdmin          PauseReason = "admin" // paused by the service; the owner cannot resume it
)

var knownPauseReasons = map[PauseReason]bool{
	PauseInactive: true, PauseAccountMissing: true, PausePlanEnded: true,
	PauseOverAllowance: true, PauseUser: true, PauseAdmin: true,
}

// pauseNote is the note a schedule is paused with: the reason, a colon and a
// few words for whoever reads the schedule in Temporal.
func pauseNote(reason PauseReason, detail string) string {
	return string(reason) + ": " + detail
}

// pauseReasonFromNote reads the reason from a schedule's pause note: its first
// word, without a trailing colon. A pause whose note names no known reason was
// made by someone with access to Temporal, so it counts as an admin pause.
func pauseReasonFromNote(note string) PauseReason {
	fields := strings.Fields(note)
	if len(fields) == 0 {
		return PauseAdmin
	}
	reason := PauseReason(strings.ToLower(strings.TrimSuffix(fields[0], ":")))
	if !knownPauseReasons[reason] {
		return PauseAdmin
	}
	return reason
}

// ResumableByOwner reports whether the owner may resume a task paused for
// this reason. An admin pause is final, and a plan_ended hold lifts by itself
// when the plan allows the task again.
func ResumableByOwner(reason PauseReason) bool {
	return reason != PauseAdmin && reason != PausePlanEnded && knownPauseReasons[reason]
}

// PauseExplanation is a sentence about a paused task that the model can pass
// on to the user.
func PauseExplanation(reason PauseReason) string {
	switch reason {
	case PauseInactive:
		return "This task was paused because the account was not used for a long time. Ask me to resume it."
	case PauseAccountMissing:
		return "This task was paused because its account was unavailable. Ask me to resume it."
	case PausePlanEnded:
		return "This task is on hold because it does not fit the limits of the current plan. It resumes by itself when the plan allows it again; otherwise delete it and create one with a less frequent schedule."
	case PauseOverAllowance:
		return "This task was paused because it went over the plan's allowance. Ask me to resume it."
	case PauseUser:
		return "This task was paused at the user's request. Ask me to resume it."
	default:
		return "This task was paused by the service and cannot be resumed."
	}
}

func taskFromRow(row pgdb.Task) *Task {
	t := &Task{
		TaskID:    row.TaskID,
		UserID:    row.UserID,
		ChatID:    row.ChatID,
		TaskName:  row.TaskName,
		TaskText:  row.TaskText,
		Type:      row.Type,
		Time:      row.Time,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	if row.Status == string(TaskStatusPaused) && row.PauseReason != nil {
		t.PauseReason = *row.PauseReason
	}
	return t
}

// errScheduleGone answers a pause or resume of a task whose schedule no longer
// exists. The reconciler removes such rows.
var errScheduleGone = &ValidationError{Message: "This task is no longer scheduled. Delete it and create it again."}

// PauseTask pauses one of the caller's tasks until the caller resumes it. A
// task that is already paused keeps its reason.
func (s *Service) PauseTask(ctx context.Context, userID, taskID string) (*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	// The steps do not stop when the client disconnects, and run under the
	// task's lock, so a pause cannot interleave with a resume or a deletion of
	// the same task.
	opCtx, cancel := detached(ctx, scheduleSetupTimeout)
	defer cancel()
	var paused *Task
	err := s.withTaskLock(opCtx, taskID, func() error {
		row, err := getOwnedTask(opCtx, s.queries, userID, taskID)
		if err != nil {
			return err
		}
		switch TaskStatus(row.Status) {
		case TaskStatusPaused:
			paused = taskFromRow(row)
			return nil
		case TaskStatusActive:
		case TaskStatusPending:
			return &ValidationError{Message: "The task is still being set up. Try again in a moment."}
		default:
			return &ValidationError{Message: "This task has finished and cannot be paused."}
		}

		// The schedule is changed first and the row follows it.
		state, err := s.scheduleState(opCtx, taskID)
		if err != nil {
			return fmt.Errorf("failed to read schedule: %w", err)
		}
		if state == nil {
			return errScheduleGone
		}
		reason := PauseUser
		if state.Paused {
			// Already paused in Temporal, for instance by an admin before the row was
			// updated. Keep that pause and its note, so it cannot be turned into one
			// the owner may lift.
			reason = pauseReasonFromNote(state.Note)
		} else if err := s.temporalClient.ScheduleClient().GetHandle(opCtx, taskID).Pause(opCtx,
			client.SchedulePauseOptions{Note: pauseNote(PauseUser, "paused by the owner")}); err != nil {
			if isNotFound(err) {
				return errScheduleGone
			}
			return fmt.Errorf("failed to pause schedule: %w", err)
		}

		reasonText := string(reason)
		if _, err := s.queries.MarkTaskPaused(opCtx, pgdb.MarkTaskPausedParams{TaskID: taskID, PauseReason: &reasonText}); err != nil {
			// The schedule is paused; the reconciler records it from the pause note.
			log.Error("failed to record a task's pause", slog.String("error", err.Error()), slog.String("task_id", taskID))
			return fmt.Errorf("failed to record the pause: %w", err)
		}
		log.Info("task paused", slog.String("task_id", taskID), slog.String("user_id", userID), slog.String("reason", reasonText))
		row.Status = string(TaskStatusPaused)
		row.PauseReason = &reasonText
		paused = taskFromRow(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paused, nil
}

// scheduleState reads a task's schedule state: nil when the schedule does not
// exist.
func (s *Service) scheduleState(ctx context.Context, taskID string) (*client.ScheduleState, error) {
	desc, err := s.temporalClient.ScheduleClient().GetHandle(ctx, taskID).Describe(ctx)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if desc.Schedule.State == nil {
		return &client.ScheduleState{}, nil
	}
	return desc.Schedule.State, nil
}

func rowReason(row pgdb.Task) PauseReason {
	if row.PauseReason == nil {
		return ""
	}
	return PauseReason(*row.PauseReason)
}

// refuseResume is the refusal for resuming a task paused for reason, or nil
// when the owner may resume it.
func refuseResume(reason PauseReason, tier tiers.Config) error {
	switch {
	case reason == PausePlanEnded:
		return &LimitError{Reason: "plan_ended", Tier: tier.Name, Message: fmt.Sprintf(
			"This task is on hold because it does not fit the limits of the %s plan. It resumes by itself when the plan allows it again; otherwise delete it and create one with a less frequent schedule.",
			tier.DisplayName)}
	case !ResumableByOwner(reason):
		return &LimitError{Reason: "paused_by_admin", Tier: tier.Name, Message: "This task was paused by the service and cannot be resumed."}
	}
	return nil
}

// ResumeTask lets a paused task of the caller's run again on its schedule. It
// refuses a task paused by an admin or held because the plan ended, and one
// that does not fit the caller's current plan as a new task would have to: the
// number of tasks and, for a recurring task, the shortest interval and the
// runs per day with the caller's other recurring tasks. Otherwise pausing one
// task to make room for another, then resuming the first, would get past the
// limits. A row that says active is checked against its schedule, which may
// be paused if recording a pause failed.
func (s *Service) ResumeTask(ctx context.Context, userID, taskID string) (*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	if s.tierLookup == nil || s.groups == nil || s.admission == nil {
		return nil, fmt.Errorf("task limits are not configured")
	}
	tier, _, err := s.tierLookup.GetUserTierConfig(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up plan: %w", err)
	}

	// The steps do not stop when the client disconnects, and run under the
	// task's lock, so a resume cannot interleave with a pause or a deletion of
	// the same task.
	opCtx, cancel := detached(ctx, scheduleSetupTimeout)
	defer cancel()
	var resumed *Task
	err = s.withTaskLock(opCtx, taskID, func() error {
		row, err := getOwnedTask(opCtx, s.queries, userID, taskID)
		if err != nil {
			return err
		}
		rowPaused := row.Status == string(TaskStatusPaused)
		if !rowPaused && row.Status != string(TaskStatusActive) {
			return &ValidationError{Message: "Only a paused task can be resumed."}
		}
		if reason := rowReason(row); rowPaused && reason != "" {
			if refusal := refuseResume(reason, tier); refusal != nil {
				return refusal
			}
		}
		now := s.currentTime()
		if row.Type == string(TaskTypeOneTime) {
			schedule, err := parseStoredSchedule(row.Time)
			if err != nil {
				return &ValidationError{Message: "This one-time task cannot be resumed. Create a new task instead."}
			}
			if next := schedule.Next(row.CreatedAt); next.IsZero() || !next.After(now) {
				return &ValidationError{Message: "The time of this one-time task has passed. Create a new task instead."}
			}
		}

		// A paused row counts as active from here: a task created at the same time
		// sees it. A row that stayed active while its schedule was paused counts
		// already.
		if rowPaused {
			if err := s.markResumed(opCtx, userID, row, tier, now); err != nil {
				return err
			}
		}
		// setPaused puts a pause back on the row, so it stops counting as active.
		setPaused := func(reason *string) {
			if reason == nil {
				return
			}
			restoreCtx, cancelRestore := detached(ctx, cleanupTimeout)
			defer cancelRestore()
			if _, err := s.queries.MarkTaskPaused(restoreCtx, pgdb.MarkTaskPausedParams{TaskID: taskID, PauseReason: reason}); err != nil {
				log.Error("failed to restore a task's pause", slog.String("error", err.Error()), slog.String("task_id", taskID))
			}
		}
		restoreRow := func() {
			if rowPaused {
				setPaused(row.PauseReason)
			}
		}

		// The schedule's pause note is the authority on why it is paused: the row
		// may not have caught up with a pause made in Temporal. It is read right
		// before the schedule is resumed, so that as little time as possible is
		// left for a pause made in Temporal to slip in between.
		state, err := s.scheduleState(opCtx, taskID)
		switch {
		case err != nil:
			restoreRow()
			return fmt.Errorf("failed to read schedule: %w", err)
		case state == nil:
			restoreRow()
			return errScheduleGone
		case state.Paused:
			noteReason := pauseReasonFromNote(state.Note)
			if refusal := refuseResume(noteReason, tier); refusal != nil {
				// The row follows the schedule's pause.
				reasonText := string(noteReason)
				setPaused(&reasonText)
				return refusal
			}
			if err := s.temporalClient.ScheduleClient().GetHandle(opCtx, taskID).Unpause(opCtx,
				client.ScheduleUnpauseOptions{Note: "resumed by the owner"}); err != nil {
				if err := s.settleFailedResume(ctx, taskID, err, restoreRow); err != nil {
					return err
				}
			}
		}
		log.Info("task resumed", slog.String("task_id", taskID), slog.String("user_id", userID), slog.String("reason", string(rowReason(row))))
		row.Status = string(TaskStatusActive)
		row.PauseReason = nil
		resumed = taskFromRow(row)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resumed, nil
}

// settleFailedResume decides what a failed Unpause left behind. The schedule
// may have resumed even so: the row is paused again only once a lookup shows
// the schedule paused or gone. While its state is unknown, the row stays
// active and counted, and the reconciler settles it.
func (s *Service) settleFailedResume(ctx context.Context, taskID string, unpauseErr error, restoreRow func()) error {
	log := s.logger.WithContext(ctx).WithComponent("task-service")
	log.Error("failed to resume a task's schedule", slog.String("error", unpauseErr.Error()), slog.String("task_id", taskID))
	checkCtx, cancel := detached(ctx, cleanupTimeout)
	defer cancel()
	state, err := s.scheduleState(checkCtx, taskID)
	switch {
	case err != nil:
		log.Error("could not confirm whether the schedule resumed; the task stays counted",
			slog.String("error", err.Error()), slog.String("task_id", taskID))
		return fmt.Errorf("failed to resume schedule: %w", unpauseErr)
	case state == nil:
		restoreRow()
		return errScheduleGone
	case state.Paused:
		restoreRow()
		return fmt.Errorf("failed to resume schedule: %w", unpauseErr)
	}
	log.Warn("the schedule resumed despite the error", slog.String("task_id", taskID))
	return nil
}

// checkResumedSchedule applies the plan's recurring-task limits to a task
// being resumed, beside the group's open recurring tasks.
func checkResumedSchedule(row pgdb.Task, open openTasks, tier tiers.Config, now time.Time) error {
	if open.recurringCount >= tier.TaskMaxActiveRecurring {
		return &LimitError{Reason: "active_recurring_tasks", Tier: tier.Name, Message: fmt.Sprintf(
			"The limit of %d active recurring tasks on the %s plan has been reached. Pause or delete another recurring task first.",
			tier.TaskMaxActiveRecurring, tier.DisplayName)}
	}
	schedule, err := parseStoredSchedule(row.Time)
	if err != nil {
		return &ValidationError{Message: "This task's schedule cannot be checked against the plan's limits, so it cannot be resumed. Delete it and create a new task instead."}
	}
	if open.unreadableRecurring > 0 {
		return &LimitError{Reason: "unreadable_schedule", Tier: tier.Name, Message: "An existing recurring task has a schedule that cannot be checked against the plan's limits. Delete that task before resuming another recurring task."}
	}
	if gap := minInterval(schedule, now); gap > 0 && gap < tier.TaskMinInterval {
		return &LimitError{Reason: "min_interval", Tier: tier.Name, Message: fmt.Sprintf(
			"On the %s plan a recurring task can run at most %s, and this task runs more often, so it cannot be resumed. Delete it and create one with a less frequent schedule.",
			tier.DisplayName, describeInterval(tier.TaskMinInterval))}
	}
	if exceedsRunsPerDay(append(slices.Clone(open.recurring), schedule), now, tier.TaskMaxRunsPerDay) {
		return &LimitError{Reason: "runs_per_day", Tier: tier.Name, Message: fmt.Sprintf(
			"Resuming this task would take recurring tasks past %d runs a day, the limit on the %s plan. Pause or delete another recurring task first, or create this one with a less frequent schedule.",
			tier.TaskMaxRunsPerDay, tier.DisplayName)}
	}
	return nil
}

// markResumed sets a paused row active under the admission lock, after
// checking it against the plan as creation does, so a task created at the same
// time sees it.
func (s *Service) markResumed(ctx context.Context, userID string, row pgdb.Task, tier tiers.Config, now time.Time) error {
	userIDs, err := s.groups.UserIDs(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to look up quota group: %w", err)
	}
	return s.admission(ctx, userIDs, func(q pgdb.Querier) error {
		rows, err := q.ListOpenTasksForUsers(ctx, userIDs)
		if err != nil {
			return fmt.Errorf("failed to list tasks: %w", err)
		}
		open := summarizeOpenTasks(rows, now)
		if row.Type == string(TaskTypeRecurring) {
			if err := checkResumedSchedule(row, open, tier, now); err != nil {
				return err
			}
		}
		if row.Type == string(TaskTypeOneTime) && open.pendingOneTime >= tier.TaskMaxPendingOneTime {
			return &LimitError{Reason: "pending_one_time_tasks", Tier: tier.Name, Message: fmt.Sprintf(
				"The limit of %d scheduled one-time tasks on the %s plan has been reached. Delete another one-time task first.",
				tier.TaskMaxPendingOneTime, tier.DisplayName)}
		}
		changed, err := q.MarkTaskResumed(ctx, pgdb.MarkTaskResumedParams{TaskID: row.TaskID, PauseReason: row.PauseReason})
		if err != nil {
			return fmt.Errorf("failed to record the resume: %w", err)
		}
		if changed == 0 {
			return &ValidationError{Message: "The task changed while it was being resumed. Try again."}
		}
		return nil
	})
}
