package task

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Stored schedules predate the strict five-field rule: the REST endpoint used
// to pass any string to Temporal. They are read here the way Temporal reads a
// cron_string (go.temporal.io/api schedule/v1), so they count toward the limits:
//
//   - 5 fields: minute, hour, day-of-month, month, day-of-week
//   - 6 fields: the same plus a year
//   - 7 fields: second in front, year at the end
//   - @yearly, @monthly, @weekly, @daily, @hourly
//   - @every <interval>[/<phase>], with units s, m, h or d
//   - an optional CRON_TZ= or TZ= prefix (UTC without one) and a trailing # comment
//   - 7 as well as 0 for Sunday
//   - a run only on days matching both day-of-month and day-of-week

// storedFieldsParser reads the time fields once they are normalized to six,
// seconds first; storedDescriptorParser reads the @ shorthands.
var (
	storedFieldsParser     = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	storedDescriptorParser = cron.NewParser(cron.Descriptor)
)

var everyPart = regexp.MustCompile(`^([0-9]+)([smhd])$`)

// parseStoredSchedule reads a schedule as Temporal does. An error means the
// schedule cannot be counted, which callers must not treat as "never runs".
func parseStoredSchedule(expr string) (cron.Schedule, error) {
	s := expr
	if i := strings.Index(s, "#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)

	tz := "CRON_TZ=UTC "
	if strings.HasPrefix(s, "CRON_TZ=") || strings.HasPrefix(s, "TZ=") {
		prefix, rest, _ := strings.Cut(s, " ")
		_, zone, _ := strings.Cut(prefix, "=")
		if _, err := time.LoadLocation(zone); err != nil {
			return nil, fmt.Errorf("unknown time zone %q", zone)
		}
		tz = "CRON_TZ=" + zone + " "
		s = strings.TrimSpace(rest)
	}

	if rest, ok := strings.CutPrefix(s, "@every "); ok {
		return parseEvery(strings.TrimSpace(rest))
	}
	if strings.HasPrefix(s, "@") {
		return storedDescriptorParser.Parse(tz + s)
	}

	fields := strings.Fields(s)
	second, year := "0", "*"
	switch len(fields) {
	case 5:
	case 6:
		year, fields = fields[5], fields[:5]
	case 7:
		second, year, fields = fields[0], fields[6], fields[1:6]
	default:
		return nil, fmt.Errorf("a stored schedule has %d fields, want 5, 6 or 7", len(fields))
	}
	fields[4] = sundayAsZero(fields[4])

	parsed, err := storedFieldsParser.Parse(tz + second + " " + strings.Join(fields, " "))
	if err != nil {
		return nil, err
	}
	spec := withTemporalDays(parsed)
	if year == "*" {
		return spec, nil
	}
	allow, err := parseYearField(year)
	if err != nil {
		return nil, err
	}
	return yearSchedule{inner: spec, allow: allow}, nil
}

// sundayAsZero rewrites Temporal's 7 for Sunday, which robfig does not accept,
// in plain values and in ranges without a step.
func sundayAsZero(dow string) string {
	parts := strings.Split(dow, ",")
	for i, p := range parts {
		switch lo, hi, isRange := strings.Cut(p, "-"); {
		case p == "7":
			parts[i] = "0"
		case isRange && hi == "7" && lo == "7":
			parts[i] = "0"
		case isRange && hi == "7":
			parts[i] = lo + "-6,0"
		}
	}
	return strings.Join(parts, ",")
}

// parseEvery reads "<interval>[/<phase>]".
func parseEvery(spec string) (cron.Schedule, error) {
	intervalText, phaseText, hasPhase := strings.Cut(spec, "/")
	every, err := parseEveryDuration(intervalText)
	if err != nil || every <= 0 {
		return nil, fmt.Errorf("invalid @every interval %q", intervalText)
	}
	var phase time.Duration
	if hasPhase {
		if phase, err = parseEveryDuration(phaseText); err != nil {
			return nil, fmt.Errorf("invalid @every phase %q", phaseText)
		}
	}
	return intervalSchedule{every: every, phase: phase % every}, nil
}

func parseEveryDuration(s string) (time.Duration, error) {
	m := everyPart.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n > int64(100*366*24*time.Hour/unit) { // past a century, and before overflow
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return time.Duration(n) * unit, nil
}

// intervalSchedule is a stored @every schedule. Temporal runs it at the
// multiples of the interval since the Unix epoch, shifted by the phase.
type intervalSchedule struct {
	every, phase time.Duration
}

func (s intervalSchedule) Next(t time.Time) time.Time {
	origin := time.Unix(0, 0).UTC().Add(s.phase)
	n := t.Sub(origin) / s.every
	return origin.Add((n + 1) * s.every)
}

// profile counts an interval shorter than a day as running at midnight and
// at every multiple after it, on every day: the most runs it can have on any
// day. Days do not repeat the same times of day when the interval does not
// divide a day, so the generic profile could undercount by one.
func (s intervalSchedule) profile(from time.Time) dailyProfile {
	var p dailyProfile
	for d := time.Duration(0); d < 24*time.Hour && len(p.timesOfDay) < maxRunsPerDayWalked; d += s.every {
		p.timesOfDay = append(p.timesOfDay, d)
	}
	first := from.UTC().Truncate(24 * time.Hour)
	for day := first; day.Before(from.Add(scheduleHorizon)) && len(p.days) <= maxFiringDays; day = day.Add(24 * time.Hour) {
		p.days = append(p.days, day)
	}
	return p
}

// yearSchedule keeps the runs of a schedule that fall in the years its year
// field allows.
type yearSchedule struct {
	inner cron.Schedule
	allow func(year int) bool
}

func (y yearSchedule) Next(t time.Time) time.Time {
	// robfig gives up five years ahead; a few skips cover that.
	for range 8 {
		n := y.inner.Next(t)
		if n.IsZero() || y.allow(n.Year()) {
			return n
		}
		t = time.Date(n.Year()+1, 1, 1, 0, 0, 0, 0, n.Location()).Add(-time.Nanosecond)
	}
	return time.Time{}
}

// parseYearField reads a year field: a comma list of *, x, x-z, x/y and x-z/y.
func parseYearField(field string) (func(int) bool, error) {
	var matchers []func(int) bool
	for _, part := range strings.Split(field, ",") {
		rangeText, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid year step %q", part)
			}
			step = n
		}
		if rangeText == "*" {
			matchers = append(matchers, func(int) bool { return true })
			continue
		}
		loText, hiText, isRange := strings.Cut(rangeText, "-")
		lo, err := strconv.Atoi(loText)
		if err != nil {
			return nil, fmt.Errorf("invalid year %q", part)
		}
		hi := lo
		switch {
		case isRange:
			if hi, err = strconv.Atoi(hiText); err != nil || hi < lo {
				return nil, fmt.Errorf("invalid year range %q", part)
			}
		case hasStep:
			hi = 1 << 30 // x/y runs from x onwards
		}
		matchers = append(matchers, func(y int) bool { return y >= lo && y <= hi && (y-lo)%step == 0 })
	}
	return func(y int) bool {
		for _, m := range matchers {
			if m(y) {
				return true
			}
		}
		return false
	}, nil
}

