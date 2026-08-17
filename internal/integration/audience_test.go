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

// The per-step audience cutoff: send a message only to contacts who entered the
// campaign at or after a given moment.
//
// The boundary is inclusive and the comparison is against
// campaign_contacts.enrolled_at, which is written once when the trigger matches
// and never moved afterwards. These tests pin both, because an off-by-one or a
// drifting entry time would send the message to exactly the people it was meant
// to spare.

// enrollAt puts a contact into the campaign with a specific entry time.
//
// HandleTrigger stamps enrolled_at with the current clock, so the row is
// adjusted afterwards to place the contact at the moment the test needs, and
// the schedule is then rebuilt from that. This is how a contact who "arrived at
// 18:00" is expressed without waiting until 18:00.
func enrollAt(t *testing.T, f *fixture, campaignID uuid.UUID, phone string, joinedAt time.Time) *domain.Enrollment {
	t.Helper()
	ctx := context.Background()

	contact := f.createContact(t, phone)
	match, err := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if err != nil || match == nil {
		t.Fatalf("MatchTrigger: %v (match=%v)", err, match)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, joinedAt); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaignID, contact.ID)
	if _, err := testDB.Exec(ctx,
		`UPDATE campaign_contacts SET enrolled_at = $2 WHERE id = $1`,
		enrollment.ID, joinedAt); err != nil {
		t.Fatalf("set enrolled_at: %v", err)
	}

	// Clear the schedule and let reconciliation rebuild it from the corrected
	// entry time, which is the same path a real enrolment takes.
	if _, err := testDB.Exec(ctx,
		`DELETE FROM scheduled_messages WHERE enrollment_id = $1`, enrollment.ID); err != nil {
		t.Fatalf("clear schedule: %v", err)
	}
	if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaignID); err != nil {
		t.Fatalf("ReconcileCampaign: %v", err)
	}

	return enrollmentOf(t, f, campaignID, contact.ID)
}

// setAudienceFilter configures the cutoff on one step through the service, the
// same path the admin panel uses.
func setAudienceFilter(t *testing.T, f *fixture, step domain.CampaignStep, enabled bool, cutoff *time.Time) {
	t.Helper()

	if _, err := f.campaignSvc.UpdateStep(context.Background(), step.ID, campaigns.StepInput{
		Name:                  step.Name,
		OffsetSeconds:         step.OffsetSeconds,
		TemplateID:            step.TemplateID,
		Enabled:               step.Enabled,
		ScheduleKind:          string(step.ScheduleKind),
		AudienceFilterEnabled: enabled,
		AudienceMinJoinedAt:   cutoff,
	}); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}
}

// jobFor returns the row for one step of one enrolment, skips included.
func jobFor(t *testing.T, f *fixture, enrollmentID, stepID uuid.UUID) (domain.JobStatus, string, bool) {
	t.Helper()

	for _, job := range allJobs(t, f, enrollmentID) {
		if job.StepID == stepID {
			return job.Status, job.CancelReason, true
		}
	}
	return "", "", false
}

// willSend reports whether this step will actually reach this contact.
func willSend(t *testing.T, f *fixture, enrollmentID, stepID uuid.UUID) bool {
	t.Helper()

	status, reason, found := jobFor(t, f, enrollmentID, stepID)
	if !found {
		t.Fatalf("step %s has no row at all for enrolment %s", stepID, enrollmentID)
	}
	if status == domain.JobCancelled && reason == domain.SkipNotEligible {
		return false
	}
	return status == domain.JobPending || status == domain.JobSent || status == domain.JobProcessing
}

// audienceFixture builds a webinar campaign with one restricted step.
//
// The event is placed in the future so every step is live, and the cutoff sits
// five minutes before it — the "20:55 for a 21:00 webinar" shape from the brief.
func audienceFixture(t *testing.T) (*fixture, *domain.Campaign, domain.CampaignStep, time.Time) {
	t.Helper()

	f := newFixture(t)
	eventStart := time.Now().UTC().Add(4 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3 * 3600, -300})
	f.addTrigger(t, campaign.ID, "Айран")

	steps, err := f.campaignRepo.ListSteps(context.Background(), nil, campaign.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}

	cutoff := eventStart.Add(-300) // the restricted step's own send time
	return f, campaign, steps[1], cutoff
}

