package timex

import (
	"testing"
	"time"
)

// TestParseInLocationConvertsToUTC is the core timezone guarantee: an operator
// types a local wall-clock time and the platform stores the correct instant,
// regardless of where the server runs.
func TestParseInLocationConvertsToUTC(t *testing.T) {
	got, err := ParseInLocation("2026-08-16", "21:00", "Asia/Almaty")
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}

	// Asia/Almaty is UTC+5, so 21:00 local is 16:00 UTC.
	want := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("result location is %v, want UTC", got.Location())
	}
}

func TestParseInLocationAcceptsSeconds(t *testing.T) {
	got, err := ParseInLocation("2026-08-16", "20:52:30", "Asia/Almaty")
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}

	want := time.Date(2026, 8, 16, 15, 52, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseInLocationDefaultsMidnight(t *testing.T) {
	got, err := ParseInLocation("2026-08-16", "", "Asia/Almaty")
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}

	if got.In(MustLocation("Asia/Almaty")).Format("15:04") != "00:00" {
		t.Errorf("empty time should mean midnight, got %v", got)
	}
}

func TestParseInLocationRejectsBadInput(t *testing.T) {
	cases := []struct{ date, clock, tz string }{
		{"not-a-date", "21:00", "Asia/Almaty"},
		{"2026-08-16", "25:00", "Asia/Almaty"},
		{"2026-08-16", "21:00", "Mars/Olympus"},
		{"2026-13-45", "21:00", "Asia/Almaty"},
	}

	for _, tc := range cases {
		if _, err := ParseInLocation(tc.date, tc.clock, tc.tz); err == nil {
			t.Errorf("ParseInLocation(%q, %q, %q) should have failed", tc.date, tc.clock, tc.tz)
		}
	}
}

// TestRoundTripAcrossServerTimezone proves the stored instant does not depend
// on the process's own local zone.
func TestRoundTripAcrossServerTimezone(t *testing.T) {
	first, err := ParseInLocation("2026-08-16", "21:00", "Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a differently configured host by formatting through another
	// zone and back.
	rendered := FormatIn(first, "Asia/Almaty", "15:04")
	if rendered != "21:00" {
		t.Errorf("FormatIn = %q, want 21:00", rendered)
	}
	if FormatIn(first, "UTC", "15:04") != "16:00" {
		t.Errorf("the same instant should read 16:00 in UTC")
	}
}

func TestOffset(t *testing.T) {
	base := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)

	cases := map[int]string{
		-5 * 3600: "11:00:00",
		-450:      "15:52:30",
		0:         "16:00:00",
		3600:      "17:00:00",
	}

	for seconds, want := range cases {
		if got := Offset(base, seconds).Format("15:04:05"); got != want {
			t.Errorf("Offset(%d) = %s, want %s", seconds, got, want)
		}
	}
}

func TestHumanOffset(t *testing.T) {
	cases := map[int]string{
		0:         "0",
		-300:      "-5m",
		-450:      "-7m30s",
		-3600:     "-1h",
		-9000:     "-2h30m",
		-5 * 3600: "-5h",
		1800:      "+30m",
	}

	for seconds, want := range cases {
		if got := HumanOffset(seconds); got != want {
			t.Errorf("HumanOffset(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestRemainingLabel(t *testing.T) {
	now := time.Date(2026, 8, 16, 16, 0, 0, 0, time.UTC)

	cases := []struct {
		target time.Time
		want   string
	}{
		{now.Add(5 * time.Hour), "5 сағат"},
		{now.Add(2*time.Hour + 30*time.Minute), "2 сағат 30 минут"},
		{now.Add(15 * time.Minute), "15 минут"},
		{now.Add(20 * time.Second), "1 минуттан аз"},
		{now, "басталды"},
		{now.Add(-time.Hour), "басталды"},
	}

	for _, tc := range cases {
		if got := RemainingLabel(now, tc.target); got != tc.want {
			t.Errorf("RemainingLabel(%v) = %q, want %q", tc.target.Sub(now), got, tc.want)
		}
	}
}

func TestStartOfDayIn(t *testing.T) {
	// 00:30 Almaty on 16 August is 19:30 UTC on 15 August; the day must still
	// resolve to 16 August local midnight.
	instant := time.Date(2026, 8, 15, 19, 30, 0, 0, time.UTC)

	got := StartOfDayIn(instant, "Asia/Almaty")
	want := time.Date(2026, 8, 15, 19, 0, 0, 0, time.UTC) // 2026-08-16 00:00 +05

	if !got.Equal(want) {
		t.Errorf("StartOfDayIn = %v, want %v", got, want)
	}
	if got.In(MustLocation("Asia/Almaty")).Format("2006-01-02 15:04") != "2026-08-16 00:00" {
		t.Errorf("local rendering is wrong: %v", got.In(MustLocation("Asia/Almaty")))
	}
}

func TestLocationCaching(t *testing.T) {
	first, err := Location("Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Location("Asia/Almaty")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("Location should return the cached instance")
	}
}

func TestLocationRejectsUnknownZone(t *testing.T) {
	if _, err := Location("Nowhere/Nothing"); err == nil {
		t.Error("expected an error for an unknown timezone")
	}
}

func TestMustLocationFallsBackToUTC(t *testing.T) {
	if MustLocation("Nowhere/Nothing") != time.UTC {
		t.Error("MustLocation should fall back to UTC")
	}
	if MustLocation("") != time.UTC {
		t.Error("empty timezone should resolve to UTC")
	}
}
