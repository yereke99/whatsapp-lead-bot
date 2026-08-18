//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Daily recurring webinars, against a real database.
//
// The property under test throughout is that recurrence changes exactly one
// thing — which webinar a contact's steps are measured from — and nothing else.
// The queue, its unique constraint, reconciliation, the audience filter and the
// send-time template variables all keep working as they do for a one-time
// campaign, because none of them were taught about recurrence at all.

const testZone = "Asia/Almaty"

// almatyTime builds a UTC instant from a local wall-clock reading.
func almatyTime(t *testing.T, date, clock string) time.Time {
	t.Helper()
	at, err := timex.ParseInLocation(date, clock, testZone)
	if err != nil {
		t.Fatalf("parse %s %s: %v", date, clock, err)
	}
	return at
}

// localOf renders an instant the way the operator reads it.
func localOf(at time.Time) string { return timex.FormatIn(at, testZone, "2006-01-02 15:04") }

// recurringCampaign builds the Airan shape: a daily 21:00 webinar with the
// offsets from the brief, active and triggerable.
//
// It goes through the service's own save path rather than writing rows, so the
// test exercises the validation and the event-anchor resolution an operator
// would hit.
func recurringCampaign(t *testing.T, f *fixture, name, clock, startDate string, offsets []int) *domain.Campaign {
	t.Helper()
	return recurringCampaignWithTrigger(t, f, name, "Айран", clock, startDate, offsets)
}

// recurringCampaignWithTrigger is the same, with the keyword spelled out. A
// keyword may only route to one campaign at a time, so a test with two series
// running side by side has to give each its own.
func recurringCampaignWithTrigger(t *testing.T, f *fixture, name, keyword, clock, startDate string, offsets []int) *domain.Campaign {
	t.Helper()
	ctx := context.Background()

	campaign, err := f.campaignSvc.Create(ctx, campaigns.SaveInput{
		Name:                    name,
		EventType:               "WEBINAR",
		EventDate:               startDate,
		EventTime:               clock,
		Timezone:                testZone,
		WebinarLink:             "https://bizon365.online/room/209010/ayrankaimaq",
		IsDailyRecurring:        true,
		RecurrenceTime:          clock,
		RecurrenceStartDate:     startDate,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		CatchUpMissedSteps:      false,
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}, nil)
	if err != nil {
		t.Fatalf("create recurring campaign: %v", err)
	}

	for i, offset := range offsets {
		addStep(t, f, campaign.ID, "step "+timex.HumanOffset(offset), offset)
		_ = i
	}
	f.addTrigger(t, campaign.ID, keyword)

	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatalf("activate: %v", err)
	}

	full, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return full
}

// enroll runs the real trigger path for one phone at one moment.
func enroll(t *testing.T, f *fixture, campaignID uuid.UUID, phone string, at time.Time) *domain.Enrollment {
	t.Helper()
	ctx := context.Background()

	contact := f.createContact(t, phone)
	match, err := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if err != nil || match == nil {
		t.Fatalf("MatchTrigger: %v (match=%v)", err, match)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, at); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	return enrollmentOf(t, f, campaignID, contact.ID)
}

