package api

import (
	"testing"
	"time"
)

func TestReportSinceUsesUTCForDefaultCutoff(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			"spring_DST_transition",
			time.Date(2026, time.March, 20, 12, 0, 0, 0, newYork),
			time.Date(2026, time.February, 18, 16, 0, 0, 0, time.UTC),
		},
		{
			"autumn_DST_transition",
			time.Date(2026, time.November, 10, 12, 0, 0, 0, newYork),
			time.Date(2026, time.October, 11, 17, 0, 0, 0, time.UTC),
		},
		{
			"positive_offset_preserves_instant",
			time.Date(2026, time.March, 15, 14, 30, 0, 0, time.FixedZone("UTC+14", 14*60*60)),
			time.Date(2026, time.February, 13, 0, 30, 0, 0, time.UTC),
		},
		{
			"nanosecond_precision",
			time.Date(2026, time.May, 5, 4, 3, 2, 123456789, time.UTC),
			time.Date(2026, time.April, 5, 4, 3, 2, 123456789, time.UTC),
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given a current instant, when no since date is supplied,
			// then the default cutoff is exactly 30 UTC calendar days earlier.
			got, err := reportSince("", scenario.now)
			if err != nil || !got.Equal(scenario.want) || got.Location() != time.UTC {
				t.Errorf("cutoff=%v (%v), want %v; err=%v", got, got.Location(), scenario.want, err)
			}
		})
	}
}

func TestReportSinceParsesExplicitDatesInUTC(t *testing.T) {
	// Given a date-only query, its midnight boundary is independent of the
	// operator's clock/timezone and accepts valid leap days.
	now := time.Date(2030, time.July, 1, 12, 0, 0, 0, time.FixedZone("UTC-8", -8*60*60))
	got, err := reportSince("2024-02-29", now)
	want := time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)
	if err != nil || !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("explicit cutoff=%v, want %v; err=%v", got, want, err)
	}
	for _, value := range []string{"2026-02-29", "2026-02-30", "2026-01-01T00:00:00Z", "tomorrow"} {
		t.Run(value, func(t *testing.T) {
			if _, err := reportSince(value, now); err == nil {
				t.Errorf("invalid date %q accepted", value)
			}
		})
	}
}
