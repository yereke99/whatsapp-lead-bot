//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/domain"
)

// TestProductionSequenceAiranCampaign replays the exact sequence recorded in
// the production database, step by step, in the order the audit log shows it.
//
// The original outcome was five campaign steps, one enrolled contact, and two
// scheduled messages. Three steps reached nobody and the contact was marked
// COMPLETED while the campaign still had work for them.
//
// Reconstructed from audit_logs and the row timestamps in whatsapp.db:
//
//	10:34:11  campaign created, event 16:00Z (21:00 Asia/Almaty)
//	10:37:37  step 1 created  (ON_TRIGGER +2s)
//	10:37:41  campaign activated
//	10:39:07  step 2 created  (-19200s -> 10:40Z)
//	10:39:22  contact enrolls -> 2 jobs queued
//	10:39:25  step 1 sent
//	10:40:00  step 2 sent
//	10:42:36  completion sweep marks the enrollment COMPLETED  <-- the bug
//	10:43:46  step 3 created  -> no job, contact is no longer ACTIVE
//	10:46:19  step 4 created  -> no job
//	10:50:10  step 5 created  -> no job
//	10:55-11:23  eleven step edits, all affecting zero rows
//
// The test asserts the outcome that sequence must produce now: five steps, five
// jobs, and a contact who is still in the campaign.
func TestProductionSequenceAiranCampaign(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The event is placed in the future so the later steps are still live; the
	// original run had roughly five hours between enrolment and the webinar.
	eventStart := time.Now().UTC().Add(5 * time.Hour)

	// 10:34 - 10:39: campaign, first two steps, activation, trigger.
	campaign := f.createCampaign(t, "Айран кәсібі", eventStart, []int{-5 * 3600})
	f.addTrigger(t, campaign.ID, "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді")

	contact := f.createContact(t, "77011234567")
	match, err := f.campaignSvc.MatchTrigger(ctx, nil,
		"Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді")
	if err != nil || match == nil {
		t.Fatalf("MatchTrigger: %v (match=%v)", err, match)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	// 10:39 - 10:40: the queued messages go out. This is the state the real
	// database was in when the completion sweep ran.
	for _, job := range liveJobs(t, f, enrollment.ID) {
		markSent(t, job.ID)
	}

	// 10:42:36 - the completion sweep. Previously this closed the enrollment and
	// made everything afterwards unreachable.
	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}

	// 10:43 - 10:50: the operator adds the remaining steps. In production these
	// created zero jobs.
	addStep(t, f, campaign.ID, "2 сағат қалғанда", -2*3600)
	addStep(t, f, campaign.ID, "1 сағат қалғанда", -3600)
	addStep(t, f, campaign.ID, "30 минут қалғанда", -1800)

	steps, err := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 4 {
		t.Fatalf("%d steps, want 4", len(steps))
	}

	// The invariant: every step has exactly one job for this enrollment.
	rows := allJobs(t, f, enrollment.ID)
	perStep := map[string]int{}
	for _, job := range rows {
		perStep[job.StepID.String()]++
	}
	for _, step := range steps {
		switch perStep[step.ID.String()] {
		case 1:
			// exactly what is wanted
		case 0:
			t.Errorf("step %q has no scheduled message — this is the production bug", step.Name)
		default:
			t.Errorf("step %q has %d scheduled messages, want 1", step.Name, perStep[step.ID.String()])
		}
	}

	if len(rows) != len(steps) {
		t.Errorf("%d job rows for %d steps", len(rows), len(steps))
	}

	// The three added steps must be live and correctly timed.
	live := liveJobs(t, f, enrollment.ID)
	pending := 0
	for _, step := range steps {
		job, ok := live[step.ID]
		if !ok {
			continue
		}
		if job.Status == domain.JobPending {
			pending++
			want := eventStart.Add(time.Duration(step.OffsetSeconds) * time.Second)
			if !job.ScheduledAt.Equal(want) {
				t.Errorf("step %q is scheduled at %s, want %s", step.Name, job.ScheduledAt, want)
			}
		}
	}
	if pending != 3 {
		t.Errorf("%d pending messages after the three additions, want 3", pending)
	}

	// And the contact must be back in the campaign rather than closed out.
	if got := enrollmentOf(t, f, campaign.ID, contact.ID).Status; got != domain.EnrollmentActive {
		t.Errorf("enrollment is %s, want ACTIVE — a campaign with unsent steps "+
			"must not consider the contact finished", got)
	}

	// 10:55 - 11:23: the eleven step edits. Every one must land on the queue.
	last := steps[len(steps)-1]
	for i, offset := range []int{-1740, -1680, -1620, -1560} {
		if _, err := f.campaignSvc.UpdateStep(ctx, last.ID, campaignStepInput(last, offset)); err != nil {
			t.Fatalf("UpdateStep %d: %v", i, err)
		}

		job, ok := liveJobs(t, f, enrollment.ID)[last.ID]
		if !ok {
			t.Fatalf("edit %d left the step with no live job", i)
		}
		want := eventStart.Add(time.Duration(offset) * time.Second)
		if !job.ScheduledAt.Equal(want) {
			t.Errorf("after edit %d the job is at %s, want %s", i, job.ScheduledAt, want)
		}
	}

	// Nothing was duplicated along the way.
	if got := len(allJobs(t, f, enrollment.ID)); got != len(steps) {
		t.Errorf("%d job rows after four edits, want %d", got, len(steps))
	}

	// And the database agrees it is healthy.
	report, err := f.campaignSvc.Consistency(ctx)
	if err != nil {
		t.Fatalf("Consistency: %v", err)
	}
	if !report.Healthy {
		for _, c := range report.Checks {
			if !c.Healthy {
				t.Errorf("consistency check %q found %d problems: %s", c.Name, c.Count, c.Detail)
			}
		}
	}
}