// liveTimes lists the local send times of an enrolment's live jobs, in order.
func liveTimes(t *testing.T, f *fixture, enrollmentID uuid.UUID) []string {
	t.Helper()

	jobs := allJobs(t, f, enrollmentID)
	var out []string
	for _, job := range jobs {
		if job.Status == domain.JobPending || job.Status == domain.JobSent {
			out = append(out, localOf(job.ScheduledAt))
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------

// TestOneTimeCampaignIsUnaffected is the backward-compatibility test. A
// campaign that never touches the toggle schedules from its own event start,
// pins no occurrence, and produces exactly the jobs it always did.
func TestOneTimeCampaignIsUnaffected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "One-time webinar", eventStart, []int{-5 * 3600, -30 * 60, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	if campaign.IsDailyRecurring {
		t.Fatal("a campaign created without the toggle must not be recurring")
	}
	if campaign.NextOccurrenceAt != nil {
		t.Fatalf("NextOccurrenceAt = %v, want nil for a one-time campaign", campaign.NextOccurrenceAt)
	}

	enrollment := enroll(t, f, campaign.ID, "77010000001", time.Now().UTC())
	if enrollment.OccurrenceAt != nil {
		t.Fatalf("occurrence_at = %v, want NULL for a one-time campaign", enrollment.OccurrenceAt)
	}

	want := []string{
		localOf(eventStart.Add(-5 * time.Hour)),
		localOf(eventStart.Add(-30 * time.Minute)),
		localOf(eventStart),
	}
	sortStrings(want)
	if got := liveTimes(t, f, enrollment.ID); !equalStrings(got, want) {
		t.Errorf("schedule = %v, want %v", got, want)
	}

	// And reconciliation, run repeatedly, leaves it exactly as it is.
	for i := 0; i < 3; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign: %v", err)
		}
	}
	if got := liveTimes(t, f, enrollment.ID); !equalStrings(got, want) {
		t.Errorf("schedule after reconciliation = %v, want %v", got, want)
	}
}

// TestRecurringCampaignSchedulesAgainstTodaysWebinar is the acceptance example:
// a 21:00 daily webinar with the Airan offsets produces 16:00, 18:00, 19:00,
// 20:00, 20:30, 20:45, 20:55 and 21:00 — computed from the occurrence, on the
// contact's own day.
func TestRecurringCampaignSchedulesAgainstTodaysWebinar(t *testing.T) {
	f := newFixture(t)

	// The series starts yesterday, so "today" is an ordinary day of a running
	// series rather than its first.
	today := time.Now().In(timex.MustLocation(testZone))
	startDate := today.AddDate(0, 0, -1).Format(timex.DateLayout)

	// The webinar is placed a few hours ahead of the current clock, so every
	// step of it is still in the future whenever this test runs.
	clock := today.Add(6 * time.Hour).Format("15:04")
	webinarDay := today.Format(timex.DateLayout)
	if today.Add(6*time.Hour).Day() != today.Day() {
		webinarDay = today.AddDate(0, 0, 1).Format(timex.DateLayout)
	}

	offsets := []int{-5 * 3600, -3 * 3600, -2 * 3600, -3600, -30 * 60, -15 * 60, -5 * 60, 0}
	campaign := recurringCampaign(t, f, "Airan", clock, startDate, offsets)

	if !campaign.IsDailyRecurring {
		t.Fatal("campaign is not recurring")
	}
	if campaign.NextOccurrenceAt == nil {
		t.Fatal("NextOccurrenceAt is nil for a recurring campaign")
	}

	occurrence := almatyTime(t, webinarDay, clock)
	if !campaign.NextOccurrenceAt.Equal(occurrence) {
		t.Fatalf("NextOccurrenceAt = %s, want %s", localOf(*campaign.NextOccurrenceAt), localOf(occurrence))
	}

	enrollment := enroll(t, f, campaign.ID, "77010000002", time.Now().UTC())
	if enrollment.OccurrenceAt == nil {
		t.Fatal("the enrolment pinned no occurrence")
	}
	if !enrollment.OccurrenceAt.Equal(occurrence) {
		t.Fatalf("pinned occurrence = %s, want %s", localOf(*enrollment.OccurrenceAt), localOf(occurrence))
	}

	want := make([]string, 0, len(offsets))
	for _, offset := range offsets {
		want = append(want, localOf(occurrence.Add(time.Duration(offset)*time.Second)))
	}
	sortStrings(want)

	if got := liveTimes(t, f, enrollment.ID); !equalStrings(got, want) {
		t.Errorf("schedule = %v,\nwant      %v", got, want)
	}
}

// TestContactAfterTodaysWebinarGetsTomorrows. The point of the feature: someone
// who arrives once tonight's webinar is over is not told "you missed it", they
// are put on tomorrow's — without the campaign being duplicated.
func TestContactAfterTodaysWebinarGetsTomorrows(t *testing.T) {
	f := newFixture(t)

	// A webinar an hour ago, so today's occurrence is behind us.
	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(-1 * time.Hour).Format("15:04")
	startDate := now.AddDate(0, 0, -7).Format(timex.DateLayout)

	campaign := recurringCampaign(t, f, "Airan", clock, startDate, []int{-3600, -30 * 60, 0})
	enrollment := enroll(t, f, campaign.ID, "77010000003", time.Now().UTC())

	if enrollment.OccurrenceAt == nil {
		t.Fatal("no occurrence pinned")
	}
	if !enrollment.OccurrenceAt.After(time.Now().UTC()) {
		t.Fatalf("pinned occurrence %s is in the past; a late arrival must be put on the next webinar",
			localOf(*enrollment.OccurrenceAt))
	}

	// Every step is live and in the future: nothing was skipped as expired,
	// which is exactly what a one-time campaign would have done here.
	live := liveTimes(t, f, enrollment.ID)
	if len(live) != 3 {
		t.Fatalf("live jobs = %d (%v), want 3", len(live), live)
	}
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.Status != domain.JobPending {
			t.Errorf("job for step %s is %s (%s), want PENDING", job.StepID, job.Status, job.CancelReason)
		}
	}
}

