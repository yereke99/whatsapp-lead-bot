package campaigns

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Recurrence is a campaign's daily-webinar configuration, reduced to the four
// facts the arithmetic needs.
//
// It exists as its own type so the occurrence calculation is pure: no database,
// no clock of its own, no campaign loaded. The same function then serves
// enrolment, the admin preview, the "next webinar" the panel shows and the
// tests, and none of them can drift from the others.
type Recurrence struct {
	Enabled bool
	// Time is the daily start as "HH:MM" (or "HH:MM:SS") wall-clock in Zone.
	Time string
	// StartDate is the first calendar day of the series, "YYYY-MM-DD" in Zone.
	// Empty means the series has no lower bound of its own.
	StartDate string
	Zone      string
}

// RecurrenceOf reads a campaign's recurrence settings.
//
// The start date falls back to the day of event_start_at, which is the date the
// operator already picked in the existing date field. That is what lets the
// date picker keep its meaning for a recurring campaign — it becomes the day
// the series begins — instead of being removed or ignored.
func RecurrenceOf(c *domain.Campaign) Recurrence {
	r := Recurrence{
		Enabled:   c.IsDailyRecurring,
		Time:      strings.TrimSpace(c.RecurrenceTime),
		StartDate: strings.TrimSpace(c.RecurrenceStartDate),
		Zone:      c.Timezone,
	}
	if r.StartDate == "" && c.EventStartAt != nil {
		r.StartDate = timex.FormatIn(*c.EventStartAt, c.Timezone, timex.DateLayout)
	}
	return r
}

// ErrNoRecurrence reports that a campaign has no daily webinar configured, so
// there is no occurrence to compute. Callers treat it as "use the one-time
// anchor", not as a failure.
var ErrNoRecurrence = errors.New("campaign has no daily recurring webinar")

// Validate rejects a recurrence an operator could not have meant.
//
// It runs on the server on every save. The panel validates too, but client-side
// validation is a convenience for the operator, never a guarantee about what
// reaches the database.
func (r Recurrence) Validate() error {
	if !r.Enabled {
		return nil
	}
	if _, err := timex.Location(r.Zone); err != nil {
		return fmt.Errorf("уақыт белдеуі жарамсыз: %s", r.Zone)
	}
	if strings.TrimSpace(r.Time) == "" {
		return errors.New("күн сайынғы вебинар уақытын көрсетіңіз")
	}
	if _, _, err := parseClock(r.Time); err != nil {
		return fmt.Errorf("вебинар уақыты жарамсыз: %s", r.Time)
	}
	if r.StartDate != "" {
		if _, err := time.Parse(timex.DateLayout, r.StartDate); err != nil {
			return fmt.Errorf("қайталану басталатын күн жарамсыз: %s", r.StartDate)
		}
	}
	return nil
}

