//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// TestClaimSerialisesPerContact is the per-contact ordering guarantee: a
// contact with three overdue messages must receive them one at a time and in
// schedule order, never as a burst.
func TestClaimSerialisesPerContact(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Everything is already due, which is the situation after an outage.
	eventStart := time.Now().UTC().Add(-time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-1800, -1200, -600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	// CatchUp collapses the older overdue steps, so queue three known jobs
	// directly instead of relying on the plan for this particular assertion.
	f.cancelAll(t, campaign.ID)

	enrollmentID := f.enrollmentID(t, campaign.ID, contact.ID)
	steps := f.stepIDs(t, campaign.ID)
	base := time.Now().UTC().Add(-30 * time.Minute)

	jobs := make([]scheduler.NewJob, 0, len(steps))
	for i, stepID := range steps {
		jobs = append(jobs, scheduler.NewJob{
			CampaignID:   campaign.ID,
			ContactID:    contact.ID,
			EnrollmentID: enrollmentID,
			StepID:       stepID,
			RunNumber:    2, // a fresh run, so the cancelled rows do not collide
			ScheduledAt:  base.Add(time.Duration(i) * time.Minute),
		})
	}
	if _, err := f.jobRepo.Enqueue(ctx, nil, jobs); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A generous batch size: the limit on what comes back must come from the
	// per-contact rule, not from the batch.
	var order []time.Time
	for i := 0; i < len(steps); i++ {
		claimed, err := f.jobRepo.Claim(ctx, "worker-1", 50, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if len(claimed) != 1 {
			t.Fatalf("claim %d returned %d jobs for one contact, want exactly 1", i, len(claimed))
		}
		order = append(order, claimed[0].ScheduledAt)

		// Delivering the message is what releases the contact for the next one.
		message := f.recordMessage(t, claimed[0])
		if err := f.jobRepo.MarkSent(ctx, nil, claimed[0].ID, message, time.Now().UTC()); err != nil {
			t.Fatalf("mark sent: %v", err)
		}
	}

	for i := 1; i < len(order); i++ {
		if order[i].Before(order[i-1]) {
			t.Errorf("job %d was delivered out of order: %v before %v", i, order[i], order[i-1])
		}
	}

	// Nothing is left, and nothing was skipped.
	remaining, err := f.jobRepo.Claim(ctx, "worker-1", 50, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
	if err != nil {
		t.Fatalf("final claim: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d jobs remain after draining the queue", len(remaining))
	}
}

// TestClaimSkipsContactsWithWorkInFlight is the other half of the per-contact
// rule: while one message is being sent, the contact's next one waits.
func TestClaimSkipsContactsWithWorkInFlight(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-10 * time.Second)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{0})
	f.addTrigger(t, campaign.ID, "Айран")

	busy := f.createContact(t, "77011234567")
	other := f.createContact(t, "77017654321")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")

	for _, contact := range []*domain.Contact{busy, other} {
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("HandleTrigger: %v", err)
		}
	}

	first, err := f.jobRepo.Claim(ctx, "worker-1", 1, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(first))
	}

	// A second worker may still serve the other contact, but must not touch
	// the one already being sent to.
	second, err := f.jobRepo.Claim(ctx, "worker-2", 10, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, job := range second {
		if job.ContactID == first[0].ContactID {
			t.Errorf("claimed a second job for a contact already in flight")
		}
	}
}

