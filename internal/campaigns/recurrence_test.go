package campaigns

import (
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

const almatyTZ = "Asia/Almaty"

func mustLocalTime(t *testing.T, date, clock, tz string) time.Time {
	t.Helper()
	at, err := timex.ParseInLocation(date, clock, tz)
	if err != nil {
		t.Fatalf("parse %s %s %s: %v", date, clock, tz, err)
	}
	return at
}

func dailyAiran() Recurrence {
	return Recurrence{Enabled: true, Time: "21:00", StartDate: "2026-08-18", Zone: almatyTZ}
}

// TestOneTimeCampaignHasNoOccurrence is the backward-compatibility guarantee:
// a campaign nobody has switched over is anchored to its event start and the
// recurrence machinery declines to answer at all.
func TestOneTimeCampaignHasNoOccurrence(t *testing.T) {
	event := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ)
	campaign := &domain.Campaign{Timezone: almatyTZ, EventStartAt: &event}

	if next := NextOccurrence(campaign, time.Now().UTC()); next != nil {
		t.Fatalf("NextOccurrence = %v, want nil for a one-time campaign", next)
	}
	if got := EventAnchor(campaign, &domain.Enrollment{}); !got.Equal(event) {
		t.Fatalf("EventAnchor = %v, want the campaign event start %v", got, event)
	}
	if got := PreviewAnchor(campaign, time.Now().UTC()); !got.Equal(event) {
		t.Fatalf("PreviewAnchor = %v, want %v", got, event)
	}

	if _, err := (Recurrence{Enabled: false}).NextOccurrenceAfter(time.Now()); err != ErrNoRecurrence {
		t.Fatalf("NextOccurrenceAfter error = %v, want ErrNoRecurrence", err)
	}
}

// TestDailyOccurrenceRollsOverEachDay is the feature in one assertion: the same
// wall-clock time, every calendar day, without the campaign being duplicated.
func TestDailyOccurrenceRollsOverEachDay(t *testing.T) {
	r := dailyAiran()

	cases := []struct {
		name string
		now  string // local "date time"
		want string
	}{
		{"before today's webinar", "2026-08-18 16:00", "2026-08-18 21:00"},
		{"one minute before", "2026-08-18 20:59", "2026-08-18 21:00"},
		{"exactly at the start", "2026-08-18 21:00", "2026-08-18 21:00"},
		{"after today's webinar", "2026-08-18 22:30", "2026-08-19 21:00"},
		{"the next morning", "2026-08-19 09:00", "2026-08-19 21:00"},
		{"a week later", "2026-08-25 20:00", "2026-08-25 21:00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := mustLocalTime(t, tc.now[:10], tc.now[11:], almatyTZ)
			got, err := r.NextOccurrenceAfter(now)
			if err != nil {
				t.Fatalf("NextOccurrenceAfter: %v", err)
			}
			want := mustLocalTime(t, tc.want[:10], tc.want[11:], almatyTZ)
			if !got.Equal(want) {
				t.Errorf("occurrence = %s, want %s",
					timex.FormatIn(got, almatyTZ, "2006-01-02 15:04"),
					timex.FormatIn(want, almatyTZ, "2006-01-02 15:04"))
			}
		})
	}
}

// TestStepOffsetsResolveAgainstTheOccurrence covers the acceptance examples:
// -5h is 16:00, -30m is 20:30 and 0 is 21:00 — computed, never hardcoded, and
// the same on every day of the series.
func TestStepOffsetsResolveAgainstTheOccurrence(t *testing.T) {
	offsets := map[string]int{
		"16:00": -5 * 3600,
		"18:00": -3 * 3600,
		"19:00": -2 * 3600,
		"20:00": -1 * 3600,
		"20:30": -30 * 60,
		"20:45": -15 * 60,
		"20:55": -5 * 60,
		"21:00": 0,
	}

	for _, day := range []string{"2026-08-18", "2026-08-19", "2026-08-20"} {
		occurrence := mustLocalTime(t, day, "21:00", almatyTZ)
		for wantClock, offset := range offsets {
			runAt := occurrence.Add(time.Duration(offset) * time.Second)
			gotDate := timex.FormatIn(runAt, almatyTZ, "2006-01-02")
			gotClock := timex.FormatIn(runAt, almatyTZ, "15:04")
			if gotClock != wantClock || gotDate != day {
				t.Errorf("%s offset %ds resolved to %s %s, want %s %s",
					day, offset, gotDate, gotClock, day, wantClock)
			}
		}
	}
}

