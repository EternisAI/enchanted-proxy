package task

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"go.temporal.io/sdk/client"
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

// capturingTemporal records the schedule CreateTask asks for, then fails it.
type capturingTemporal struct {
	client.Client
	opts *client.ScheduleOptions
}

func (c capturingTemporal) ScheduleClient() client.ScheduleClient {
	return capturingSchedules{opts: c.opts}
}

type capturingSchedules struct {
	client.ScheduleClient
	opts *client.ScheduleOptions
}

func (c capturingSchedules) Create(_ context.Context, o client.ScheduleOptions) (client.ScheduleHandle, error) {
	*c.opts = o
	return nil, stderrors.New("captured")
}

func TestOneTimeTaskEndsAfterTemporalsFirstRun(t *testing.T) {
	var opts client.ScheduleOptions
	s := newLimitService(tiers.TierFree, &limitQuerier{})
	s.temporalClient = capturingTemporal{opts: &opts}
	_, _ = s.CreateTask(context.Background(), "user-1", request("one_time", "0 9 1 * MON"))

	first := time.Now().UTC()
	for first = first.Truncate(24 * time.Hour); first.Day() != 1 || first.Weekday() != time.Monday; first = first.Add(24 * time.Hour) {
	}
	want := first.Add(9*time.Hour + 5*time.Minute)
	if !opts.Spec.EndAt.Equal(want) {
		t.Fatalf("one-time schedule ends at %v, want %v (after the first Monday the 1st)", opts.Spec.EndAt.UTC(), want)
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