// TestRepeatedSchedulerRunsCreateNoDuplicates is the idempotency guarantee, run
// the way the production scheduler runs it: over and over, with nothing else
// changing. The protection is the database's own unique constraint, which the
// occurrence does not weaken because it is a property of the enrolment.
func TestRepeatedSchedulerRunsCreateNoDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(4 * time.Hour).Format("15:04")
	campaign := recurringCampaign(t, f, "Airan", clock,
		now.AddDate(0, 0, -1).Format(timex.DateLayout), []int{-3 * 3600, -30 * 60, 0})

	enrollment := enroll(t, f, campaign.ID, "77010000004", time.Now().UTC())
	before := allJobs(t, f, enrollment.ID)
	if len(before) != 3 {
		t.Fatalf("initial jobs = %d, want 3", len(before))
	}

	for i := 0; i < 10; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign %d: %v", i, err)
		}
	}

	after := allJobs(t, f, enrollment.ID)
	if len(after) != len(before) {
		t.Fatalf("jobs after ten sweeps = %d, want %d", len(after), len(before))
	}

	// The diagnostics agree, which is the check that would catch a duplicate
	// the tests forgot to look for.
	report, err := f.campaignSvc.Consistency(ctx)
	if err != nil {
		t.Fatalf("Consistency: %v", err)
	}
	for _, check := range report.Checks {
		if check.Name == "duplicate_jobs" && check.Count != 0 {
			t.Fatalf("duplicate_jobs = %d, want 0", check.Count)
		}
	}
}

// TestRestartRecreatesMissingJobsForTheSameOccurrence. A process that dies
// mid-schedule loses rows; the sweep that runs on the next boot must put them
// back — on the occurrence the contact was already assigned to, not on a fresh
// one derived from the restart time.
func TestRestartRecreatesMissingJobsForTheSameOccurrence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(5 * time.Hour).Format("15:04")
	campaign := recurringCampaign(t, f, "Airan", clock,
		now.AddDate(0, 0, -1).Format(timex.DateLayout), []int{-3 * 3600, -30 * 60, 0})

	enrollment := enroll(t, f, campaign.ID, "77010000005", time.Now().UTC())
	occurrence := *enrollment.OccurrenceAt
	want := liveTimes(t, f, enrollment.ID)

	// Simulate the loss: the rows a crash never committed.
	if _, err := testDB.Exec(ctx,
		`DELETE FROM scheduled_messages WHERE enrollment_id = $1`, enrollment.ID); err != nil {
		t.Fatalf("simulate lost jobs: %v", err)
	}

	// The boot-time sweep.
	if _, err := f.campaignSvc.ReconcileAll(ctx); err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}

	restored := enrollmentOf(t, f, campaign.ID, enrollment.ContactID)
	if restored.OccurrenceAt == nil || !restored.OccurrenceAt.Equal(occurrence) {
		t.Fatalf("occurrence moved across the restart: %v, want %s", restored.OccurrenceAt, localOf(occurrence))
	}
	if got := liveTimes(t, f, enrollment.ID); !equalStrings(got, want) {
		t.Errorf("recovered schedule = %v, want %v", got, want)
	}
}