// TestAudienceFilterDisabledSendsToEveryone is the backward-compatibility case,
// and the most important one: an unconfigured step must behave exactly as it
// did before this feature existed.
func TestAudienceFilterDisabledSendsToEveryone(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)

	// Nothing is configured, so the stored cutoff is irrelevant.
	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-3*time.Hour))
	late := enrollAt(t, f, campaign.ID, "77010000002", cutoff.Add(time.Minute))

	if !willSend(t, f, early.ID, step.ID) {
		t.Error("a contact who joined early must still receive the step when no filter is set")
	}
	if !willSend(t, f, late.ID, step.ID) {
		t.Error("a contact who joined late must receive the step")
	}
	if step.AudienceFilterEnabled {
		t.Error("steps must default to having no audience filter")
	}
}

// TestAudienceFilterBoundary is the inclusive-cutoff rule, checked at the
// second either side of it and exactly on it.
func TestAudienceFilterBoundary(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)
	setAudienceFilter(t, f, step, true, &cutoff)

	cases := []struct {
		name     string
		joined   time.Time
		phone    string
		eligible bool
	}{
		{"three hours early", cutoff.Add(-3 * time.Hour), "77010000001", false},
		{"one minute early", cutoff.Add(-time.Minute), "77010000002", false},
		{"one second early", cutoff.Add(-time.Second), "77010000003", false},
		{"exactly at the cutoff", cutoff, "77010000004", true},
		{"one second late", cutoff.Add(time.Second), "77010000005", true},
		{"one minute late", cutoff.Add(time.Minute), "77010000006", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enrollment := enrollAt(t, f, campaign.ID, tc.phone, tc.joined)
			if got := willSend(t, f, enrollment.ID, step.ID); got != tc.eligible {
				t.Errorf("joined %s, cutoff %s: will send = %v, want %v",
					tc.joined.Format(time.RFC3339), cutoff.Format(time.RFC3339), got, tc.eligible)
			}
		})
	}
}

// TestAudienceFilterRecordsSkipReason checks the ineligible contact ends up with
// a recorded skip rather than a missing row, so the operator can see why.
func TestAudienceFilterRecordsSkipReason(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)
	setAudienceFilter(t, f, step, true, &cutoff)

	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-2*time.Hour))

	status, reason, found := jobFor(t, f, early.ID, step.ID)
	if !found {
		t.Fatal("an ineligible contact must still have a row, so the decision is visible")
	}
	if status != domain.JobCancelled {
		t.Errorf("status = %s, want CANCELLED", status)
	}
	if reason != domain.SkipNotEligible {
		t.Errorf("cancel_reason = %q, want %q", reason, domain.SkipNotEligible)
	}

	// And it must never become live work, however often reconciliation runs.
	for i := 0; i < 5; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(context.Background(), campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign: %v", err)
		}
	}
	if willSend(t, f, early.ID, step.ID) {
		t.Error("reconciliation revived a job for an ineligible contact")
	}
}

// TestAudienceFilterIsPerStep checks each step evaluates its own configuration.
func TestAudienceFilterIsPerStep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(4 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3 * 3600, -1800, -300})
	f.addTrigger(t, campaign.ID, "Айран")

	steps, _ := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	open, restricted30, restricted5 := steps[0], steps[1], steps[2]

	cutoff30 := eventStart.Add(-1800 * time.Second)
	cutoff5 := eventStart.Add(-300 * time.Second)
	setAudienceFilter(t, f, restricted30, true, &cutoff30)
	setAudienceFilter(t, f, restricted5, true, &cutoff5)

	// Joined between the two cutoffs: eligible for the 30-minute step, not the
	// 5-minute one.
	middle := enrollAt(t, f, campaign.ID, "77010000001", cutoff30.Add(time.Minute))

	if !willSend(t, f, middle.ID, open.ID) {
		t.Error("the unrestricted step must reach everyone")
	}
	if !willSend(t, f, middle.ID, restricted30.ID) {
		t.Error("the contact joined after the 30-minute cutoff and must receive that step")
	}
	if willSend(t, f, middle.ID, restricted5.ID) {
		t.Error("the contact joined before the 5-minute cutoff and must not receive that step")
	}
}

