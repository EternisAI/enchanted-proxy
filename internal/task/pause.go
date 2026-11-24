package task

import (
	"context"
	"fmt"
	"log/slog"
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

	row, err := getOwnedTask(ctx, s.queries, userID, taskID)
	if err != nil {
		return nil, err
	}
	switch TaskStatus(row.Status) {
	case TaskStatusPaused:
		return taskFromRow(row), nil
	case TaskStatusActive:
	case TaskStatusPending:
		return nil, &ValidationError{Message: "The task is still being set up. Try again in a moment."}
	default:
		return nil, &ValidationError{Message: "This task has finished and cannot be paused."}
	}

	// The schedule is changed first and the row follows it. The steps do not stop
	// when the client disconnects.
	opCtx, cancel := detached(ctx, scheduleSetupTimeout)
	defer cancel()
	handle := s.temporalClient.ScheduleClient().GetHandle(opCtx, taskID)
	desc, err := handle.Describe(opCtx)
	if isNotFound(err) {
		return nil, errScheduleGone
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read schedule: %w", err)
	}

	reason := PauseUser
	if desc.Schedule.State != nil && desc.Schedule.State.Paused {
		// Already paused in Temporal, for instance by an admin before the row was
		// updated. Keep that pause and its note, so it cannot be turned into one
		// the owner may lift.
		reason = pauseReasonFromNote(desc.Schedule.State.Note)
	} else if err := handle.Pause(opCtx, client.SchedulePauseOptions{Note: pauseNote(PauseUser, "paused by the owner")}); err != nil {
		if isNotFound(err) {
			return nil, errScheduleGone
		}
		return nil, fmt.Errorf("failed to pause schedule: %w", err)
	}

	reasonText := string(reason)
	if _, err := s.queries.MarkTaskPaused(opCtx, pgdb.MarkTaskPausedParams{TaskID: taskID, PauseReason: &reasonText}); err != nil {
		// The schedule is paused; the reconciler records it from the pause note.
		log.Error("failed to record a task's pause", slog.String("error", err.Error()), slog.String("task_id", taskID))
		return nil, fmt.Errorf("failed to record the pause: %w", err)
	}
	log.Info("task paused", slog.String("task_id", taskID), slog.String("user_id", userID), slog.String("reason", reasonText))

	row.Status = string(TaskStatusPaused)
	row.PauseReason = &reasonText
	return taskFromRow(row), nil
}

// ResumeTask lets a paused task of the caller's run again on its schedule. It
// refuses a task paused by an admin or held because the plan ended, and a
// task that would take the caller over the plan's number of tasks. The other
// creation limits are not applied again, so that pausing stays reversible. A
// row that says active is checked against its schedule, which may be paused
// if recording a pause failed.
func (s *Service) ResumeTask(ctx context.Context, userID, taskID string) (*Task, error) {
	log := s.logger.WithContext(ctx).WithComponent("task-service")

	row, err := getOwnedTask(ctx, s.queries, userID, taskID)
	if err != nil {
		return nil, err
	}
	rowPaused := row.Status == string(TaskStatusPaused)
	if !rowPaused && row.Status != string(TaskStatusActive) {
		return nil, &ValidationError{Message: "Only a paused task can be resumed."}
	}

	if s.tierLookup == nil || s.groups == nil || s.admission == nil {
		return nil, fmt.Errorf("task limits are not configured")
	}
	tier, _, err := s.tierLookup.GetUserTierConfig(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up plan: %w", err)
	}

	// The schedule's pause note is the authority on why it is paused: the row may
	// not have caught up with a pause made in Temporal.
	opCtx, cancel := detached(ctx, scheduleSetupTimeout)
	defer cancel()
	handle := s.temporalClient.ScheduleClient().GetHandle(opCtx, taskID)
	desc, err := handle.Describe(opCtx)
	if isNotFound(err) {
		return nil, errScheduleGone
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read schedule: %w", err)
	}
	schedulePaused := desc.Schedule.State != nil && desc.Schedule.State.Paused
	if !rowPaused && !schedulePaused {
		return taskFromRow(row), nil // running already
	}
	var reason PauseReason
	var reasons []PauseReason
	if rowPaused && row.PauseReason != nil && *row.PauseReason != "" {
		reason = PauseReason(*row.PauseReason)
		reasons = append(reasons, reason)
	}
	if schedulePaused {
		reasons = append(reasons, pauseReasonFromNote(desc.Schedule.State.Note))
	}
	for _, r := range reasons {
		switch {
		case r == PausePlanEnded:
			return nil, &LimitError{Reason: "plan_ended", Tier: tier.Name, Message: fmt.Sprintf(
				"This task is on hold because it does not fit the limits of the %s plan. It resumes by itself when the plan allows it again; otherwise delete it and create one with a less frequent schedule.",
				tier.DisplayName)}
		case !ResumableByOwner(r):
			return nil, &LimitError{Reason: "paused_by_admin", Tier: tier.Name, Message: "This task was paused by the service and cannot be resumed."}
		}
	}

	now := s.currentTime()
	if row.Type == string(TaskTypeOneTime) {
		schedule, err := parseStoredSchedule(row.Time)
		if err != nil {
			return nil, &ValidationError{Message: "This one-time task cannot be resumed. Create a new task instead."}
		}
		if next := schedule.Next(row.CreatedAt); next.IsZero() || !next.After(now) {
			return nil, &ValidationError{Message: "The time of this one-time task has passed. Create a new task instead."}
		}
	}

	// A row that stayed active while its schedule was paused counts already;
	// only the schedule needs resuming.
	if rowPaused {
		if err := s.markResumed(ctx, userID, row, tier, now); err != nil {
			return nil, err
		}
	}
	if schedulePaused {
		if err := handle.Unpause(opCtx, client.ScheduleUnpauseOptions{Note: "resumed by the owner"}); err != nil {
			log.Error("failed to resume a task's schedule", slog.String("error", err.Error()), slog.String("task_id", taskID))
			// Put the pause back on the row, so the task stops counting as active. If
			// the schedule did resume after all, the reconciler sets the row active.
			if rowPaused {
				revertCtx, cancelRevert := detached(ctx, cleanupTimeout)
				defer cancelRevert()
				if _, revertErr := s.queries.MarkTaskPaused(revertCtx, pgdb.MarkTaskPausedParams{TaskID: taskID, PauseReason: row.PauseReason}); revertErr != nil {
					log.Error("failed to restore a task's pause", slog.String("error", revertErr.Error()), slog.String("task_id", taskID))
				}
			}
			if isNotFound(err) {
				return nil, errScheduleGone
			}
			return nil, fmt.Errorf("failed to resume schedule: %w", err)
		}
	}
	log.Info("task resumed", slog.String("task_id", taskID), slog.String("user_id", userID), slog.String("reason", string(reason)))

	row.Status = string(TaskStatusActive)
	row.PauseReason = nil
	return taskFromRow(row), nil
}

// markResumed sets a paused row active under the admission lock, after
// checking the plan's task count, so a task created at the same time sees it.
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
		if row.Type == string(TaskTypeRecurring) && open.recurringCount >= tier.TaskMaxActiveRecurring {
			return &LimitError{Reason: "active_recurring_tasks", Tier: tier.Name, Message: fmt.Sprintf(
				"The limit of %d active recurring tasks on the %s plan has been reached. Pause or delete another recurring task first.",
				tier.TaskMaxActiveRecurring, tier.DisplayName)}
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
