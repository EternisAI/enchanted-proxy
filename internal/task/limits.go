package task

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/robfig/cron/v3"
)

const (
	maxTaskNameRunes = 100
	maxTaskTextRunes = 2000

	// Schedules are examined over the next year, which covers every
	// month, day-of-month and weekday combination a cron can express.
	scheduleHorizon = 366 * 24 * time.Hour
	// Bounds on the work per schedule: firing days in the horizon, and runs in one
	// day (a minute-level schedule runs 1,440 times a day; one with seconds more).
	maxFiringDays       = 400
	maxRunsPerDayWalked = 100_000
)

// strictParser accepts standard five-field cron only: no seconds, no year, no
// descriptors such as @every or @daily. Temporal accepts more, but limits are
// computed from what this parser understands, so anything else is refused.
var strictParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// lenientParser reads schedules created before the strict rule, so they still
// count toward the limits.
var lenientParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ValidationError is a malformed request; the REST endpoint answers 400.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

// LimitError is a request refused by a plan limit; the REST endpoint answers 403.
// Message is a sentence a user can read and the model can pass on.
type LimitError struct {
	Reason  string // machine-readable: which limit
	Message string
	Tier    string
}

func (e *LimitError) Error() string { return e.Message }

// TierLookup returns the caller's plan; request_tracking.Service implements it.
type TierLookup interface {
	GetUserTierConfig(ctx context.Context, userID string) (tiers.Config, *time.Time, error)
}

// GroupLookup returns the accounts whose tasks count toward the caller's limits.
type GroupLookup interface {
	UserIDs(ctx context.Context, userID string) ([]string, error)
}

// parseStrictSchedule parses a schedule for a new task.
func parseStrictSchedule(expr string) (cron.Schedule, error) {
	trimmed := strings.TrimSpace(expr)
	if strings.HasPrefix(trimmed, "@") || strings.Contains(trimmed, "TZ=") || len(strings.Fields(trimmed)) != 5 {
		return nil, &ValidationError{Message: "The schedule must be a standard five-field cron expression: minute hour day-of-month month day-of-week."}
	}
	s, err := strictParser.Parse(trimmed)
	if err != nil {
		return nil, &ValidationError{Message: "The schedule is not a valid cron expression: " + err.Error()}
	}
	return s, nil
}

// dailyProfile describes when a schedule runs within the horizon. Cron fields
// multiply out, so on every day a schedule fires it fires at the same times of
// day; the profile keeps those times and the list of firing days, which avoids
// walking every single run.
type dailyProfile struct {
	timesOfDay []time.Duration // offsets from midnight UTC, ascending
	days       []time.Time     // UTC midnights of the firing days, ascending
}

func profileSchedule(s cron.Schedule, from time.Time) dailyProfile {
	end := from.Add(scheduleHorizon)
	var p dailyProfile
	for t := s.Next(from); !t.IsZero() && t.Before(end) && len(p.days) <= maxFiringDays; {
		day := t.UTC().Truncate(24 * time.Hour)
		p.days = append(p.days, day)
		t = s.Next(day.Add(24*time.Hour - time.Nanosecond)) // first run on a later day
	}
	if len(p.days) == 0 {
		return p
	}
	// Read the times of day from a full firing day; the first may have begun before from.
	day := p.days[0]
	if len(p.days) > 1 {
		day = p.days[1]
	}
	for t := s.Next(day.Add(-time.Nanosecond)); !t.IsZero() && t.Before(day.Add(24*time.Hour)) && len(p.timesOfDay) < maxRunsPerDayWalked; t = s.Next(t) {
		p.timesOfDay = append(p.timesOfDay, t.Sub(day))
	}
	return p
}

// minInterval is the shortest gap between consecutive runs within the horizon,
// or 0 when the schedule runs fewer than twice.
func minInterval(s cron.Schedule, from time.Time) time.Duration {
	p := profileSchedule(s, from)
	var shortest time.Duration
	consider := func(d time.Duration) {
		if d > 0 && (shortest == 0 || d < shortest) {
			shortest = d
		}
	}
	for i := 1; i < len(p.timesOfDay); i++ {
		consider(p.timesOfDay[i] - p.timesOfDay[i-1])
	}
	if n := len(p.timesOfDay); n > 0 {
		// Last run of one firing day to the first run of the next.
		span := p.timesOfDay[n-1] - p.timesOfDay[0]
		for i := 1; i < len(p.days); i++ {
			consider(p.days[i].Sub(p.days[i-1]) - span)
		}
	}
	return shortest
}

// exceedsRunsPerDay reports whether the schedules together run more than limit
// times on any UTC day within the horizon.
func exceedsRunsPerDay(schedules []cron.Schedule, from time.Time, limit int) bool {
	perDay := map[time.Time]int{}
	for _, s := range schedules {
		p := profileSchedule(s, from)
		for _, day := range p.days {
			perDay[day] += len(p.timesOfDay)
			if perDay[day] > limit {
				return true
			}
		}
	}
	return false
}

