package billing

import (
	"fmt"
	"time"
)

// Period is a half-open monthly billing interval.
type Period struct{ From, To time.Time }

// MonthlyPeriod determines the period containing now for an original boarding
// anchor. The anchor's day number is retained across short months (31st -> last
// day of February -> 31st again), avoiding cumulative date drift.
func MonthlyPeriod(anchor, now time.Time, location *time.Location) (Period, error) {
	if anchor.IsZero() || now.IsZero() {
		return Period{}, fmt.Errorf("billing: anchor and now are required")
	}
	if location == nil {
		location = time.UTC
	}
	anchorLocal := anchor.In(location)
	if now.Before(anchor) { // First period starts exactly at the boarding instant.
		to := anniversary(anchorLocal, monthIndex(anchorLocal)+1, location)
		return Period{From: anchor, To: strictlyAfter(anchor, to, anchorLocal, monthIndex(anchorLocal)+1, location)}, nil
	}
	idx := monthIndex(now.In(location))
	start := anniversary(anchorLocal, idx, location)
	if start.Before(anchor) { // Clamp can produce a date before a late-month anchor.
		idx++
		start = anniversary(anchorLocal, idx, location)
	}
	if now.Before(start) {
		idx--
		start = anniversary(anchorLocal, idx, location)
		if start.Before(anchor) {
			start = anchor
		}
	}
	end := anniversary(anchorLocal, idx+1, location)
	end = strictlyAfter(start, end, anchorLocal, idx+1, location)
	return Period{From: start.UTC(), To: end.UTC()}, nil
}

// Anniversary returns the local monthly anniversary for a month index. The
// index is absolute (year*12 + month-1), so it does not drift after clamping.
func Anniversary(anchor time.Time, index int, location *time.Location) time.Time {
	if location == nil {
		location = time.UTC
	}
	return anniversary(anchor.In(location), index, location)
}

func monthIndex(t time.Time) int { return t.Year()*12 + int(t.Month()) - 1 }
func anniversary(anchor time.Time, index int, location *time.Location) time.Time {
	year, month := index/12, time.Month(index%12+1)
	day := anchor.Day()
	max := daysInMonth(year, month)
	if day > max {
		day = max
	}
	return time.Date(year, month, day, anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), location)
}
func strictlyAfter(from, candidate, anchor time.Time, index int, location *time.Location) time.Time {
	for !candidate.After(from) {
		index++
		candidate = anniversary(anchor, index, location)
	}
	return candidate.UTC()
}
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