// TestRecurrenceStartDateBoundsTheSeries: a series configured today for next
// week does not start today.
func TestRecurrenceStartDateBoundsTheSeries(t *testing.T) {
	r := Recurrence{Enabled: true, Time: "21:00", StartDate: "2026-08-25", Zone: almatyTZ}

	got, err := r.NextOccurrenceAfter(mustLocalTime(t, "2026-08-18", "10:00", almatyTZ))
	if err != nil {
		t.Fatalf("NextOccurrenceAfter: %v", err)
	}
	want := mustLocalTime(t, "2026-08-25", "21:00", almatyTZ)
	if !got.Equal(want) {
		t.Fatalf("occurrence = %v, want the series start %v", got, want)
	}
}

// TestOccurrenceForKeepsLateArrivalsOnTodaysWebinar. A contact who writes in at
// 18:40 is late for the 16:00 and 18:00 messages but is still coming to
// tonight's webinar, so the campaign's own catch-up policy gets to decide what
// they receive. Once the last step of the occurrence has passed there is
// nothing left to send them and the next day's webinar is the honest answer.
func TestOccurrenceForKeepsLateArrivalsOnTodaysWebinar(t *testing.T) {
	r := dailyAiran()
	// The Airan funnel ends at the webinar start, so its tail is zero.
	tail := time.Duration(0)

	late := mustLocalTime(t, "2026-08-18", "18:40", almatyTZ)
	got, err := r.OccurrenceFor(late, tail)
	if err != nil {
		t.Fatalf("OccurrenceFor: %v", err)
	}
	if want := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ); !got.Equal(want) {
		t.Errorf("18:40 arrival got %v, want tonight's webinar %v", got, want)
	}

	tooLate := mustLocalTime(t, "2026-08-18", "23:30", almatyTZ)
	got, err = r.OccurrenceFor(tooLate, tail)
	if err != nil {
		t.Fatalf("OccurrenceFor: %v", err)
	}
	if want := mustLocalTime(t, "2026-08-19", "21:00", almatyTZ); !got.Equal(want) {
		t.Errorf("23:30 arrival got %v, want tomorrow's webinar %v", got, want)
	}
}

// TestStepTailExtendsTheClaimingWindow. A campaign with a follow-up after the
// webinar still has something to say to someone arriving during it, so that
// occurrence keeps claiming them for exactly as long as the follow-up is away.
func TestStepTailExtendsTheClaimingWindow(t *testing.T) {
	steps := []domain.CampaignStep{
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: -5 * 3600},
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: 3600},
		// Neither of these counts: one is off, the other is not event-anchored.
		{Enabled: false, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: 9 * 3600},
		{Enabled: true, ScheduleKind: domain.ScheduleOnTrigger, OffsetSeconds: 12 * 3600},
	}
	if got, want := StepTail(steps), time.Hour; got != want {
		t.Fatalf("StepTail = %v, want %v", got, want)
	}

	r := dailyAiran()
	got, err := r.OccurrenceFor(mustLocalTime(t, "2026-08-18", "21:30", almatyTZ), time.Hour)
	if err != nil {
		t.Fatalf("OccurrenceFor: %v", err)
	}
	if want := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ); !got.Equal(want) {
		t.Errorf("21:30 arrival got %v, want tonight's webinar %v (the +1h follow-up is still owed)", got, want)
	}
}

