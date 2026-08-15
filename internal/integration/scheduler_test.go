//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// TestEnrollmentCreatesFullSchedule walks the whole funnel: a contact sends the
// trigger, gets enrolled, and every enabled step is queued exactly once.
func TestEnrollmentCreatesFullSchedule(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart,
		[]int{-5 * 3600, -3 * 3600, -3600, -450, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")

	match, err := f.campaignSvc.MatchTrigger(ctx, nil, "  АЙРАН  ")
	if err != nil {
		t.Fatalf("MatchTrigger: %v", err)
	}
	if match == nil {
		t.Fatal("the trigger did not match despite normalization")
	}

	result, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC())
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if result.Action != campaignsActionEnrolled {
		t.Fatalf("Action = %v, want ENROLLED", result.Action)
	}
	if result.JobsCreated != 5 {
		t.Errorf("JobsCreated = %d, want 5", result.JobsCreated)
	}

	jobs, total, err := f.jobRepo.List(ctx, scheduler.ListFilter{CampaignID: &campaign.ID, Limit: 50})
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if total != 5 {
		t.Errorf("stored %d jobs, want 5", total)
	}

	for _, job := range jobs {
		if job.Status != domain.JobPending {
			t.Errorf("job %s has status %s, want PENDING", job.ID, job.Status)
		}
	}
}

// TestDuplicateTriggerDoesNotDuplicateJobs is the central anti-duplicate
// guarantee under the default IGNORE behaviour.
func TestDuplicateTriggerDoesNotDuplicateJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")

	for i := 0; i < 3; i++ {
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("HandleTrigger attempt %d: %v", i, err)
		}
	}

	_, total, err := f.jobRepo.List(ctx, scheduler.ListFilter{CampaignID: &campaign.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("three triggers produced %d jobs, want 2", total)
	}
}

// TestConcurrentTriggersEnrollOnce simulates two webhook deliveries racing on
// the same trigger, which is what a provider retry looks like in production.
func TestConcurrentTriggersEnrollOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")

	var wg sync.WaitGroup
	errs := make([]error, 8)

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, errs[index] = f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent HandleTrigger %d failed: %v", i, err)
		}
	}

	_, total, err := f.jobRepo.List(ctx, scheduler.ListFilter{CampaignID: &campaign.ID, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("concurrent triggers produced %d jobs, want 2", total)
	}

	var enrollments int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1`, campaign.ID).Scan(&enrollments); err != nil {
		t.Fatal(err)
	}
	if enrollments != 1 {
		t.Errorf("created %d enrollments, want 1", enrollments)
	}
}

// TestClaimIsExclusive is the guarantee that lets several replicas run at once:
// no job may be handed to two workers.
func TestClaimIsExclusive(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Hour) // everything is already due
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{0})
	f.addTrigger(t, campaign.ID, "Айран")

	// Enrol twenty contacts so there is a real batch to fight over.
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	for i := 0; i < 20; i++ {
		contact := f.createContact(t, formatPhone(i))
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("enrol contact %d: %v", i, err)
		}
	}

	const workers = 6
	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[uuid.UUID]int{}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for {
				jobs, err := f.jobRepo.Claim(ctx, workerName(workerID), 3, time.Now().UTC())
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(jobs) == 0 {
					return
				}
				mu.Lock()
				for _, job := range jobs {
					claimed[job.ID]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(claimed) == 0 {
		t.Fatal("no jobs were claimed")
	}
	for id, count := range claimed {
		if count != 1 {
			t.Errorf("job %s was claimed %d times, want exactly 1", id, count)
		}
	}
}

// TestReleaseStaleRequeuesOrphanedJobs models a worker crashing mid-send.
func TestReleaseStaleRequeuesOrphanedJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	claimed, err := f.jobRepo.Claim(ctx, "worker-that-will-die", 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d jobs, want 1", len(claimed))
	}

	// Nothing is due while the lock is held.
	again, err := f.jobRepo.Claim(ctx, "another-worker", 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a locked job was claimed again by another worker")
	}

	// Age the lock past the timeout, as the reaper would find it.
	if _, err := testDB.Exec(ctx,
		`UPDATE scheduled_messages SET locked_at = $2 WHERE id = $1`,
		claimed[0].ID, time.Now().UTC().Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	released, err := f.jobRepo.ReleaseStale(ctx, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Errorf("released %d jobs, want 1", released)
	}

	recovered, err := f.jobRepo.Claim(ctx, "replacement-worker", 10, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 {
		t.Errorf("the orphaned job was not recoverable: claimed %d", len(recovered))
	}
}

// TestRescheduleOnlyMovesPendingJobs is the requirement that changing a webinar
// time must never rewrite history.
func TestRescheduleOnlyMovesPendingJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600, -1800, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	jobs, _, err := f.jobRepo.List(ctx, scheduler.ListFilter{CampaignID: &campaign.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}

	// Mark one job as already sent; it must not move.
	sentJob := jobs[0]
	sentAt := time.Now().UTC()
	if err := f.jobRepo.MarkSent(ctx, nil, sentJob.ID, uuid.Nil, sentAt); err != nil {
		// MarkSent needs a real message id; write the state directly instead.
		if _, execErr := testDB.Exec(ctx,
			`UPDATE scheduled_messages SET status = 'SENT', sent_at = now() WHERE id = $1`,
			sentJob.ID); execErr != nil {
			t.Fatal(execErr)
		}
	}

	var frozenAt time.Time
	if err := testDB.QueryRow(ctx,
		`SELECT scheduled_at FROM scheduled_messages WHERE id = $1`, sentJob.ID).Scan(&frozenAt); err != nil {
		t.Fatal(err)
	}

	newStart := eventStart.Add(-time.Hour)
	moved, err := f.jobRepo.RescheduleCampaign(ctx, nil, campaign.ID, newStart)
	if err != nil {
		t.Fatalf("RescheduleCampaign: %v", err)
	}
	if moved != 2 {
		t.Errorf("moved %d jobs, want 2 (the sent one must stay)", moved)
	}

	var afterAt time.Time
	if err := testDB.QueryRow(ctx,
		`SELECT scheduled_at FROM scheduled_messages WHERE id = $1`, sentJob.ID).Scan(&afterAt); err != nil {
		t.Fatal(err)
	}
	if !afterAt.Equal(frozenAt) {
		t.Errorf("a sent job was rescheduled: %v -> %v", frozenAt, afterAt)
	}

	// Every pending job should now sit exactly one hour earlier.
	rows, err := testDB.Query(ctx, `
		SELECT sm.scheduled_at, cs.offset_seconds
		FROM scheduled_messages sm
		JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
		WHERE sm.campaign_id = $1 AND sm.status = 'PENDING'`, campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for rows.Next() {
		var scheduledAt time.Time
		var offset int
		if err := rows.Scan(&scheduledAt, &offset); err != nil {
			t.Fatal(err)
		}
		want := newStart.Add(time.Duration(offset) * time.Second)
		if !scheduledAt.Round(time.Second).Equal(want.Round(time.Second)) {
			t.Errorf("job with offset %d is at %v, want %v", offset, scheduledAt, want)
		}
	}
}

// TestUnsubscribeCancelsPendingJobs covers the STOP path end to end.
func TestUnsubscribeCancelsPendingJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600, -1800, 0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	isStop, err := f.campaignSvc.IsUnsubscribe(ctx, nil, contact.ID, "ТОҚТАТУ")
	if err != nil {
		t.Fatal(err)
	}
	if !isStop {
		t.Fatal("ТОҚТАТУ was not recognised as an unsubscribe keyword")
	}

	if err := f.contactRepo.SetOptOut(ctx, nil, contact.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.campaignSvc.StopForContact(ctx, contact.ID, domain.EnrollmentUnsubscribed, "stop"); err != nil {
		t.Fatal(err)
	}

	var pending int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages WHERE contact_id = $1 AND status = 'PENDING'`,
		contact.ID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Errorf("%d jobs are still pending after unsubscribe", pending)
	}

	updated, err := f.contactRepo.GetByID(ctx, contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.OptedOut || updated.Status != domain.ContactUnsubscribed {
		t.Errorf("contact state = %s / opted_out=%v", updated.Status, updated.OptedOut)
	}
	if updated.CanReceiveMessages() {
		t.Error("an unsubscribed contact must not be messageable")
	}
}