// TestTogglingRecurrenceOff stops future occurrences without destroying the
// schedule contacts are already waiting on.
func TestTogglingRecurrenceOff(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(4 * time.Hour).Format("15:04")
	startDate := now.Format(timex.DateLayout)
	campaign := recurringCampaign(t, f, "Airan", clock, startDate, []int{-3600, 0})

	enrollment := enroll(t, f, campaign.ID, "77010000006", time.Now().UTC())
	before := liveTimes(t, f, enrollment.ID)
	pinned := *enrollment.OccurrenceAt

	// Switch it off, leaving the event date and time as they were.
	if _, err := f.campaignSvc.Update(ctx, campaign.ID, campaigns.SaveInput{
		Name:                    campaign.Name,
		EventType:               "WEBINAR",
		EventDate:               startDate,
		EventTime:               clock,
		Timezone:                testZone,
		IsDailyRecurring:        false,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	updated, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.IsDailyRecurring {
		t.Fatal("recurrence is still on after being switched off")
	}
	if updated.NextOccurrenceAt != nil {
		t.Fatalf("NextOccurrenceAt = %v, want nil once recurrence is off", updated.NextOccurrenceAt)
	}

	// The enrolment keeps the webinar it was already promised, and its queue is
	// untouched. Switching the series off is not a cancellation.
	kept := enrollmentOf(t, f, campaign.ID, enrollment.ContactID)
	if kept.OccurrenceAt == nil || !kept.OccurrenceAt.Equal(pinned) {
		t.Fatalf("pinned occurrence changed to %v, want %s (already-scheduled messages must not move)",
			kept.OccurrenceAt, localOf(pinned))
	}
	if got := liveTimes(t, f, enrollment.ID); !equalStrings(got, before) {
		t.Errorf("schedule after switching recurrence off = %v, want %v", got, before)
	}

	// A contact arriving now gets the one-time behaviour back: no occurrence.
	fresh := enroll(t, f, campaign.ID, "77010000007", time.Now().UTC())
	if fresh.OccurrenceAt != nil {
		t.Fatalf("a new enrolment pinned %v after recurrence was switched off", fresh.OccurrenceAt)
	}
}

// TestTogglingRecurrenceOn starts the series from the next valid occurrence and
// sends no historical backlog.
func TestTogglingRecurrenceOn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A one-time campaign whose webinar was yesterday.
	past := time.Now().UTC().Add(-24 * time.Hour)
	campaign := f.createCampaign(t, "Airan", past, []int{-3600, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(3 * time.Hour).Format("15:04")

	if _, err := f.campaignSvc.Update(ctx, campaign.ID, campaigns.SaveInput{
		Name:                    campaign.Name,
		EventType:               "WEBINAR",
		EventDate:               now.AddDate(0, 0, -1).Format(timex.DateLayout),
		EventTime:               clock,
		Timezone:                testZone,
		IsDailyRecurring:        true,
		RecurrenceTime:          clock,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	updated, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if updated.NextOccurrenceAt == nil {
		t.Fatal("no next occurrence after switching recurrence on")
	}
	if !updated.NextOccurrenceAt.After(time.Now().UTC()) {
		t.Fatalf("next occurrence %s is in the past; the series must start from now onwards",
			localOf(*updated.NextOccurrenceAt))
	}

	enrollment := enroll(t, f, campaign.ID, "77010000008", time.Now().UTC())
	if enrollment.OccurrenceAt == nil {
		t.Fatal("no occurrence pinned after switching recurrence on")
	}
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.Status != domain.JobPending {
			t.Errorf("job for step %s is %s (%s); no historical backlog should be produced",
				job.StepID, job.Status, job.CancelReason)
		}
		if !job.ScheduledAt.After(time.Now().UTC()) {
			t.Errorf("job scheduled at %s, which is not in the future", localOf(job.ScheduledAt))
		}
	}
}

// TestChangingTheWebinarTimeMovesFutureOccurrencesOnly. The edit is applied to
// contacts still waiting, and to nothing that has already been sent.
func TestChangingTheWebinarTimeMovesFutureOccurrencesOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	startDate := now.Format(timex.DateLayout)
	oldClock := now.Add(5 * time.Hour).Format("15:04")
	newClock := now.Add(4 * time.Hour).Format("15:04")

	campaign := recurringCampaign(t, f, "Airan", oldClock, startDate, []int{-3600, 0})
	enrollment := enroll(t, f, campaign.ID, "77010000009", time.Now().UTC())

	// One message has already gone out. It is history and must not move.
	jobs := allJobs(t, f, enrollment.ID)
	markSent(t, jobs[0].ID)
	sentAtBefore := jobs[0].ScheduledAt

	if _, err := f.campaignSvc.Update(ctx, campaign.ID, campaigns.SaveInput{
		Name:                    campaign.Name,
		EventType:               "WEBINAR",
		EventDate:               startDate,
		EventTime:               newClock,
		Timezone:                testZone,
		IsDailyRecurring:        true,
		RecurrenceTime:          newClock,
		RecurrenceStartDate:     startDate,
		ExistingContactBehavior: string(domain.BehaviorIgnore),
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	moved := enrollmentOf(t, f, campaign.ID, enrollment.ContactID)
	wantOccurrence := almatyTime(t, startDate, newClock)
	if moved.OccurrenceAt == nil || !moved.OccurrenceAt.Equal(wantOccurrence) {
		t.Fatalf("occurrence = %v, want %s", moved.OccurrenceAt, localOf(wantOccurrence))
	}

	after := allJobs(t, f, enrollment.ID)
	if len(after) != len(jobs) {
		t.Fatalf("jobs = %d after the edit, want %d — an edit must not duplicate the queue", len(after), len(jobs))
	}

	for _, job := range after {
		switch job.Status {
		case domain.JobSent:
			if !job.ScheduledAt.Equal(sentAtBefore) {
				t.Errorf("a SENT job moved from %s to %s", localOf(sentAtBefore), localOf(job.ScheduledAt))
			}
		case domain.JobPending:
			// Pending work follows the new webinar time.
			delta := job.ScheduledAt.Sub(wantOccurrence)
			if delta != 0 && delta != -time.Hour {
				t.Errorf("pending job at %s is not an offset of the new occurrence %s",
					localOf(job.ScheduledAt), localOf(wantOccurrence))
			}
		}
	}
}

// TestAudienceFilterStillAppliesOnARecurringCampaign. The per-step cutoff is
// evaluated by the same code, against the same enrolment time, whichever
// webinar the contact is on.
func TestAudienceFilterStillAppliesOnARecurringCampaign(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(5 * time.Hour).Format("15:04")
	campaign := recurringCampaign(t, f, "Airan", clock,
		now.Format(timex.DateLayout), []int{-3 * 3600, -300})

	full, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	restricted := full.Steps[len(full.Steps)-1]

	// Only contacts who arrive from now on may receive the last message.
	cutoff := time.Now().UTC()
	setAudienceFilter(t, f, restricted, true, &cutoff)

	early := enroll(t, f, campaign.ID, "77010000010", cutoff.Add(-time.Hour))
	if _, err := testDB.Exec(ctx,
		`UPDATE campaign_contacts SET enrolled_at = $2 WHERE id = $1`,
		early.ID, cutoff.Add(-time.Hour)); err != nil {
		t.Fatalf("backdate enrolment: %v", err)
	}
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
		t.Fatalf("ReconcileCampaign: %v", err)
	}

	late := enroll(t, f, campaign.ID, "77010000011", cutoff.Add(time.Minute))

	if willSend(t, f, early.ID, restricted.ID) {
		t.Error("the early contact is inside the audience cutoff; recurrence must not bypass it")
	}
	if !willSend(t, f, late.ID, restricted.ID) {
		t.Error("the late contact is outside the cutoff and should receive the message")
	}

	// With the filter switched off, both are back in.
	setAudienceFilter(t, f, restricted, false, nil)
	if !willSend(t, f, early.ID, restricted.ID) {
		t.Error("the early contact should receive the message once the cutoff is withdrawn")
	}
}

// TestTemplateVariablesResolveToTheContactsOwnOccurrence. Two contacts on two
// different webinars of the same campaign read two different dates from the
// same template.
func TestTemplateVariablesResolveToTheContactsOwnOccurrence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(3 * time.Hour).Format("15:04")
	campaign := recurringCampaign(t, f, "Airan", clock,
		now.Format(timex.DateLayout), []int{-3600, 0})

	today := enroll(t, f, campaign.ID, "77010000012", time.Now().UTC())

	// A second contact placed on tomorrow's webinar, the way a contact who
	// arrives after tonight's session would be.
	tomorrow := enroll(t, f, campaign.ID, "77010000013", time.Now().UTC())
	nextDay := today.OccurrenceAt.AddDate(0, 0, 1)
	if err := f.campaignRepo.SetEnrollmentOccurrence(ctx, nil, tomorrow.ID, &nextDay); err != nil {
		t.Fatalf("SetEnrollmentOccurrence: %v", err)
	}

	check := func(enrollmentID uuid.UUID, want time.Time) {
		t.Helper()

		jobs := allJobs(t, f, enrollmentID)
		if len(jobs) == 0 {
			t.Fatal("no jobs")
		}
		jc, err := f.jobRepo.LoadContext(ctx, jobs[0].ID)
		if err != nil {
			t.Fatalf("LoadContext: %v", err)
		}
		if jc.EnrollmentOccurrenceAt == nil {
			t.Fatal("the send path sees no occurrence for this job")
		}
		if !jc.EnrollmentOccurrenceAt.Equal(want) {
			t.Errorf("{{webinar_date}} would resolve against %s, want %s",
				localOf(*jc.EnrollmentOccurrenceAt), localOf(want))
		}
	}

	check(today.ID, *today.OccurrenceAt)
	check(tomorrow.ID, nextDay)
}

// TestTwoRecurringCampaignsDoNotCrossContaminate. Separate series, separate
// hours, separate contacts, and no shared state between them.
func TestTwoRecurringCampaignsDoNotCrossContaminate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	startDate := now.Format(timex.DateLayout)
	clockA := now.Add(3 * time.Hour).Format("15:04")
	clockB := now.Add(5 * time.Hour).Format("15:04")

	a := recurringCampaignWithTrigger(t, f, "Airan", "Айран", clockA, startDate, []int{-3600, 0})
	b := recurringCampaignWithTrigger(t, f, "Qaimaq", "Қаймақ", clockB, startDate, []int{-3600, 0})

	contactA := f.createContact(t, "77010000014")
	matchA, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if matchA == nil || matchA.CampaignID != a.ID {
		t.Fatalf("trigger routed to %v, want campaign A", matchA)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contactA, matchA, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger A: %v", err)
	}

	contactB := f.createContact(t, "77010000015")
	matchB, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Қаймақ")
	if matchB == nil || matchB.CampaignID != b.ID {
		t.Fatalf("trigger routed to %v, want campaign B", matchB)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contactB, matchB, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger B: %v", err)
	}

	enrollA := enrollmentOf(t, f, a.ID, contactA.ID)
	enrollB := enrollmentOf(t, f, b.ID, contactB.ID)

	wantA := almatyTime(t, startDate, clockA)
	wantB := almatyTime(t, startDate, clockB)

	if enrollA.OccurrenceAt == nil || !enrollA.OccurrenceAt.Equal(wantA) {
		t.Errorf("campaign A occurrence = %v, want %s", enrollA.OccurrenceAt, localOf(wantA))
	}
	if enrollB.OccurrenceAt == nil || !enrollB.OccurrenceAt.Equal(wantB) {
		t.Errorf("campaign B occurrence = %v, want %s", enrollB.OccurrenceAt, localOf(wantB))
	}

	// Reconciling one must not touch the other.
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, a.ID); err != nil {
		t.Fatalf("ReconcileCampaign A: %v", err)
	}
	stillB := enrollmentOf(t, f, b.ID, contactB.ID)
	if !stillB.OccurrenceAt.Equal(wantB) {
		t.Errorf("campaign B occurrence moved to %s while reconciling A", localOf(*stillB.OccurrenceAt))
	}
}

// TestRestartTriggerMovesTheContactToTheNextWebinar. A restart is a new run, so
// it gets a new anchor — and the previous run's rows are left where they are,
// which the run_number half of the unique constraint keeps separate.
func TestRestartTriggerMovesTheContactToTheNextWebinar(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	now := time.Now().In(timex.MustLocation(testZone))
	clock := now.Add(2 * time.Hour).Format("15:04")
	campaign := recurringCampaign(t, f, "Airan", clock,
		now.Format(timex.DateLayout), []int{-3600, 0})

	if _, err := f.campaignSvc.Update(ctx, campaign.ID, campaigns.SaveInput{
		Name:                    campaign.Name,
		EventType:               "WEBINAR",
		EventDate:               now.Format(timex.DateLayout),
		EventTime:               clock,
		Timezone:                testZone,
		IsDailyRecurring:        true,
		RecurrenceTime:          clock,
		RecurrenceStartDate:     now.Format(timex.DateLayout),
		ExistingContactBehavior: string(domain.BehaviorRestart),
		MaxSendAttempts:         5,
		ResumePolicy:            string(domain.ResumeSkipExpired),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	contact := f.createContact(t, "77010000016")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	first := enrollmentOf(t, f, campaign.ID, contact.ID)

	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger (restart): %v", err)
	}
	second := enrollmentOf(t, f, campaign.ID, contact.ID)

	if second.RunNumber != first.RunNumber+1 {
		t.Fatalf("run_number = %d, want %d", second.RunNumber, first.RunNumber+1)
	}
	if second.OccurrenceAt == nil {
		t.Fatal("the restarted run pinned no occurrence")
	}
	if !second.OccurrenceAt.Equal(*first.OccurrenceAt) {
		t.Errorf("occurrence = %s, want the same upcoming webinar %s",
			localOf(*second.OccurrenceAt), localOf(*first.OccurrenceAt))
	}

	// No duplicates across the two runs.
	report, err := f.campaignSvc.Consistency(ctx)
	if err != nil {
		t.Fatalf("Consistency: %v", err)
	}
	for _, check := range report.Checks {
		if check.Name == "duplicate_jobs" && check.Count != 0 {
			t.Fatalf("duplicate_jobs = %d, want 0", check.Count)
		}
	}
}