// TestStepTailIsZeroWhenEveryStepPrecedesTheWebinar guards the clamp: a funnel
// of negative offsets must not produce a negative tail and pull the window
// backwards.
func TestStepTailIsZeroWhenEveryStepPrecedesTheWebinar(t *testing.T) {
	steps := []domain.CampaignStep{
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: -5 * 3600},
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: -300},
	}
	if got := StepTail(steps); got != 0 {
		t.Fatalf("StepTail = %v, want 0", got)
	}
	if got := StepTail(nil); got != 0 {
		t.Fatalf("StepTail(nil) = %v, want 0", got)
	}
}

// TestCalendarRecurrenceSurvivesDaylightSaving. Asia/Almaty has no DST, but the
// zone is the operator's choice, so the arithmetic must be a calendar one. In
// Europe/Berlin the clocks go forward on 29 March 2026: successive 21:00
// occurrences are 24 hours apart in wall-clock terms and 23 in elapsed time,
// and it is the wall clock the webinar is announced in.
func TestCalendarRecurrenceSurvivesDaylightSaving(t *testing.T) {
	const berlin = "Europe/Berlin"
	r := Recurrence{Enabled: true, Time: "21:00", StartDate: "2026-03-27", Zone: berlin}

	before, err := r.NextOccurrenceAfter(mustLocalTime(t, "2026-03-28", "10:00", berlin))
	if err != nil {
		t.Fatalf("NextOccurrenceAfter: %v", err)
	}
	after, err := r.NextOccurrenceAfter(before.Add(time.Second))
	if err != nil {
		t.Fatalf("NextOccurrenceAfter: %v", err)
	}

	if got := timex.FormatIn(after, berlin, "2006-01-02 15:04"); got != "2026-03-29 21:00" {
		t.Errorf("occurrence after the clock change = %s, want 2026-03-29 21:00 local", got)
	}
	if elapsed := after.Sub(before); elapsed != 23*time.Hour {
		t.Errorf("elapsed between occurrences = %v, want 23h across the spring-forward "+
			"(adding 24h to a UTC timestamp would give 24h and move the webinar to 22:00)", elapsed)
	}
}

// TestRecurrenceValidationRejectsBadInput. The panel validates too, but a
// request does not have to come from the panel.
func TestRecurrenceValidationRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		r       Recurrence
		wantErr bool
	}{
		{"valid", dailyAiran(), false},
		{"valid without a start date", Recurrence{Enabled: true, Time: "21:00", Zone: almatyTZ}, false},
		{"disabled is always valid", Recurrence{Enabled: false, Time: "nonsense", Zone: "Mars/Olympus"}, false},
		{"no time", Recurrence{Enabled: true, Zone: almatyTZ}, true},
		{"bad time", Recurrence{Enabled: true, Time: "25:71", Zone: almatyTZ}, true},
		{"time is not a time", Recurrence{Enabled: true, Time: "кешке", Zone: almatyTZ}, true},
		{"bad zone", Recurrence{Enabled: true, Time: "21:00", Zone: "Mars/Olympus"}, true},
		{"bad start date", Recurrence{Enabled: true, Time: "21:00", StartDate: "18.08.2026", Zone: almatyTZ}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.r.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("Validate accepted an invalid recurrence")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate rejected a valid recurrence: %v", err)
			}
		})
	}
}

// TestRecurrenceOfFallsBackToTheEventDate: the existing date picker keeps its
// meaning. An operator who ticks the box without touching the date gets a
// series that starts on the day they had already chosen.
func TestRecurrenceOfFallsBackToTheEventDate(t *testing.T) {
	event := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ)
	campaign := &domain.Campaign{
		Timezone:         almatyTZ,
		EventStartAt:     &event,
		IsDailyRecurring: true,
		RecurrenceTime:   "21:00",
	}

	if got := RecurrenceOf(campaign).StartDate; got != "2026-08-18" {
		t.Fatalf("StartDate = %q, want the event's own date 2026-08-18", got)
	}
}