// NextOccurrenceAfter returns the first webinar at or after from, as a UTC
// instant.
//
// The recurrence is a *calendar* one: the same wall-clock time on consecutive
// local days, built with Go's zone database. Adding 24h to a UTC timestamp
// would be a different thing entirely — it would drift by an hour the moment a
// zone changes its offset, which is precisely the bug this avoids for zones
// that observe daylight saving. Asia/Almaty does not today, but the campaign's
// zone is the operator's choice and the arithmetic must not assume.
//
// A wall-clock time that does not exist on some day — the hour a zone skips
// when it springs forward — is resolved by Go's own normalisation rather than
// being an error. The webinar still happens; it happens at the instant that
// clock reading maps onto.
func (r Recurrence) NextOccurrenceAfter(from time.Time) (time.Time, error) {
	if !r.Enabled {
		return time.Time{}, ErrNoRecurrence
	}
	if err := r.Validate(); err != nil {
		return time.Time{}, err
	}

	loc, err := timex.Location(r.Zone)
	if err != nil {
		return time.Time{}, err
	}
	hour, minute, err := parseClock(r.Time)
	if err != nil {
		return time.Time{}, err
	}

	local := from.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)

	// The series cannot start before the day the operator set it to, so a
	// campaign configured today for next Monday produces next Monday's webinar
	// rather than today's.
	if r.StartDate != "" {
		start, err := time.ParseInLocation(timex.DateLayout, r.StartDate, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("қайталану басталатын күн жарамсыз: %s", r.StartDate)
		}
		if day.Before(start) {
			day = start
		}
	}

	// Two candidate days are always enough: today's occurrence, or tomorrow's
	// when today's has passed. The third iteration is slack for a zone whose
	// offset shifts between the two, so the loop can never fall through in a
	// way that would silently return the zero time.
	for i := 0; i < 3; i++ {
		occurrence := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if !occurrence.Before(from) {
			return occurrence.UTC(), nil
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, fmt.Errorf("no occurrence of %s found in %s after %s", r.Time, r.Zone, from)
}

// OccurrenceFor picks the webinar a contact arriving at now belongs to.
//
// tail is how long an occurrence keeps claiming new arrivals after it starts:
// the largest step offset the campaign has, so a campaign whose last message
// goes out at the webinar start stops claiming at the webinar start, and one
// with a +1h follow-up keeps claiming for an hour. Expressed that way the rule
// needs no window of its own — "is there still anything this occurrence would
// send you?" is a question the campaign's own steps already answer.
//
// Choosing the current occurrence rather than always jumping to tomorrow is
// what keeps catch-up alive: a contact who writes in at 18:40 for a 21:00
// webinar is late for the 16:00 and 18:00 messages, and the campaign's existing
// catch_up_missed_steps policy decides what they get. Snapping them to
// tomorrow would make that policy unreachable for recurring campaigns.
func (r Recurrence) OccurrenceFor(now time.Time, tail time.Duration) (time.Time, error) {
	if tail < 0 {
		tail = 0
	}
	return r.NextOccurrenceAfter(now.Add(-tail))
}

// StepTail is the largest offset among a campaign's enabled event-anchored
// steps, clamped at zero. It is how long an occurrence still has work to do
// once it has started.
func StepTail(steps []domain.CampaignStep) time.Duration {
	tail := 0
	for _, step := range steps {
		if !step.Enabled || step.ScheduleKind == domain.ScheduleOnTrigger {
			continue
		}
		if step.OffsetSeconds > tail {
			tail = step.OffsetSeconds
		}
	}
	return time.Duration(tail) * time.Second
}

// NextOccurrence is the upcoming webinar of a recurring campaign, for display.
// It returns nil for a one-time campaign, whose upcoming webinar is its
// event_start_at and needs no derivation.
func NextOccurrence(c *domain.Campaign, now time.Time) *time.Time {
	if c == nil || !c.IsDailyRecurring {
		return nil
	}
	next, err := RecurrenceOf(c).NextOccurrenceAfter(now)
	if err != nil {
		return nil
	}
	return &next
}

// EventAnchor is the instant a RELATIVE_TO_EVENT step is measured from for one
// enrolment.
//
// This is the whole of the recurring feature, in four lines. An enrolment that
// pinned an occurrence is scheduled from that occurrence; everything else is
// scheduled from the campaign's event start, exactly as before. Every caller
// that used to read campaign.EventStartAt reads this instead, so the planner,
// the reconciler and the send path can never disagree about which webinar a
// contact is waiting for.
func EventAnchor(campaign *domain.Campaign, enrollment *domain.Enrollment) *time.Time {
	if enrollment != nil && enrollment.OccurrenceAt != nil {
		return enrollment.OccurrenceAt
	}
	return campaign.EventStartAt
}

// PreviewAnchor is the instant the admin panel should render a campaign's
// timeline against: the next occurrence for a recurring series, the fixed event
// start otherwise.
func PreviewAnchor(c *domain.Campaign, now time.Time) *time.Time {
	if next := NextOccurrence(c, now); next != nil {
		return next
	}
	return c.EventStartAt
}

// parseClock reads "HH:MM" or "HH:MM:SS", returning hour and minute. Seconds
// are accepted because the panel's time input may submit them, and discarded
// because a daily webinar starts on the minute.
func parseClock(clock string) (hour, minute int, err error) {
	clock = strings.TrimSpace(clock)
	layout := timex.TimeLayout
	if strings.Count(clock, ":") == 2 {
		layout = timex.TimeSecLayout
	}
	t, err := time.Parse(layout, clock)
	if err != nil {
		return 0, 0, err
	}
	return t.Hour(), t.Minute(), nil
}