// TestStuckJobDoesNotStallTheContactsQueue is the guarantee that a single
// message which never completes cannot silence the rest of a customer's
// funnel.
//
// Serialising per contact means a job in flight holds back the next one. If a
// worker dies mid-send, its row stays PROCESSING; without a lease check the
// contact's remaining messages would wait behind a worker that is never coming
// back, and a message scheduled a minute later would simply never arrive.
func TestStuckJobDoesNotStallTheContactsQueue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-1800, -600, -300})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	// Queue three due messages for this contact.
	f.cancelAll(t, campaign.ID)
	enrollmentID := f.enrollmentID(t, campaign.ID, contact.ID)
	base := time.Now().UTC().Add(-10 * time.Minute)

	jobs := make([]scheduler.NewJob, 0, 3)
	for i, stepID := range f.stepIDs(t, campaign.ID) {
		jobs = append(jobs, scheduler.NewJob{
			CampaignID: campaign.ID, ContactID: contact.ID, EnrollmentID: enrollmentID,
			StepID: stepID, RunNumber: 2,
			ScheduledAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	if _, err := f.jobRepo.Enqueue(ctx, nil, jobs); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// The first message is claimed and then its worker dies: the row stays
	// PROCESSING and is never resolved.
	stuck := f.claim(t, "worker-that-dies", 10)
	if len(stuck) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(stuck))
	}

	// While the lease is alive the contact is correctly held: message 2 must
	// not overtake message 1.
	if held := f.claim(t, "worker-2", 10); len(held) != 0 {
		t.Errorf("claimed %d jobs while one was legitimately in flight, want 0", len(held))
	}

	// The lease now expires, as it would for a crashed worker.
	if _, err := testDB.Exec(ctx,
		`UPDATE scheduled_messages SET locked_at = $2 WHERE id = $1`,
		stuck[0].ID, time.Now().UTC().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// The queue must move again rather than waiting on a dead worker.
	resumed := f.claim(t, "worker-3", 10)
	if len(resumed) == 0 {
		t.Fatal("the contact's queue is stalled behind an abandoned job: no messages could be claimed")
	}
	if resumed[0].ID == stuck[0].ID {
		// Recovery may hand back the orphan itself, which is also fine.
		return
	}
	if resumed[0].ContactID != contact.ID {
		t.Errorf("claimed a job for the wrong contact")
	}
}

// TestTriggerRelativeCampaignGivesEachContactTheirOwnSchedule covers the drip
// mode end to end: two contacts triggering forty minutes apart must get two
// different timetables out of the same campaign.
func TestTriggerRelativeCampaignGivesEachContactTheirOwnSchedule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	campaign := f.createTriggerCampaign(t, "Drip", []int{0, 10 * 60, 30 * 60, 3600})
	f.addTrigger(t, campaign.ID, "Айран")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")

	first := f.createContact(t, "77011111111")
	firstAt := time.Now().UTC()
	if _, err := f.campaignSvc.HandleTrigger(ctx, first, match, firstAt); err != nil {
		t.Fatalf("HandleTrigger A: %v", err)
	}

	second := f.createContact(t, "77012222222")
	secondAt := time.Now().UTC()
	if _, err := f.campaignSvc.HandleTrigger(ctx, second, match, secondAt); err != nil {
		t.Fatalf("HandleTrigger B: %v", err)
	}

	wantDelays := []time.Duration{2 * time.Second, 10 * time.Minute, 30 * time.Minute, time.Hour}

	for _, tc := range []struct {
		name    string
		contact *domain.Contact
		at      time.Time
	}{
		{"contact A", first, firstAt},
		{"contact B", second, secondAt},
	} {
		jobs := f.jobsForContact(t, tc.contact.ID)
		if len(jobs) != len(wantDelays) {
			t.Fatalf("%s has %d jobs, want %d", tc.name, len(jobs), len(wantDelays))
		}
		for i, job := range jobs {
			gap := job.ScheduledAt.Sub(tc.at)
			// The anchor is the later of the trigger timestamp and the moment
			// the plan was built, so allow a second of processing time.
			if gap < wantDelays[i] || gap > wantDelays[i]+2*time.Second {
				t.Errorf("%s job %d scheduled %v after the trigger, want ~%v",
					tc.name, i, gap, wantDelays[i])
			}
		}
	}
}

// TestResumeSkipsExpiredJobs is the pause requirement: a campaign paused at
// 18:00 and resumed at 20:00 must not release the 18:00 and 19:00 messages at
// once.
func TestResumeSkipsExpiredJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Three steps, all still ahead at enrollment so the whole queue is built.
	eventStart := time.Now().UTC().Add(3 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart,
		[]int{-2 * 3600, -3600, 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if pending, _ := f.jobCounts(t, campaign.ID); pending != 3 {
		t.Fatalf("queued %d jobs before the pause, want 3", pending)
	}

	// The pause spans the first two steps: their moment passes while the
	// campaign is off.
	f.backdateJobs(t, campaign.ID, 2)

	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatalf("resume: %v", err)
	}

	pending, cancelled := f.jobCounts(t, campaign.ID)
	if cancelled != 2 {
		t.Errorf("cancelled %d expired jobs, want 2", cancelled)
	}
	if pending != 1 {
		t.Errorf("%d jobs remain queued, want 1 (the step still ahead)", pending)
	}
}