// TestAudienceFilterIgnoresLaterMessages is the no-retroactive-eligibility rule:
// an old contact who keeps chatting must not drift across the cutoff.
func TestAudienceFilterIgnoresLaterMessages(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)
	setAudienceFilter(t, f, step, true, &cutoff)

	ctx := context.Background()
	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-2*time.Hour))
	joinedBefore := early.EnrolledAt

	// The contact writes again, well after the cutoff. Under the default IGNORE
	// behaviour this must change nothing at all.
	contact, err := f.contactRepo.GetByID(ctx, early.ContactID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	after := enrollmentOf(t, f, campaign.ID, early.ContactID)
	if !after.EnrolledAt.Equal(joinedBefore) {
		t.Errorf("enrolled_at moved from %s to %s; the entry time must be immutable",
			joinedBefore, after.EnrolledAt)
	}
	if willSend(t, f, early.ID, step.ID) {
		t.Error("an old contact became eligible by sending another message")
	}
}

// TestAudienceFilterAppliedToExistingEnrollments covers the retroactive case
// from the brief: the contact is already enrolled when the operator adds the
// cutoff, and must be dropped from that step.
func TestAudienceFilterAppliedToExistingEnrollments(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)

	// Enrolled first, restricted afterwards.
	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-2*time.Hour))
	if !willSend(t, f, early.ID, step.ID) {
		t.Fatal("the step should reach the contact before the filter is configured")
	}

	setAudienceFilter(t, f, step, true, &cutoff)

	if willSend(t, f, early.ID, step.ID) {
		t.Error("enabling the cutoff must withdraw the queued message from an early contact")
	}
	status, reason, _ := jobFor(t, f, early.ID, step.ID)
	if status != domain.JobCancelled || reason != domain.SkipNotEligible {
		t.Errorf("job is %s/%q, want CANCELLED/%s", status, reason, domain.SkipNotEligible)
	}
}

// TestAudienceFilterDisablingRestoresDelivery checks the switch works both ways.
func TestAudienceFilterDisablingRestoresDelivery(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)
	setAudienceFilter(t, f, step, true, &cutoff)

	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-2*time.Hour))
	if willSend(t, f, early.ID, step.ID) {
		t.Fatal("the contact should be excluded while the filter is on")
	}

	// The cutoff value stays stored; only the switch changes.
	setAudienceFilter(t, f, step, false, &cutoff)

	if !willSend(t, f, early.ID, step.ID) {
		t.Error("switching the filter off must let the message through again")
	}
}

// TestAudienceFilterRequiresCutoff is the validation rule.
func TestAudienceFilterRequiresCutoff(t *testing.T) {
	f, _, step, _ := audienceFixture(t)

	_, err := f.campaignSvc.UpdateStep(context.Background(), step.ID, campaigns.StepInput{
		Name:                  step.Name,
		OffsetSeconds:         step.OffsetSeconds,
		TemplateID:            step.TemplateID,
		Enabled:               true,
		ScheduleKind:          string(step.ScheduleKind),
		AudienceFilterEnabled: true,
		AudienceMinJoinedAt:   nil,
	})
	if err == nil {
		t.Error("enabling the filter without a cutoff must be rejected: it would send to everybody")
	}
}

