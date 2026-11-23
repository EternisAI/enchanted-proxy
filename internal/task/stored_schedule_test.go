package task

import (
	"context"
	"testing"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	"github.com/robfig/cron/v3"
)

func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// runsOnDay counts a schedule's runs within one UTC day.
func runsOnDay(s cron.Schedule, d time.Time) int {
	n := 0
	for t := s.Next(d.Add(-time.Nanosecond)); !t.IsZero() && t.Before(d.Add(24*time.Hour)) && n < 200_000; t = s.Next(t) {
		n++
	}
	return n
}

func mustParseStored(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := parseStoredSchedule(expr)
	if err != nil {
		t.Fatalf("parse stored %q: %v", expr, err)
	}
	return s
}

func TestParseStoredScheduleReadsTemporalLayouts(t *testing.T) {
	weekday, in2027 := day(2026, 11, 23), day(2027, 1, 4) // both Mondays
	cases := []struct {
		expr string
		on   time.Time
		runs int
	}{
		{"0 9 * * *", weekday, 1},
		{"0 * * * * 2027", weekday, 0},            // six fields: minute to day-of-week, then a year
		{"0 * * * * 2027", in2027, 24},            //
		{"0 * * * * 2026-2030/2", in2027, 0},      // even years only
		{"0 * * * * 2025,2027", in2027, 24},       //
		{"*/30 * * * * * *", weekday, 2880},       // seven fields: seconds first
		{"0 0 9 * * * 2026-2027", weekday, 1},     //
		{"@daily", weekday, 1},                    //
		{"@hourly", weekday, 24},                  //
		{"@every 1d", weekday, 1},                 //
		{"0 9 * * * # every morning", weekday, 1}, //
		{"0 9 * * 7", weekday, 0},                 // 7 is Sunday
		{"0 9 * * 7", day(2026, 11, 29), 1},       //
		{"0 9 * * 5-7", day(2026, 11, 29), 1},     //
		{"0 9 * * 1,7", weekday, 1},               //
	}
	for _, c := range cases {
		if got := runsOnDay(mustParseStored(t, c.expr), c.on); got != c.runs {
			t.Errorf("%q on %s: %d runs, want %d", c.expr, c.on.Format("2006-01-02"), got, c.runs)
		}
	}
	if next := mustParseStored(t, "0 * * * * 2025").Next(now); !next.IsZero() {
		t.Errorf("a schedule for 2025 runs again at %v", next)
	}
}

func TestEveryRunsOnTemporalsGrid(t *testing.T) {
	at := time.Date(2026, 11, 22, 10, 31, 0, 0, time.UTC)
	for expr, want := range map[string]time.Time{
		"@every 10m":    time.Date(2026, 11, 22, 10, 40, 0, 0, time.UTC),
		"@every 2h/30m": time.Date(2026, 11, 22, 12, 30, 0, 0, time.UTC), // multiples of 2h from 00:30
		"@every 45s":    time.Date(2026, 11, 22, 10, 31, 30, 0, time.UTC),
	} {
		if got := mustParseStored(t, expr).Next(at); !got.Equal(want) {
			t.Errorf("%q after %v: %v, want %v", expr, at, got, want)
		}
	}
	// An interval that does not divide a day counts the most runs any day has.
	if p := profileSchedule(mustParseStored(t, "@every 7h"), now); len(p.timesOfDay) != 4 {
		t.Errorf("@every 7h counts %d runs a day, want 4", len(p.timesOfDay))
	}
}

func TestParseStoredScheduleRefusesWhatItCannotCount(t *testing.T) {
	for _, expr := range []string{"", "bogus", "0 9 * * * * * *", "0 9 *", "@every 10x", "@every 0s", "@every", "CRON_TZ=Nowhere/City 0 9 * * *", "CRON_TZ=Europe/Berlin 0 9 * * *", "TZ=UTC 0 9 * * *", "0 9 * * * 20x6", "61 * * * *"} {
		if _, err := parseStoredSchedule(expr); err == nil {
			t.Errorf("%q was read", expr)
		}
	}
}

func TestStoredLegacySchedulesCountTowardLimits(t *testing.T) {
	stored := func(exprs ...string) *limitQuerier { return &limitQuerier{rows: recurringRows(exprs...)} }
	oneTimeOnly := request("one_time", "0 9 24 12 *")
	tests := []struct {
		name   string
		tier   tiers.Tier
		q      *limitQuerier
		req    *CreateTaskRequest
		reason string
	}{
		// Hourly during 2027 plus two more runs a day: 26 on 2027 days, within the next year.
		{"six-field hourly in 2027", tiers.TierFree, stored("0 * * * * 2027"), request("recurring", "0 12,13 * * *"), "runs_per_day"},
		{"six-field hourly in 2025", tiers.TierFree, stored("0 * * * * 2025"), request("recurring", "0 12,13 * * *"), ""},
		{"seven-field every 30 seconds", tiers.TierPro, stored("*/30 * * * * * *"), request("recurring", "0 9 * * *"), "runs_per_day"},
		{"every 10 minutes", tiers.TierPlus, stored("@every 10m"), request("recurring", "0 9 * * *"), "runs_per_day"},
		{"every 10 minutes on Pro", tiers.TierPro, stored("@every 10m"), request("recurring", "0 9 * * *"), ""},
		{"unreadable blocks recurring", tiers.TierPro, stored("bogus"), request("recurring", "0 9 * * *"), "unreadable_schedule"},
		{"time zone is unreadable", tiers.TierPro, stored("CRON_TZ=Asia/Tokyo 0 9 * * *"), request("recurring", "0 9 * * *"), "unreadable_schedule"},
		{"unreadable leaves one-time alone", tiers.TierPro, stored("bogus"), oneTimeOnly, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := newLimitService(tt.tier, tt.q).admitErr(context.Background(), "user-1", tt.req, now)
			if got := limitReason(err); got != tt.reason {
				t.Fatalf("reason = %q (err %v), want %q", got, err, tt.reason)
			}
		})
	}
}

func TestUnreadableOneTimeTaskCountsAsPending(t *testing.T) {
	rows := make([]pgdb.ListOpenTasksForUsersRow, 20)
	for i := range rows {
		rows[i] = pgdb.ListOpenTasksForUsersRow{Type: "one_time", Time: "bogus", Status: "active", CreatedAt: now.Add(-time.Hour)}
	}
	err := newLimitService(tiers.TierFree, &limitQuerier{rows: rows}).admitErr(context.Background(), "user-1", request("one_time", "0 9 24 12 *"), now)
	if limitReason(err) != "pending_one_time_tasks" {
		t.Fatalf("err = %v, want the pending one-time limit", err)
	}
}

func TestSteppedYearsFollowTemporalsCalendar(t *testing.T) {
	cases := map[string]map[int]bool{
		"*/2":    {2026: true, 2027: false, 2028: true, 2102: false},
		"2026/3": {2026: true, 2029: true, 2030: false},
		"*":      {1999: true, 2200: true},
	}
	for field, years := range cases {
		allow, err := parseYearField(field)
		if err != nil {
			t.Fatalf("%q: %v", field, err)
		}
		for year, want := range years {
			if allow(year) != want {
				t.Errorf("%q allows %d: %v, want %v", field, year, !want, want)
			}
		}
	}
	evenYears := mustParseStored(t, "0 * * * * */2")
	if runsOnDay(evenYears, day(2027, 1, 4)) != 0 || runsOnDay(evenYears, day(2028, 1, 3)) != 24 {
		t.Error("an hourly schedule in even years runs in 2027 or not in 2028")
	}
}
