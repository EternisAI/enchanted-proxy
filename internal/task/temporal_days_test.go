package task

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
)

// Temporal runs a schedule only when every field matches; it does not have
// cron's rule that a restricted day-of-month and day-of-week are alternatives.
func TestDaysMatchBothFieldsAsInTemporal(t *testing.T) {
	mondayFirst := time.Date(2027, 2, 1, 9, 0, 0, 0, time.UTC) // the first Monday the 1st after now
	for name, s := range map[string]interface{ Next(time.Time) time.Time }{
		"new":        mustParse(t, "0 9 1 * MON"),
		"stored":     mustParseStored(t, "0 9 1 * MON"),
		"stored six": mustParseStored(t, "0 9 1 * MON 2027"),
	} {
		if got := s.Next(now); !got.Equal(mondayFirst) {
			t.Errorf("%s: next run %v, want %v", name, got, mondayFirst)
		}
	}
	if got := runsOnDay(mustParse(t, "0 9 1 * MON"), day(2026, 12, 1)); got != 0 {
		t.Errorf("runs %d times on Tuesday Dec 1, want 0", got)
	}
	// A star in either field leaves robfig's matching as it was.
	if got := mustParse(t, "0 9 * * MON").Next(now); !got.Equal(time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("every Monday: next run %v", got)
	}
	// A stepped day-of-month is a restriction too: odd days that are Mondays.
	if got := mustParse(t, "0 9 */2 * MON").Next(now); got.Weekday() != time.Monday || got.Day()%2 != 1 {
		t.Errorf("odd-day Mondays: next run %v", got)
	}
	// A combination that never happens does not run.
	if got := mustParse(t, "0 9 30 2 MON").Next(now); !got.IsZero() {
		t.Errorf("Monday Feb 30 runs at %v", got)
	}
}

func TestDayMatchingDoesNotOvercountRunsPerDay(t *testing.T) {
	// The two schedules never fire on the same day: at most 24 runs a day.
	q := &limitQuerier{rows: recurringRows("0 0,1 2 * TUE")}
	if err := newLimitService(tiers.TierFree, q).admitErr(context.Background(), "user-1", request("recurring", "0 * 1 * MON"), now); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// A one-time task's schedule is pinned to its first run after the task row's
// creation time: it starts and ends there, so it fires once.
func TestOneTimeScheduleIsPinnedToItsRun(t *testing.T) {
	for _, c := range []struct {
		name      string
		createdAt time.Time
		want      time.Time
	}{
		// The first Monday the 1st after now is February 1, 2027.
		{"before", now, time.Date(2027, 2, 1, 9, 0, 0, 0, time.UTC)},
		// On that day after 09:00, the next one is March 1.
		{"same day, later", time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC), time.Date(2027, 3, 1, 9, 0, 0, 0, time.UTC)},
	} {
		schedules := &scriptedSchedules{}
		s := scriptedService(&limitQuerier{createdAt: c.createdAt}, schedules)
		s.clock = func() time.Time { return c.createdAt }
		if _, err := s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 1 * MON")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if spec := schedules.options.Spec; !spec.StartAt.Equal(c.want) || !spec.EndAt.Equal(c.want) {
			t.Errorf("%s: schedule from %v to %v, want both %v", c.name, spec.StartAt.UTC(), spec.EndAt.UTC(), c.want)
		}
	}
}

// The run is taken from the task row's creation time, which pending
// accounting also reads, so the two agree. A run that passes while the task is
// admitted cannot be scheduled; the task is released before any schedule.
func TestOneTimeRunFollowsTheTaskRow(t *testing.T) {
	createdAt := time.Date(2026, 11, 23, 8, 59, 59, 0, time.UTC)
	nine := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

	// Admitted before 09:00: the run is today's, and the row counts as pending
	// until then, as the schedule does.
	q := &limitQuerier{createdAt: createdAt}
	schedules := &scriptedSchedules{}
	s := scriptedService(q, schedules)
	s.clock = func() time.Time { return createdAt.Add(500 * time.Millisecond) }
	if _, err := s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 * * *")); err != nil {
		t.Fatal(err)
	}
	if !schedules.options.Spec.StartAt.Equal(nine) {
		t.Fatalf("scheduled run %v, want %v", schedules.options.Spec.StartAt.UTC(), nine)
	}
	row := []pgdb.ListOpenTasksForUsersRow{{Type: "one_time", Time: "0 9 * * *", Status: "active", CreatedAt: createdAt}}
	if summarizeOpenTasks(row, nine.Add(-time.Second)).pendingOneTime != 1 || summarizeOpenTasks(row, nine.Add(time.Second)).pendingOneTime != 0 {
		t.Fatal("pending accounting disagrees with the scheduled run")
	}

	// Admission ends after 09:00: refused and released, no schedule created.
	q = &limitQuerier{createdAt: createdAt}
	schedules = &scriptedSchedules{}
	s = scriptedService(q, schedules)
	s.clock = func() time.Time { return nine.Add(time.Second) }
	_, err := s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 * * *"))
	var v *ValidationError
	if !stderrors.As(err, &v) || !strings.Contains(err.Error(), "passed") {
		t.Fatalf("err = %v, want a ValidationError saying the time passed", err)
	}
	if len(q.releasedCreation) != 1 || schedules.options.ID != "" {
		t.Fatalf("released %v, schedule %q; want the task released and no schedule", q.releasedCreation, schedules.options.ID)
	}
}

// Temporal reads a schedule without a time zone in UTC, whatever the zone of
// the server checking it.
func TestSchedulesAreReadInUTC(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("UTC-7", -7*60*60)
	defer func() { time.Local = saved }()

	nine, midnight := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for name, c := range map[string]struct {
		s    interface{ Next(time.Time) time.Time }
		want time.Time
	}{
		"new":               {mustParse(t, "0 9 * * *"), nine},
		"stored":            {mustParseStored(t, "0 9 * * *"), nine},
		"stored seven":      {mustParseStored(t, "0 0 9 * * * *"), nine},
		"stored descriptor": {mustParseStored(t, "@daily"), midnight},
	} {
		if got := c.s.Next(now); !got.Equal(c.want) {
			t.Errorf("%s: next run %v, want %v", name, got.UTC(), c.want)
		}
	}
}