// TestAudienceFilterTimezoneConversion checks a local wall-clock cutoff is
// stored and compared as the right instant.
func TestAudienceFilterTimezoneConversion(t *testing.T) {
	f, campaign, step, _ := audienceFixture(t)
	ctx := context.Background()

	// 20:55 Asia/Almaty on the campaign's event date.
	eventDay := timex.FormatIn(*campaign.EventStartAt, "Asia/Almaty", "2006-01-02")
	cutoff, err := timex.ParseInLocation(eventDay, "20:55", "Asia/Almaty")
	if err != nil {
		t.Fatalf("ParseInLocation: %v", err)
	}
	setAudienceFilter(t, f, step, true, &cutoff)

	// Asia/Almaty is UTC+5, so 20:55 local is 15:55 UTC.
	stored, err := f.campaignRepo.GetStep(ctx, step.ID)
	if err != nil {
		t.Fatalf("GetStep: %v", err)
	}
	if stored.AudienceMinJoinedAt == nil {
		t.Fatal("the cutoff was not stored")
	}
	if got := stored.AudienceMinJoinedAt.UTC().Format("15:04"); got != "15:55" {
		t.Errorf("stored cutoff is %s UTC, want 15:55 (20:55 Asia/Almaty)", got)
	}
	if got := timex.FormatIn(*stored.AudienceMinJoinedAt, "Asia/Almaty", "15:04"); got != "20:55" {
		t.Errorf("cutoff reads as %s in Asia/Almaty, want 20:55", got)
	}

	// A contact who joined at 20:54:59 local is out; 20:55:00 is in.
	justBefore := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-time.Second))
	exactly := enrollAt(t, f, campaign.ID, "77010000002", cutoff)

	if willSend(t, f, justBefore.ID, step.ID) {
		t.Error("20:54:59 Asia/Almaty must be excluded")
	}
	if !willSend(t, f, exactly.ID, step.ID) {
		t.Error("20:55:00 Asia/Almaty must be included")
	}
}

// TestWorkerRefusesIneligibleJob is the defense-in-depth layer: a live job that
// should not exist is stopped at the send path rather than delivered.
func TestWorkerRefusesIneligibleJob(t *testing.T) {
	f, campaign, step, cutoff := audienceFixture(t)
	ctx := context.Background()

	early := enrollAt(t, f, campaign.ID, "77010000001", cutoff.Add(-2*time.Hour))

	// Configure the cutoff behind the reconciler's back, then force the job
	// back to live and due. This is the shape of every way a stale row can
	// reach the worker: a deploy mid-schedule, a hand-edited row, a step that
	// gained a cutoff after its jobs were queued.
	if _, err := testDB.Exec(ctx, `
		UPDATE campaign_steps
		SET audience_filter_enabled = 1, audience_min_joined_at = $2, updated_at = $3
		WHERE id = $1`, step.ID, cutoff, time.Now().UTC()); err != nil {
		t.Fatalf("configure cutoff directly: %v", err)
	}
	if _, err := testDB.Exec(ctx, `
		UPDATE scheduled_messages
		SET status = 'PENDING', scheduled_at = $3, cancelled_at = NULL, cancel_reason = ''
		WHERE enrollment_id = $1 AND campaign_step_id = $2`,
		early.ID, step.ID, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("force the job live: %v", err)
	}

	claimed := f.claim(t, "worker-1", 10)
	var target *domain.ScheduledMessage
	for i := range claimed {
		if claimed[i].StepID == step.ID {
			target = &claimed[i]
		}
	}
	if target == nil {
		t.Fatal("the forced job was not claimed")
	}

	// The worker loads the job's context and asks the same question the planner
	// asked. It must refuse.
	jobCtx, err := f.jobRepo.LoadContext(ctx, target.ID)
	if err != nil {
		t.Fatalf("LoadContext: %v", err)
	}
	if jobCtx == nil {
		t.Fatal("job context is missing")
	}
	if !jobCtx.StepAudienceFilter {
		t.Error("the job context does not carry the step's audience filter")
	}
	if !jobCtx.EnrollmentJoinedAt.Equal(early.EnrolledAt) {
		t.Errorf("job context entry time is %s, want %s",
			jobCtx.EnrollmentJoinedAt, early.EnrolledAt)
	}

	check := domain.CampaignStep{
		AudienceFilterEnabled: jobCtx.StepAudienceFilter,
		AudienceMinJoinedAt:   jobCtx.StepAudienceMinJoinedAt,
	}
	if check.EligibleFor(jobCtx.EnrollmentJoinedAt) {
		t.Error("the send path considers an ineligible contact eligible")
	}
}

// TestAudienceFilterSurvivesMigrationDefaults checks existing rows are opted
// out, which is what keeps the feature backward compatible.
func TestAudienceFilterSurvivesMigrationDefaults(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(4 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600, 0})

	steps, err := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for i, step := range steps {
		if step.AudienceFilterEnabled {
			t.Errorf("step %d has the filter on by default", i+1)
		}
		if step.AudienceMinJoinedAt != nil {
			t.Errorf("step %d has a cutoff set by default", i+1)
		}
	}
}
