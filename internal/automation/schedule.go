package automation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxHorizonDays bounds the day-by-day search for the next occurrence so a
// schedule that can never fire (an impossible wall-clock time after zone
// normalization, a hand-edited record) returns an error instead of looping.
// A valid daily/weekly schedule always fires within 7 days; 800 days is a
// generous bound (the reference design uses the same horizon).
const maxHorizonDays = 800

// LoadLocation resolves the schedule timezone: an empty name means the
// host's local zone, anything else must be a loadable IANA name.
func LoadLocation(name string) (*time.Location, error) {
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown IANA timezone %q", name)
	}
	return loc, nil
}

// ParseClock parses "HH:MM" (0-padded or not) into (hour, minute).
// An empty string means the default fire time 09:00.
func ParseClock(v string) (int, int, error) {
	if v == "" {
		return 9, 0, nil
	}
	// Split and Atoi rather than fmt.Sscanf("%d:%d"): Sscanf stops after the
	// second number and IGNORES the rest, so "9:30pm" parsed as 09:30 — a
	// silently mis-scheduled automation. Atoi rejects the trailing text.
	hPart, mPart, ok := strings.Cut(v, ":")
	if !ok {
		return 0, 0, fmt.Errorf("want HH:MM, got %q", v)
	}
	h, err := strconv.Atoi(strings.TrimSpace(hPart))
	if err != nil {
		return 0, 0, fmt.Errorf("want HH:MM, got %q", v)
	}
	m, err := strconv.Atoi(strings.TrimSpace(mPart))
	if err != nil {
		return 0, 0, fmt.Errorf("want HH:MM, got %q", v)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", v)
	}
	return h, m, nil
}

// ParseWeekdays parses a comma-separated weekday list ("1,3,5",
// 0 = Sunday … 6 = Saturday) into a sorted, de-duplicated slice.
func ParseWeekdays(v string) ([]int, error) {
	seen := map[int]bool{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var d int
		if _, err := fmt.Sscanf(part, "%d", &d); err != nil || d < 0 || d > 6 {
			return nil, fmt.Errorf("weekday %q out of range (0 = Sunday … 6 = Saturday)", part)
		}
		seen[d] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("need at least one weekday")
	}
	out := make([]int, 0, len(seen))
	for d := 0; d <= 6; d++ {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// ParseAt parses the `--at` instant for a once schedule. RFC 3339
// ("2026-08-10T09:00:00Z") is the canonical form; the plain local spelling
// "2006-01-02 15:04" is also accepted and interpreted in the schedule's
// timezone (or the host's local zone when no timezone is set).
func ParseAt(v, timezone string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	loc, err := LoadLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", v, loc); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("want RFC 3339 (2026-08-10T09:00:00Z) or \"2006-01-02 15:04\", got %q", v)
}

// NextRun returns the next fire time for the schedule strictly after
// `after`, evaluated in the schedule's timezone, or a zero time when the
// schedule yields no future occurrence (a once schedule in the past, an
// invalid kind). The error is non-nil only for schedules that cannot be
// evaluated at all (unknown kind, broken stored `at`).
//
// All wall-clock arithmetic goes through time.Date in the target zone, so
// DST transitions behave like a human would expect:
//   - spring-forward gap: a daily 02:30 schedule on a day where 02:30 does
//     not exist fires at 03:30 local (Go normalizes the nonexistent time
//     forward — the cron convention);
//   - fall-back overlap: a daily 01:30 schedule on a day with two 01:30s
//     takes the earlier (first) occurrence;
//   - "09:00 Europe/Berlin" stays 09:00 wall-clock all year round.
func NextRun(s Schedule, after time.Time) (time.Time, error) {
	loc, err := LoadLocation(s.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	after = after.In(loc)
	switch s.Kind {
	case KindOnce:
		at, err := ParseAt(s.At, s.Timezone)
		if err != nil {
			return time.Time{}, err
		}
		if at.After(after) {
			return at, nil
		}
		return time.Time{}, nil
	case KindHourly:
		return nextHourly(s.Minute, after, loc), nil
	case KindDaily, KindWeekdays, KindWeekly:
		hh, mm, err := ParseClock(s.Time)
		if err != nil {
			return time.Time{}, err
		}
		var weekdays map[int]bool
		if s.Kind == KindWeekly {
			weekdays = map[int]bool{}
			for _, d := range s.Weekdays {
				weekdays[d] = true
			}
		}
		return nextDaily(s.Kind, hh, mm, weekdays, after, loc), nil
	default:
		return time.Time{}, fmt.Errorf("unknown schedule kind %q", s.Kind)
	}
}

// nextHourly returns the next ":minute" tick after `after`. The first tick
// is wall-pinned via time.Date; subsequent ticks step one real hour at a
// time. Duration stepping keeps the :minute pin across DST shifts (a DST
// shift is a whole hour, so ±1 real hour leaves the wall minute unchanged:
// on a 23-hour day the missing hour is simply skipped).
func nextHourly(minute int, after time.Time, loc *time.Location) time.Time {
	if minute < 0 {
		minute = 0
	} else if minute > 59 {
		minute = 59
	}
	candidate := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), minute, 0, 0, loc)
	// Three real-hour bumps always suffice: the worst case is a candidate
	// whose wall construction collapsed onto `after` itself across a DST
	// gap or overlap (one bump past it), plus slack.
	for i := 0; i < 4; i++ {
		if candidate.After(after) {
			return candidate
		}
		candidate = candidate.Add(time.Hour)
	}
	// Unreachable: every bump is strictly one real hour forward.
	return time.Time{}
}

