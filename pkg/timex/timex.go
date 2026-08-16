// Package timex centralises timezone handling. Every timestamp in the
// database is UTC; campaign-local times exist only at the edges.
package timex

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	// DateLayout and TimeLayout are the wire formats the admin UI submits.
	DateLayout     = "2006-01-02"
	TimeLayout     = "15:04"
	TimeSecLayout  = "15:04:05"
	DateTimeLayout = "2006-01-02 15:04"
)

var (
	locCacheMu sync.RWMutex
	locCache   = map[string]*time.Location{}
)

// Location loads and caches an IANA timezone.
func Location(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}

	locCacheMu.RLock()
	loc, ok := locCache[name]
	locCacheMu.RUnlock()
	if ok {
		return loc, nil
	}

	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q: %w", name, err)
	}

	locCacheMu.Lock()
	locCache[name] = loc
	locCacheMu.Unlock()
	return loc, nil
}

// MustLocation returns the timezone or falls back to UTC. Use only where a
// bad value has already been rejected by validation.
func MustLocation(name string) *time.Location {
	loc, err := Location(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ParseInLocation converts a "2006-01-02 15:04" wall-clock string in the given
// zone into a UTC instant.
//
// Daylight-saving gaps are resolved by Go's own rules; the returned instant is
// always well defined even when the wall time does not literally exist.
func ParseInLocation(date, clock, tz string) (time.Time, error) {
	loc, err := Location(tz)
	if err != nil {
		return time.Time{}, err
	}

	date = strings.TrimSpace(date)
	clock = strings.TrimSpace(clock)
	if clock == "" {
		clock = "00:00"
	}

	layout := TimeLayout
	if strings.Count(clock, ":") == 2 {
		layout = TimeSecLayout
	}

	t, err := time.ParseInLocation(DateLayout+" "+layout, date+" "+clock, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date/time %q %q: %w", date, clock, err)
	}
	return t.UTC(), nil
}

// FormatIn renders a UTC instant as wall-clock text in the campaign's zone.
func FormatIn(t time.Time, tz, layout string) string {
	if t.IsZero() {
		return ""
	}
	return t.In(MustLocation(tz)).Format(layout)
}

// HumanOffset renders a signed second offset as a compact human label, e.g.
// -300*60 becomes "5 сағат бұрын" style input for the UI layer. It returns the
// numeric shape only; localisation happens in the frontend.
//
// Examples: 0 -> "0", -300 -> "-5m", -9000 -> "-2h30m", 3600 -> "+1h".
func HumanOffset(seconds int) string {
	if seconds == 0 {
		return "0"
	}

	sign := "+"
	if seconds < 0 {
		sign = "-"
		seconds = -seconds
	}

	d := time.Duration(seconds) * time.Second
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)

	var b strings.Builder
	b.WriteString(sign)
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

// Offset applies a signed second offset to an instant.
func Offset(base time.Time, seconds int) time.Time {
	return base.Add(time.Duration(seconds) * time.Second)
}

// HumanDuration renders a positive duration as Kazakh text, used for delays
// measured from a customer's trigger rather than from a fixed clock time.
//
// Examples: 2s -> "2 секунд", 10m -> "10 минут", 90m -> "1 сағат 30 минут".
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "бірден"
	}

	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)

	parts := make([]string, 0, 3)
	if h > 0 {
		parts = append(parts, fmt.Sprintf("%d сағат", h))
	}
	if m > 0 {
		parts = append(parts, fmt.Sprintf("%d минут", m))
	}
	if s > 0 {
		parts = append(parts, fmt.Sprintf("%d секунд", s))
	}
	return strings.Join(parts, " ")
}

// RemainingLabel renders the gap between now and target as Kazakh text used by
// the {{remaining_time}} template variable.
func RemainingLabel(now, target time.Time) string {
	d := target.Sub(now)
	if d <= 0 {
		return "басталды"
	}

	// Round to the nearest minute so "4 сағат 59 минут" does not appear one
	// second after a five-hour job fires.
	d = d.Round(time.Minute)
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)

	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%d сағат %d минут", h, m)
	case h > 0:
		return fmt.Sprintf("%d сағат", h)
	case m > 0:
		return fmt.Sprintf("%d минут", m)
	default:
		return "1 минуттан аз"
	}
}

// StartOfDayIn returns midnight of t's calendar day in tz, as a UTC instant.
func StartOfDayIn(t time.Time, tz string) time.Time {
	loc := MustLocation(tz)
	local := t.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc).UTC()
}