// describeInterval renders a duration as words for refusal messages.
func describeInterval(d time.Duration) string {
	switch {
	case d == time.Hour:
		return "once an hour"
	case d%time.Hour == 0:
		return fmt.Sprintf("once every %d hours", int(d/time.Hour))
	case d%time.Minute == 0:
		return fmt.Sprintf("once every %d minutes", int(d/time.Minute))
	default:
		return "once every " + d.String()
	}
}

// openTasks summarizes a quota group's tasks that may still run.
type openTasks struct {
	recurring      []cron.Schedule
	recurringCount int
	pendingOneTime int
}

func summarizeOpenTasks(rows []pgdb.ListOpenTasksForUsersRow, now time.Time) openTasks {
	var o openTasks
	for _, r := range rows {
		switch r.Type {
		case string(TaskTypeRecurring):
			o.recurringCount++
			if s, err := lenientParser.Parse(r.Time); err == nil {
				o.recurring = append(o.recurring, s)
			}
		case string(TaskTypeOneTime):
			// A one-time task runs at its first fire time after creation. Rows are
			// not marked completed afterwards, so whether it is still pending is
			// worked out from the schedule.
			if s, err := lenientParser.Parse(r.Time); err == nil {
				if next := s.Next(r.CreatedAt); !next.IsZero() && next.After(now) {
					o.pendingOneTime++
				}
			}
		}
	}
	return o
}

// checkCreationLimits applies the caller's plan limits to a new task. It returns
// a *ValidationError, a *LimitError, or another error when a lookup failed.
func (s *Service) checkCreationLimits(ctx context.Context, userID string, req *CreateTaskRequest, now time.Time) error {
	if utf8.RuneCountInString(req.TaskName) > maxTaskNameRunes {
		return &ValidationError{Message: fmt.Sprintf("The task name can be at most %d characters.", maxTaskNameRunes)}
	}
	if utf8.RuneCountInString(req.TaskText) > maxTaskTextRunes {
		return &ValidationError{Message: fmt.Sprintf("The task text can be at most %d characters.", maxTaskTextRunes)}
	}
	schedule, err := parseStrictSchedule(req.Time)
	if err != nil {
		return err
	}

	if s.tierLookup == nil || s.groups == nil {
		return fmt.Errorf("task limits are not configured")
	}
	tier, _, err := s.tierLookup.GetUserTierConfig(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to look up plan: %w", err)
	}
	userIDs, err := s.groups.UserIDs(ctx, userID)
	if err != nil {
		return fmt.Errorf("failed to look up quota group: %w", err)
	}

	created, err := s.queries.CountTaskCreationsSince(ctx, pgdb.CountTaskCreationsSinceParams{
		UserIds: userIDs,
		Since:   now.Add(-24 * time.Hour),
	})
	if err != nil {
		return fmt.Errorf("failed to count recent tasks: %w", err)
	}
	if int(created) >= tier.TaskMaxCreatedPerDay {
		return &LimitError{Reason: "tasks_created_per_day", Tier: tier.Name, Message: fmt.Sprintf(
			"The limit of %d new tasks in 24 hours on the %s plan has been reached. Try again later.",
			tier.TaskMaxCreatedPerDay, tier.DisplayName)}
	}

	rows, err := s.queries.ListOpenTasksForUsers(ctx, userIDs)
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}
	open := summarizeOpenTasks(rows, now)

	if req.Type == string(TaskTypeOneTime) {
		if open.pendingOneTime >= tier.TaskMaxPendingOneTime {
			return &LimitError{Reason: "pending_one_time_tasks", Tier: tier.Name, Message: fmt.Sprintf(
				"The limit of %d scheduled one-time tasks on the %s plan has been reached.",
				tier.TaskMaxPendingOneTime, tier.DisplayName)}
		}
		return nil
	}

	if open.recurringCount >= tier.TaskMaxActiveRecurring {
		return &LimitError{Reason: "active_recurring_tasks", Tier: tier.Name, Message: fmt.Sprintf(
			"The limit of %d active recurring tasks on the %s plan has been reached.",
			tier.TaskMaxActiveRecurring, tier.DisplayName)}
	}
	if gap := minInterval(schedule, now); gap > 0 && gap < tier.TaskMinInterval {
		return &LimitError{Reason: "min_interval", Tier: tier.Name, Message: fmt.Sprintf(
			"On the %s plan a recurring task can run at most %s. Choose a less frequent schedule.",
			tier.DisplayName, describeInterval(tier.TaskMinInterval))}
	}
	if exceedsRunsPerDay(append(open.recurring, schedule), now, tier.TaskMaxRunsPerDay) {
		return &LimitError{Reason: "runs_per_day", Tier: tier.Name, Message: fmt.Sprintf(
			"This schedule would take recurring tasks past %d runs a day, the limit on the %s plan. Choose a less frequent schedule.",
			tier.TaskMaxRunsPerDay, tier.DisplayName)}
	}
	return nil
}