// robfigStarBit marks a field written as * (robfig/cron/v3's starBit).
const robfigStarBit uint64 = 1 << 63

// bothDays applies Temporal's rule that a time matches only when every field
// matches. robfig, like traditional cron, runs on a day that matches either the
// day-of-month or the day-of-week when both are restricted.
type bothDays struct{ spec *cron.SpecSchedule }

func (b bothDays) Next(t time.Time) time.Time {
	// Each pass moves to a later day; robfig itself looks five years ahead.
	for range 5 * 366 {
		n := b.spec.Next(t)
		if n.IsZero() || b.matches(n) {
			return n
		}
		y, m, d := n.Date()
		t = time.Date(y, m, d+1, 0, 0, 0, 0, n.Location()).Add(-time.Nanosecond)
	}
	return time.Time{}
}

func (b bothDays) matches(t time.Time) bool {
	return b.spec.Dom&(1<<uint(t.Day())) != 0 && b.spec.Dow&(1<<uint(t.Weekday())) != 0
}

// withTemporalDays gives a parsed schedule Temporal's day matching. It only
// changes schedules whose day-of-month and day-of-week are both restricted.
func withTemporalDays(s cron.Schedule) cron.Schedule {
	spec, ok := s.(*cron.SpecSchedule)
	if !ok || spec.Dom&robfigStarBit != 0 || spec.Dow&robfigStarBit != 0 {
		return s
	}
	return bothDays{spec: spec}
}
