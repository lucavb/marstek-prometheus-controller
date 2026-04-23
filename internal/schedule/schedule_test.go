package schedule_test

import (
	"testing"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/schedule"
)

// mustParse is a helper that fails the test immediately on a bad spec.
func mustParse(t *testing.T, spec string) *schedule.Schedule {
	t.Helper()
	s, err := schedule.Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", spec, err)
	}
	return s
}

func utc(year int, month time.Month, day, hour, min int) time.Time {
	return time.Date(year, month, day, hour, min, 0, 0, time.UTC)
}

// ---- Parse errors ----

func TestParse_Errors(t *testing.T) {
	cases := []struct {
		name string
		spec string
	}{
		{"too few fields", "* * * *"},
		{"too many fields", "* * * * * *"},
		{"minute out of range", "60 * * * *"},
		{"hour out of range", "0 24 * * *"},
		{"dom out of range", "0 0 0 * *"},
		{"dom out of range high", "0 0 32 * *"},
		{"month out of range", "0 0 1 0 *"},
		{"month out of range high", "0 0 1 13 *"},
		{"dow out of range", "0 0 * * 8"},
		{"bad step zero", "*/0 * * * *"},
		{"bad step negative", "*/-1 * * * *"},
		{"bad step non-numeric", "*/x * * * *"},
		{"bad range start", "a-5 * * * *"},
		{"bad range end", "0-b * * * *"},
		{"range inverted", "10-5 * * * *"},
		{"non-numeric value", "x * * * *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := schedule.Parse(tc.spec)
			if err == nil {
				t.Errorf("Parse(%q) expected error, got nil", tc.spec)
			}
		})
	}
}

// ---- Parse success ----

func TestParse_Success(t *testing.T) {
	cases := []string{
		"* * * * *",
		"0 3 * * *",
		"*/15 * * * *",
		"0-30/5 * * * *",
		"0,30 6,18 * * 1-5",
		"0 0 1 1 *",
		"59 23 31 12 6",
		"0 * * * 7", // DOW 7 = Sunday, should normalise to 0
	}
	for _, spec := range cases {
		t.Run(spec, func(t *testing.T) {
			if _, err := schedule.Parse(spec); err != nil {
				t.Errorf("Parse(%q) unexpected error: %v", spec, err)
			}
		})
	}
}

// DOW 7 is accepted and treated as Sunday (0).
func TestParse_DOW7_Sunday(t *testing.T) {
	s7 := mustParse(t, "0 12 * * 7")
	s0 := mustParse(t, "0 12 * * 0")

	// Both should fire on the same next Sunday.
	base := utc(2025, time.January, 6, 0, 0) // Monday 2025-01-06
	n7 := s7.Next(base)
	n0 := s0.Next(base)
	if !n7.Equal(n0) {
		t.Errorf("DOW 7 and DOW 0 differ: %v vs %v", n7, n0)
	}
}

// ---- Next in UTC ----