// fixDSTGap repairs a wall-pinned candidate that time.Date normalized out
// of existence. Go resolves a nonexistent local time (a spring-forward gap,
// e.g. 02:30 America/New_York on 2025-03-09) to the instant with the
// POST-transition offset — 02:30 EDT = 06:30Z, which displays as 01:30 EST,
// one wall-clock hour EARLIER than requested. Firing then would run the job
// an hour early; the cron convention is to fire just after the gap (03:30),
// so a candidate whose wall clock reads before the requested time is bumped
// one real hour forward. A candidate that normalized LATER (an
// implementation picking the other side of the gap) is left as-is — an
// hour late is acceptable, an hour early is not.
func fixDSTGap(candidate time.Time, hh, mm int, loc *time.Location) time.Time {
	w := candidate.In(loc)
	if w.Hour()*60+w.Minute() >= hh*60+mm {
		return candidate
	}
	return candidate.Add(time.Hour)
}

// nextDayActive reports whether the calendar day of `day` fires for the
// given kind (daily = always; weekdays = Monday–Friday; weekly = the
// configured weekday set).
func nextDayActive(kind string, weekdays map[int]bool, day time.Time) bool {
	switch kind {
	case KindDaily:
		return true
	case KindWeekdays:
		dow := int(day.Weekday()) // 0 = Sunday … 6 = Saturday
		return dow >= 1 && dow <= 5
	case KindWeekly:
		return weekdays[int(day.Weekday())]
	default:
		return false
	}
}

// nextDaily scans forward day by day (bounded) for the next active day
// whose wall-clock fire time is strictly after `after`.
func nextDaily(kind string, hh, mm int, weekdays map[int]bool, after time.Time, loc *time.Location) time.Time {
	for i := 0; i < maxHorizonDays; i++ {
		day := after.AddDate(0, 0, i) // calendar arithmetic: keeps the zone, normalizes overflows
		if !nextDayActive(kind, weekdays, day) {
			continue
		}
		candidate := fixDSTGap(time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, loc), hh, mm, loc)
		if candidate.After(after) {
			return candidate
		}
	}
	// The bounded search found nothing (only possible for a hand-edited
	// record no validator would store, e.g. a weekly schedule whose weekday
	// list is somehow empty after normalization).
	return time.Time{}
}