func TestCompleteFinishedEnrollments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(time.Hour)
	campaign := f.createCampaign(t, "Completion", eventStart, []int{0, 60})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	if _, err := testDB.Exec(ctx,
		`UPDATE scheduled_messages SET status = 'SENT', sent_at = now() WHERE campaign_id = $1`,
		campaign.ID); err != nil {
		t.Fatal(err)
	}

	completed, err := f.campaignSvc.CompleteFinishedEnrollments(ctx)
	if err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}
	if completed != 1 {
		t.Errorf("completed %d enrollments, want 1", completed)
	}

	var status domain.EnrollmentStatus
	if err := testDB.QueryRow(ctx,
		`SELECT status FROM campaign_contacts WHERE campaign_id = $1 AND contact_id = $2`,
		campaign.ID, contact.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != domain.EnrollmentCompleted {
		t.Errorf("enrollment status = %s, want COMPLETED", status)
	}
}

// TestUniqueConstraintBlocksDuplicateStepDelivery asserts the database-level
// guarantee directly, independent of the service layer.
func TestUniqueConstraintBlocksDuplicateStepDelivery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(24 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{0})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var enrollmentID, stepID uuid.UUID
	if err := testDB.QueryRow(ctx,
		`SELECT enrollment_id, campaign_step_id FROM scheduled_messages LIMIT 1`).
		Scan(&enrollmentID, &stepID); err != nil {
		t.Fatal(err)
	}

	created, err := f.jobRepo.Enqueue(ctx, nil, []scheduler.NewJob{{
		CampaignID:   campaign.ID,
		ContactID:    contact.ID,
		EnrollmentID: enrollmentID,
		StepID:       stepID,
		RunNumber:    1,
		ScheduledAt:  time.Now().UTC(),
	}})
	if err != nil {
		t.Fatalf("Enqueue should absorb the conflict, not fail: %v", err)
	}
	if created != 0 {
		t.Errorf("a duplicate job was inserted (created=%d)", created)
	}
}

func formatPhone(index int) string {
	return "7701" + pad(index)
}

func pad(index int) string {
	const width = 7
	digits := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		digits[i] = byte('0' + index%10)
		index /= 10
	}
	return string(digits)
}

func workerName(id int) string {
	return "worker-" + string(rune('a'+id))
}

// campaignsActionEnrolled mirrors the service constant without importing it
// under a long name.
const campaignsActionEnrolled = "ENROLLED"