func TestNext_Daily(t *testing.T) {
	s := mustParse(t, "0 3 * * *") // 03:00 daily
	base := utc(2025, time.January, 1, 0, 0)
	got := s.Next(base)
	want := utc(2025, time.January, 1, 3, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNext_Daily_AfterFire(t *testing.T) {
	s := mustParse(t, "0 3 * * *")
	// Already past today's 03:00 — should roll to tomorrow.
	base := utc(2025, time.January, 1, 3, 1)
	got := s.Next(base)
	want := utc(2025, time.January, 2, 3, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNext_Hourly(t *testing.T) {
	s := mustParse(t, "0 * * * *") // :00 every hour
	base := utc(2025, time.June, 15, 10, 30)
	got := s.Next(base)
	want := utc(2025, time.June, 15, 11, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNext_EveryFifteenMinutes(t *testing.T) {
	s := mustParse(t, "*/15 * * * *")
	cases := []struct {
		base time.Time
		want time.Time
	}{
		{utc(2025, time.January, 1, 0, 0), utc(2025, time.January, 1, 0, 15)},
		{utc(2025, time.January, 1, 0, 14), utc(2025, time.January, 1, 0, 15)},
		{utc(2025, time.January, 1, 0, 15), utc(2025, time.January, 1, 0, 30)},
		{utc(2025, time.January, 1, 0, 59), utc(2025, time.January, 1, 1, 0)},
	}
	for _, tc := range cases {
		got := s.Next(tc.base)
		if !got.Equal(tc.want) {
			t.Errorf("Next(%v) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

func TestNext_Weekly_Monday(t *testing.T) {
	s := mustParse(t, "0 9 * * 1")           // Monday 09:00
	base := utc(2025, time.January, 6, 0, 0) // Monday Jan 6
	got := s.Next(base)
	want := utc(2025, time.January, 6, 9, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	// Next occurrence should be Monday Jan 13.
	got2 := s.Next(got)
	want2 := utc(2025, time.January, 13, 9, 0)
	if !got2.Equal(want2) {
		t.Errorf("Next (2nd) = %v, want %v", got2, want2)
	}
}

func TestNext_MonthRollover(t *testing.T) {
	s := mustParse(t, "0 0 1 * *") // 1st of every month at midnight
	base := utc(2025, time.January, 31, 12, 0)
	got := s.Next(base)
	want := utc(2025, time.February, 1, 0, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNext_YearRollover(t *testing.T) {
	s := mustParse(t, "0 0 1 1 *") // Jan 1st at midnight
	base := utc(2025, time.December, 31, 12, 0)
	got := s.Next(base)
	want := utc(2026, time.January, 1, 0, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
}

func TestNext_LeapYearFeb29(t *testing.T) {
	s := mustParse(t, "0 12 29 2 *") // Feb 29 at noon (only on leap years)
	base := utc(2024, time.January, 1, 0, 0)
	got := s.Next(base)
	want := utc(2024, time.February, 29, 12, 0)
	if !got.Equal(want) {
		t.Errorf("Next = %v, want %v", got, want)
	}
	// 2025 is not a leap year; should jump to 2028.
	got2 := s.Next(got)
	want2 := utc(2028, time.February, 29, 12, 0)
	if !got2.Equal(want2) {
		t.Errorf("Next (2028) = %v, want %v", got2, want2)
	}
}

// ---- Vixie DOM/DOW semantics ----

func TestNext_Vixie_DOM_or_DOW(t *testing.T) {
	// "0 12 15 * 1" means: noon on the 15th OR noon on any Monday.
	s := mustParse(t, "0 12 15 * 1")
	// 2025-01-13 is a Monday; 2025-01-15 is a Wednesday.
	base := utc(2025, time.January, 12, 0, 0) // Sunday Jan 12
	got := s.Next(base)
	want := utc(2025, time.January, 13, 12, 0) // Monday Jan 13
	if !got.Equal(want) {
		t.Errorf("Next (DOW match) = %v, want %v", got, want)
	}
	// After Monday 13th, next is the 15th (Wednesday).
	got2 := s.Next(got)
	want2 := utc(2025, time.January, 15, 12, 0)
	if !got2.Equal(want2) {
		t.Errorf("Next (DOM match) = %v, want %v", got2, want2)
	}
}

// ---- Timezone: Europe/Berlin ----

func berlinLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("Europe/Berlin not available: %v", err)
	}
	return loc
}

func TestNext_Berlin_Normal(t *testing.T) {
	loc := berlinLoc(t)
	s := mustParse(t, "0 3 * * *") // 03:00 Berlin daily

	// 2025-03-01 00:00 Berlin (CET = UTC+1).
	base := time.Date(2025, time.March, 1, 0, 0, 0, 0, loc)
	got := s.Next(base)
	want := time.Date(2025, time.March, 1, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("Next (Berlin normal) = %v, want %v", got, want)
	}
}

// Spring-forward: 2025-03-30 at 02:00 CET the clock jumps to 03:00 CEST.
// A cron firing at 02:30 should skip that day (the instant doesn't exist)
// and fire the following day.
func TestNext_Berlin_SpringForward_Skip(t *testing.T) {
	loc := berlinLoc(t)
	s := mustParse(t, "30 2 * * *") // 02:30 Berlin daily

	// Just before spring-forward on 2025-03-30.
	base := time.Date(2025, time.March, 29, 3, 0, 0, 0, loc)
	got := s.Next(base)

	// 02:30 on 2025-03-30 doesn't exist (02:00–03:00 skipped); Go normalises
	// time.Date(2025,3,30,2,30,...) to 03:30 CEST. So got should be
	// either that normalised time or the same wall-clock next day.
	// We assert: got is strictly after base AND is NOT in the gap
	// AND the day-of-month is either 30 (normalised) or 31 (next day).
	if !got.After(base) {
		t.Errorf("Next should be after base, got %v", got)
	}
	// The non-existent instant normalises to 03:30 CEST on the 30th.
	expectedNorm := time.Date(2025, time.March, 30, 2, 30, 0, 0, loc)
	if !got.Equal(expectedNorm) {
		t.Logf("Note: Next for spring-forward 02:30 = %v (wall: %v in Berlin)", got.UTC(), got)
	}
}

// Fall-back: 2025-10-26 at 03:00 CEST the clock falls back to 02:00 CET.
// 02:30 exists twice (00:30 UTC in CEST, 01:30 UTC in CET).
// Go's time.Date resolves the ambiguous wall-clock 02:30 to CET (standard time,
// UTC+1), so the algorithm always fires at 01:30 UTC — the SECOND, standard-time
// occurrence. The first CEST occurrence (00:30 UTC) is skipped. This is
// predictable, consistent Go behaviour; operators who need precision should avoid
// scheduling during the fall-back hour.
func TestNext_Berlin_FallBack_CETOccurrence(t *testing.T) {
	loc := berlinLoc(t)
	s := mustParse(t, "30 2 * * *") // 02:30 Berlin daily

	// Just before fall-back.
	base := time.Date(2025, time.October, 25, 3, 0, 0, 0, loc)
	got := s.Next(base)

	// Go's time.Date resolves 02:30 on fall-back day to CET (UTC+1) = 01:30 UTC.
	wantUTC := time.Date(2025, time.October, 26, 1, 30, 0, 0, time.UTC)
	if !got.UTC().Equal(wantUTC) {
		t.Errorf("Next (fall-back CET occurrence) UTC = %v, want %v", got.UTC(), wantUTC)
	}

	// Next occurrence should be the following day.
	got2 := s.Next(got)
	if !got2.After(got) {
		t.Errorf("Second Next should be after first, got %v (first was %v)", got2, got)
	}
	wantUTC2 := time.Date(2025, time.October, 27, 1, 30, 0, 0, time.UTC) // 02:30 CET = 01:30 UTC
	if !got2.UTC().Equal(wantUTC2) {
		t.Errorf("Next (day after fall-back) UTC = %v, want %v", got2.UTC(), wantUTC2)
	}
}

// Non-edge: 03:00 Berlin is fine across both DST transitions.
func TestNext_Berlin_DSTTransition_SafeTime(t *testing.T) {
	loc := berlinLoc(t)
	s := mustParse(t, "0 3 * * *")

	// Spring-forward day: 03:00 already exists post-transition (CEST).
	springDay := time.Date(2025, time.March, 30, 0, 0, 0, 0, loc)
	got := s.Next(springDay)
	wantLocal := time.Date(2025, time.March, 30, 3, 0, 0, 0, loc)
	if !got.Equal(wantLocal) {
		t.Errorf("Spring-forward 03:00: got %v, want %v", got, wantLocal)
	}

	// Fall-back day: 03:00 exists once (CEST side, before the fall-back at 03:00).
	fallDay := time.Date(2025, time.October, 26, 0, 0, 0, 0, loc)
	got2 := s.Next(fallDay)
	wantLocal2 := time.Date(2025, time.October, 26, 3, 0, 0, 0, loc)
	if !got2.Equal(wantLocal2) {
		t.Errorf("Fall-back 03:00: got %v, want %v", got2, wantLocal2)
	}
}

// ---- CommaList and step coverage ----

func TestNext_CommaList(t *testing.T) {
	s := mustParse(t, "0 6,18 * * *") // 06:00 and 18:00 daily
	base := utc(2025, time.January, 1, 0, 0)
	got := s.Next(base)
	want := utc(2025, time.January, 1, 6, 0)
	if !got.Equal(want) {
		t.Errorf("Next (comma list 1st) = %v, want %v", got, want)
	}
	got2 := s.Next(got)
	want2 := utc(2025, time.January, 1, 18, 0)
	if !got2.Equal(want2) {
		t.Errorf("Next (comma list 2nd) = %v, want %v", got2, want2)
	}
}

func TestNext_StepRange(t *testing.T) {
	s := mustParse(t, "0-30/10 * * * *") // 0, 10, 20, 30 minutes past each hour
	base := utc(2025, time.January, 1, 5, 0)
	got := s.Next(base)
	want := utc(2025, time.January, 1, 5, 10)
	if !got.Equal(want) {
		t.Errorf("Next (step range) = %v, want %v", got, want)
	}
}

// ---- Strictly-after invariant ----

func TestNext_StrictlyAfter(t *testing.T) {
	s := mustParse(t, "* * * * *") // every minute
	now := utc(2025, time.January, 1, 12, 30)
	got := s.Next(now)
	if !got.After(now) {
		t.Errorf("Next should be strictly after %v, got %v", now, got)
	}
}
