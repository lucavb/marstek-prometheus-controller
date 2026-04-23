// Package schedule implements a minimal 5-field UTC cron parser used to
// schedule periodic device-restart commands. It is intentionally narrow:
// timezone awareness is the caller's responsibility — pass time.Now().In(loc)
// to Next and it operates in that location's wall clock.
//
// Field order: minute hour day-of-month month day-of-week
//
// Supported syntax per field:
//   - *           wildcard (every value)
//   - N           single value
//   - N-M         inclusive range
//   - N,M,…       comma-separated list (each element may itself be a range)
//   - */S or N-M/S  step
//
// Day-of-week: 0–6, Sunday=0; 7 is also accepted as Sunday (Vixie compat).
// DOM/DOW Vixie semantics: when both DOM and DOW are restricted (non-wildcard),
// either matching is sufficient to fire.
package schedule

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// field bounds
const (
	minMinute = 0
	maxMinute = 59
	minHour   = 0
	maxHour   = 23
	minDOM    = 1
	maxDOM    = 31
	minMonth  = 1
	maxMonth  = 12
	minDOW    = 0
	maxDOW    = 6
)

// Schedule holds the parsed bitsets for each cron field.
// Each bit N being set means "value N is included in this field."
type Schedule struct {
	minute uint64 // bits 0–59
	hour   uint32 // bits 0–23
	dom    uint32 // bits 1–31  (bit 0 unused)
	month  uint16 // bits 1–12  (bit 0 unused)
	dow    uint8  // bits 0–6

	// wildcardDOM and wildcardDOW track whether the original spec was "*"
	// for Vixie DOM/DOW OR semantics.
	wildcardDOM bool
	wildcardDOW bool
}

// Parse parses a 5-field cron spec and returns a Schedule ready for use.
// Returns a descriptive error on any syntax or range violation.
func Parse(spec string) (*Schedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule: expected 5 fields, got %d in %q", len(fields), spec)
	}

	s := &Schedule{}
	var err error

	s.minute, err = parseField64(fields[0], minMinute, maxMinute)
	if err != nil {
		return nil, fmt.Errorf("schedule: minute field: %w", err)
	}
	h32, err := parseField32(fields[1], minHour, maxHour)
	if err != nil {
		return nil, fmt.Errorf("schedule: hour field: %w", err)
	}
	s.hour = h32

	s.wildcardDOM = fields[2] == "*"
	dom32, err := parseField32(fields[2], minDOM, maxDOM)
	if err != nil {
		return nil, fmt.Errorf("schedule: day-of-month field: %w", err)
	}
	s.dom = dom32

	month16, err := parseField16(fields[3], minMonth, maxMonth)
	if err != nil {
		return nil, fmt.Errorf("schedule: month field: %w", err)
	}
	s.month = month16

	s.wildcardDOW = fields[4] == "*"
	dow8, err := parseDOW(fields[4])
	if err != nil {
		return nil, fmt.Errorf("schedule: day-of-week field: %w", err)
	}
	s.dow = dow8

	return s, nil
}

// Next returns the next time strictly after `after` at which the schedule
// fires, evaluated in after.Location(). Callers control the timezone:
//
//	sched.Next(time.Now().In(loc))
//
// Returns a zero time if no valid instant can be found within ~4 years
// (should only happen for pathological specs like "0 0 31 2 *").
func (s *Schedule) Next(after time.Time) time.Time {
	loc := after.Location()

	// Advance to the next whole minute.
	t := after.Truncate(time.Minute).Add(time.Minute)

	// Safety cap: ~4 years of minutes.
	const maxIterations = 4 * 366 * 24 * 60
	for i := 0; i < maxIterations; i++ {
		// Month check (1–12).
		if s.month&(1<<uint(t.Month())) == 0 {
			// Advance to the first day of the next valid month.
			t = s.nextValidMonth(t, loc)
			if t.IsZero() {
				return time.Time{}
			}
			continue
		}

		// DOM / DOW check — Vixie semantics.
		domOK := s.wildcardDOM || s.dom&(1<<uint(t.Day())) != 0
		dowOK := s.wildcardDOW || s.dow&(1<<uint(t.Weekday())) != 0
		dayOK := false
		if s.wildcardDOM && s.wildcardDOW {
			dayOK = true
		} else if s.wildcardDOM {
			dayOK = dowOK
		} else if s.wildcardDOW {
			dayOK = domOK
		} else {
			dayOK = domOK || dowOK
		}
		if !dayOK {
			// Advance to midnight of the next calendar day.
			y, m, d := t.Date()
			t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
			continue
		}

		// Hour check.
		if s.hour&(1<<uint(t.Hour())) == 0 {
			// Advance to the next valid hour (start of that hour).
			y, m, d := t.Date()
			h := t.Hour() + 1
			t = time.Date(y, m, d, h, 0, 0, 0, loc)
			continue
		}

		// Minute check.
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}

		return t
	}
	return time.Time{}
}