// TestTwoRecurringCampaignsAreIndependent. Different hours, different zones,
// different series starts: nothing is shared between them.
func TestTwoRecurringCampaignsAreIndependent(t *testing.T) {
	airan := Recurrence{Enabled: true, Time: "21:00", StartDate: "2026-08-18", Zone: almatyTZ}
	other := Recurrence{Enabled: true, Time: "09:30", StartDate: "2026-08-19", Zone: "Europe/Berlin"}

	now := mustLocalTime(t, "2026-08-18", "12:00", almatyTZ)

	gotAiran, err := airan.NextOccurrenceAfter(now)
	if err != nil {
		t.Fatalf("airan: %v", err)
	}
	gotOther, err := other.NextOccurrenceAfter(now)
	if err != nil {
		t.Fatalf("other: %v", err)
	}

	if want := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ); !gotAiran.Equal(want) {
		t.Errorf("airan occurrence = %v, want %v", gotAiran, want)
	}
	if want := mustLocalTime(t, "2026-08-19", "09:30", "Europe/Berlin"); !gotOther.Equal(want) {
		t.Errorf("other occurrence = %v, want %v", gotOther, want)
	}
}

// TestEnrollmentOccurrenceOverridesTheCampaignAnchor is the whole mechanism:
// once an enrolment has pinned a webinar, that is what its steps measure from.
func TestEnrollmentOccurrenceOverridesTheCampaignAnchor(t *testing.T) {
	seriesStart := mustLocalTime(t, "2026-08-18", "21:00", almatyTZ)
	occurrence := mustLocalTime(t, "2026-08-21", "21:00", almatyTZ)

	campaign := &domain.Campaign{
		Timezone: almatyTZ, EventStartAt: &seriesStart,
		IsDailyRecurring: true, RecurrenceTime: "21:00", RecurrenceStartDate: "2026-08-18",
	}

	pinned := &domain.Enrollment{OccurrenceAt: &occurrence}
	if got := EventAnchor(campaign, pinned); !got.Equal(occurrence) {
		t.Errorf("anchor = %v, want the pinned occurrence %v", got, occurrence)
	}

	// An enrolment created before the toggle keeps the campaign anchor, so
	// nothing it already had queued moves.
	unpinned := &domain.Enrollment{}
	if got := EventAnchor(campaign, unpinned); !got.Equal(seriesStart) {
		t.Errorf("anchor = %v, want the campaign event start %v", got, seriesStart)
	}
}

// TestBuildPlanUsesWhateverAnchorItIsGiven ties the planner to the occurrence
// without the planner needing to know recurrence exists.
func TestBuildPlanUsesWhateverAnchorItIsGiven(t *testing.T) {
	occurrence := mustLocalTime(t, "2026-08-19", "21:00", almatyTZ)
	steps := []domain.CampaignStep{
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: -30 * 60},
		{Enabled: true, ScheduleKind: domain.ScheduleRelativeToEvent, OffsetSeconds: 0},
	}

	now := mustLocalTime(t, "2026-08-19", "12:00", almatyTZ)
	plan := BuildPlan(&occurrence, steps, PlanOptions{Now: now, EnrolledAt: now})

	if len(plan) != 2 {
		t.Fatalf("plan has %d entries, want 2", len(plan))
	}
	if got := timex.FormatIn(plan[0].RunAt, almatyTZ, "2006-01-02 15:04"); got != "2026-08-19 20:30" {
		t.Errorf("-30m step = %s, want 2026-08-19 20:30", got)
	}
	if got := timex.FormatIn(plan[1].RunAt, almatyTZ, "2006-01-02 15:04"); got != "2026-08-19 21:00" {
		t.Errorf("webinar-start step = %s, want 2026-08-19 21:00", got)
	}
}
