package task

import (
	"context"
	"testing"
	"time"

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
		"stored tz":  mustParseStored(t, "CRON_TZ=UTC 0 9 1 * 1"),
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

func TestOneTimeTaskEndsAfterTemporalsFirstRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		clock time.Time
		want  time.Time
	}{
		// The first Monday the 1st after now is February 1, 2027.
		{"before", now, time.Date(2027, 2, 1, 9, 5, 0, 0, time.UTC)},
		// On that day after 09:00, the next one is March 1.
		{"same day, later", time.Date(2027, 2, 1, 10, 0, 0, 0, time.UTC), time.Date(2027, 3, 1, 9, 5, 0, 0, time.UTC)},
	} {
		schedules := &scriptedSchedules{}
		s := scriptedService(&limitQuerier{}, schedules)
		s.clock = func() time.Time { return c.clock }
		if _, err := s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 1 * MON")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := schedules.options.Spec.EndAt; !got.Equal(c.want) {
			t.Errorf("%s: one-time schedule ends at %v, want %v", c.name, got.UTC(), c.want)
		}
	}
}

// The one-time run is chosen after admission, so a run that passed meanwhile
// is not picked: the schedule would end before its next run.
func TestOneTimeRunIsChosenAfterAdmission(t *testing.T) {
	ticks := []time.Time{
		time.Date(2026, 11, 23, 8, 59, 59, 0, time.UTC), // when the request starts
		time.Date(2026, 11, 23, 9, 0, 1, 0, time.UTC),   // after admission
	}
	schedules := &scriptedSchedules{}
	s := scriptedService(&limitQuerier{}, schedules)
	s.clock = func() time.Time {
		t := ticks[0]
		if len(ticks) > 1 {
			ticks = ticks[1:]
		}
		return t
	}
	if _, err := s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 * * *")); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 24, 9, 5, 0, 0, time.UTC); !schedules.options.Spec.EndAt.Equal(want) {
		t.Fatalf("one-time schedule ends at %v, want %v", schedules.options.Spec.EndAt.UTC(), want)
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