// TestResumeSendsNextValidKeepsOneMessage covers the alternative policy: the
// most recent missed step is delivered so the contact still gets context, and
// the older ones are dropped.
func TestResumeSendsNextValidKeepsOneMessage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(3 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart,
		[]int{-2 * 3600, -3600, 3600})
	f.addTrigger(t, campaign.ID, "Айран")
	f.setResumePolicy(t, campaign.ID, domain.ResumeSendNextValid)

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	f.backdateJobs(t, campaign.ID, 2)

	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := f.campaignSvc.SetStatus(ctx, campaign.ID, domain.CampaignActive); err != nil {
		t.Fatalf("resume: %v", err)
	}

	pending, cancelled := f.jobCounts(t, campaign.ID)
	if cancelled != 1 {
		t.Errorf("cancelled %d jobs, want 1 (only the older missed step)", cancelled)
	}
	// The pulled-forward step plus the one still ahead.
	if pending != 2 {
		t.Errorf("%d jobs remain queued, want 2", pending)
	}
}

// TestQueueRecordsTemplateVersion checks the snapshot that lets a campaign pin
// its content: every queued job records which revision it was built from.
func TestQueueRecordsTemplateVersion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	jobs := f.jobsForContact(t, contact.ID)
	if len(jobs) != 1 {
		t.Fatalf("queued %d jobs, want 1", len(jobs))
	}
	if jobs[0].TemplateVersion == nil {
		t.Fatal("job did not record a template version")
	}
	if *jobs[0].TemplateVersion != 1 {
		t.Errorf("recorded version %d, want 1", *jobs[0].TemplateVersion)
	}
}

// TestRestartLosesNothing models a deploy in the middle of a campaign: jobs
// held by the dying process come back, jobs already sent stay sent, and
// nothing is delivered twice.
func TestRestartLosesNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-10 * time.Second)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{0})
	f.addTrigger(t, campaign.ID, "Айран")

	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	for i := 0; i < 6; i++ {
		contact := f.createContact(t, formatPhone(i))
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("enrol %d: %v", i, err)
		}
	}

	// Two go out, two are mid-flight when the process dies, two are untouched.
	sent, err := f.jobRepo.Claim(ctx, "dying-worker", 4, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(sent) != 4 {
		t.Fatalf("claimed %d jobs, want 4", len(sent))
	}
	for _, job := range sent[:2] {
		message := f.recordMessage(t, job)
		if err := f.jobRepo.MarkSent(ctx, nil, job.ID, message, time.Now().UTC()); err != nil {
			t.Fatalf("mark sent: %v", err)
		}
	}

	// The restart: locks age out and the recovery sweep runs.
	if _, err := testDB.Exec(ctx,
		`UPDATE scheduled_messages SET locked_at = $1 WHERE status = 'PROCESSING'`,
		time.Now().UTC().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	released, err := f.jobRepo.ReleaseStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("release stale: %v", err)
	}
	if released != 2 {
		t.Errorf("requeued %d orphaned jobs, want 2", released)
	}

	// Everything that had not been delivered is claimable again, and the two
	// delivered ones are not.
	var reclaimed int
	for {
		batch, err := f.jobRepo.Claim(ctx, "new-worker", 10, time.Now().UTC(), time.Now().UTC().Add(-defaultLease))
		if err != nil {
			t.Fatalf("reclaim: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, job := range batch {
			if job.Status == domain.JobSent {
				t.Errorf("a SENT job was handed out again: %s", job.ID)
			}
			if err := f.jobRepo.Cancel(ctx, job.ID, "test drain"); err != nil {
				t.Fatal(err)
			}
		}
		reclaimed += len(batch)
	}
	if reclaimed != 4 {
		t.Errorf("recovered %d jobs, want 4 (2 orphaned + 2 never started)", reclaimed)
	}

	var stillSent int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'SENT'`,
		campaign.ID).Scan(&stillSent); err != nil {
		t.Fatal(err)
	}
	if stillSent != 2 {
		t.Errorf("%d jobs are SENT after the restart, want 2", stillSent)
	}
}

// TestCampaignValidationBlocksActivation checks the pre-flight list: a campaign
// with no trigger and no enabled step cannot be switched on, and says why.
func TestCampaignValidationBlocksActivation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600})
	if err := f.campaignRepo.SetStatus(ctx, nil, campaign.ID, domain.CampaignDraft); err != nil {
		t.Fatal(err)
	}

	// No trigger yet.
	_, err := f.campaignSvc.Activate(ctx, campaign.ID)
	if err == nil {
		t.Fatal("activation succeeded without a trigger")
	}
	var invalid campaigns.ValidationError
	if !asValidationError(err, &invalid) {
		t.Fatalf("error is %T, want campaigns.ValidationError", err)
	}
	if len(invalid.Problems) == 0 {
		t.Error("validation error carried no problems")
	}

	f.addTrigger(t, campaign.ID, "Айран")
	if _, err := f.campaignSvc.Activate(ctx, campaign.ID); err != nil {
		t.Fatalf("activation failed with a trigger in place: %v", err)
	}
}