// nextValidMonth advances t to midnight on the 1st of the next month that
// matches the schedule's month bitset. Returns zero time if none found in 12
// steps (impossible in a valid bitset, but guards against bugs).
func (s *Schedule) nextValidMonth(t time.Time, loc *time.Location) time.Time {
	y, m, _ := t.Date()
	for i := 0; i < 13; i++ {
		m++
		if m > 12 {
			m = 1
			y++
		}
		if s.month&(1<<uint(m)) != 0 {
			return time.Date(y, m, 1, 0, 0, 0, 0, loc)
		}
	}
	return time.Time{}
}

// ---- field parsers ----

func parseField64(field string, lo, hi int) (uint64, error) {
	var mask uint64
	for _, part := range strings.Split(field, ",") {
		bits, err := parsePart64(part, lo, hi)
		if err != nil {
			return 0, err
		}
		mask |= bits
	}
	if mask == 0 {
		return 0, fmt.Errorf("field %q matches no values in [%d,%d]", field, lo, hi)
	}
	return mask, nil
}

func parseField32(field string, lo, hi int) (uint32, error) {
	v64, err := parseField64(field, lo, hi)
	return uint32(v64), err
}

func parseField16(field string, lo, hi int) (uint16, error) {
	v64, err := parseField64(field, lo, hi)
	return uint16(v64), err
}

func parseDOW(field string) (uint8, error) {
	// Normalise 7 → 0 (Sunday).
	normalised := normaliseDOW(field)
	v64, err := parseField64(normalised, minDOW, maxDOW)
	return uint8(v64), err
}

// normaliseDOW replaces standalone "7" tokens with "0" so Sunday works either way.
func normaliseDOW(field string) string {
	parts := strings.Split(field, ",")
	for i, p := range parts {
		// Handle step suffix first: e.g. "0-7/1" — replace 7 in range end only.
		step := ""
		if idx := strings.Index(p, "/"); idx >= 0 {
			step = p[idx:]
			p = p[:idx]
		}
		if p == "7" {
			parts[i] = "0" + step
		} else if strings.HasSuffix(p, "-7") {
			parts[i] = p[:len(p)-1] + "6" + step // treat range end 7 as 6
		} else {
			parts[i] = p + step
		}
	}
	return strings.Join(parts, ",")
}

func parsePart64(part string, lo, hi int) (uint64, error) {
	// Split off step.
	step := 1
	if idx := strings.Index(part, "/"); idx >= 0 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return 0, fmt.Errorf("invalid step in %q", part)
		}
		step = s
		part = part[:idx]
	}

	var start, end int
	if part == "*" {
		start, end = lo, hi
	} else if idx := strings.Index(part, "-"); idx >= 0 {
		var err error
		start, err = strconv.Atoi(part[:idx])
		if err != nil {
			return 0, fmt.Errorf("invalid range start in %q", part)
		}
		end, err = strconv.Atoi(part[idx+1:])
		if err != nil {
			return 0, fmt.Errorf("invalid range end in %q", part)
		}
	} else {
		v, err := strconv.Atoi(part)
		if err != nil {
			return 0, fmt.Errorf("invalid value %q", part)
		}
		start, end = v, v
	}

	if start < lo || end > hi || start > end {
		return 0, fmt.Errorf("value %d-%d out of range [%d,%d]", start, end, lo, hi)
	}

	var mask uint64
	for v := start; v <= end; v += step {
		mask |= 1 << uint(v)
	}
	return mask, nil
}

// BitsSet returns the number of distinct values set in a field mask.
// Exported for testing only.
func BitsSet(mask uint64) int { return bits.OnesCount64(mask) }
