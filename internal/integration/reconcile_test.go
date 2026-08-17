//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// These tests are the regression suite for the failure found in production:
// a campaign with five steps, an enrolled contact, and only two jobs. The
// database is the evidence, so the tests reproduce the sequence that produced
// it rather than asserting on the repaired outcome alone.

// liveJobs returns the jobs that will actually be delivered, keyed by step.
// Recorded skips are excluded: they are rows, but they are not sends.
func liveJobs(t *testing.T, f *fixture, enrollmentID uuid.UUID) map[uuid.UUID]scheduler.ExistingJob {
	t.Helper()

	all, err := f.jobRepo.JobsForEnrollment(context.Background(), nil, enrollmentID)
	if err != nil {
		t.Fatalf("JobsForEnrollment: %v", err)
	}

	out := make(map[uuid.UUID]scheduler.ExistingJob, len(all))
	for _, job := range all {
		if job.Status == domain.JobCancelled && domain.IsSkipReason(job.CancelReason) {
			continue
		}
		out[job.StepID] = job
	}
	return out
}

// allJobs returns every row, skips included.
func allJobs(t *testing.T, f *fixture, enrollmentID uuid.UUID) []scheduler.ExistingJob {
	t.Helper()

	all, err := f.jobRepo.JobsForEnrollment(context.Background(), nil, enrollmentID)
	if err != nil {
		t.Fatalf("JobsForEnrollment: %v", err)
	}
	return all
}

// markSent marks a job delivered without inventing a message row.
//
// The repository's MarkSent links the job to a real message, which these tests
// do not have; the foreign key would reject a placeholder. What matters here is
// the job's state, so the row is transitioned directly.
func markSent(t *testing.T, jobID uuid.UUID) {
	t.Helper()

	if _, err := testDB.Exec(context.Background(), `
		UPDATE scheduled_messages SET
			status = 'SENT', sent_at = now(), attempt_count = attempt_count + 1,
			locked_by = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1`, jobID); err != nil {
		t.Fatalf("mark job sent: %v", err)
	}
}

func enrollmentOf(t *testing.T, f *fixture, campaignID, contactID uuid.UUID) *domain.Enrollment {
	t.Helper()

	e, err := f.campaignRepo.FindEnrollment(context.Background(), nil, campaignID, contactID)
	if err != nil {
		t.Fatalf("FindEnrollment: %v", err)
	}
	if e == nil {
		t.Fatal("enrollment not found")
	}
	return e
}

// addStep appends one enabled step to a campaign through the service, which is
// the path the admin panel uses.
func addStep(t *testing.T, f *fixture, campaignID uuid.UUID, name string, offset int) *domain.CampaignStep {
	t.Helper()

	templateID := f.createTemplate(t, name+" tpl "+uuid.NewString()[:8], "body "+name)
	step, err := f.campaignSvc.AddStep(context.Background(), campaignID, campaigns.StepInput{
		Name:          name,
		OffsetSeconds: offset,
		TemplateID:    templateID,
		Enabled:       true,
		ScheduleKind:  string(domain.ScheduleRelativeToEvent),
	})
	if err != nil {
		t.Fatalf("AddStep %s: %v", name, err)
	}
	return step
}

// TestStepsAddedAfterEnrollmentAreScheduled is the production failure itself.
//
// A contact enrolled when the campaign had two steps. Three more were added
// over the following ten minutes. Before the fix those three reached nobody:
// the enrollment had been closed out by the completion sweep in between, and
// the back-fill only considered ACTIVE enrollments.
func TestStepsAddedAfterEnrollmentAreScheduled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Айран кәсібі", eventStart, []int{-5 * 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	if got := len(liveJobs(t, f, enrollment.ID)); got != 1 {
		t.Fatalf("after enrollment: %d live jobs, want 1", got)
	}

	// The admin keeps building the campaign, exactly as the audit log shows.
	addStep(t, f, campaign.ID, "3 сағат қалғанда", -3*3600)
	addStep(t, f, campaign.ID, "1 сағат қалғанда", -3600)
	addStep(t, f, campaign.ID, "30 минут қалғанда", -1800)

	live := liveJobs(t, f, enrollment.ID)
	if len(live) != 4 {
		t.Fatalf("after adding three steps: %d live jobs, want 4 — steps added "+
			"after enrollment must reach contacts already in the campaign", len(live))
	}

	steps, err := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	for _, step := range steps {
		job, ok := live[step.ID]
		if !ok {
			t.Errorf("step %q has no job", step.Name)
			continue
		}
		want := eventStart.Add(time.Duration(step.OffsetSeconds) * time.Second)
		if !job.ScheduledAt.Equal(want) {
			t.Errorf("step %q scheduled at %s, want %s", step.Name, job.ScheduledAt, want)
		}
		if job.Status != domain.JobPending {
			t.Errorf("step %q job is %s, want PENDING", step.Name, job.Status)
		}
	}

	// And the contact must not have been closed out while work remains.
	reloaded := enrollmentOf(t, f, campaign.ID, contact.ID)
	if reloaded.Status != domain.EnrollmentActive {
		t.Errorf("enrollment is %s, want ACTIVE while steps are still pending", reloaded.Status)
	}
}

// TestCompletionRequiresEveryStep is the completion half of the bug.
//
// The old sweep asked "are there pending jobs?" and closed the enrollment when
// there were none — which is also true of an enrollment whose jobs were never
// created. Completion must be decided by the steps.
func TestCompletionRequiresEveryStep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600, -3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	jobs := liveJobs(t, f, enrollment.ID)
	if len(jobs) != 3 {
		t.Fatalf("%d live jobs, want 3", len(jobs))
	}

	// Send two of the three.
	sent := 0
	for _, job := range jobs {
		if sent == 2 {
			break
		}
		markSent(t, job.ID)
		sent++
	}

	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}
	if got := enrollmentOf(t, f, campaign.ID, contact.ID).Status; got != domain.EnrollmentActive {
		t.Fatalf("enrollment is %s with one step unsent, want ACTIVE", got)
	}

	// Send the last one; only now may it complete.
	for _, job := range liveJobs(t, f, enrollment.ID) {
		if job.Status == domain.JobPending {
			markSent(t, job.ID)
		}
	}

	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}
	if got := enrollmentOf(t, f, campaign.ID, contact.ID).Status; got != domain.EnrollmentCompleted {
		t.Errorf("enrollment is %s with every step sent, want COMPLETED", got)
	}
}

// TestCompletedEnrollmentReopensForNewStep covers the interaction that made the
// production bug unrecoverable: the contact was already COMPLETED when the
// remaining steps were created, which hid them from every repair path.
func TestCompletedEnrollmentReopensForNewStep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	for _, job := range liveJobs(t, f, enrollment.ID) {
		markSent(t, job.ID)
	}

	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}
	if got := enrollmentOf(t, f, campaign.ID, contact.ID).Status; got != domain.EnrollmentCompleted {
		t.Fatalf("enrollment is %s, want COMPLETED after its only step was sent", got)
	}

	// Now the admin adds the step they meant to add all along.
	addStep(t, f, campaign.ID, "30 минут қалғанда", -1800)

	reloaded := enrollmentOf(t, f, campaign.ID, contact.ID)
	if reloaded.Status != domain.EnrollmentActive {
		t.Errorf("enrollment is %s after a new step was added, want ACTIVE", reloaded.Status)
	}
	if got := len(liveJobs(t, f, enrollment.ID)); got != 2 {
		t.Errorf("%d live jobs after the new step, want 2", got)
	}
}

// TestReconcileIsIdempotent runs the engine repeatedly and asserts the queue
// stops changing. Reconciliation runs every 30 seconds in production, so a pass
// that creates work is a pass that duplicates messages.
func TestReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600, -3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	before := len(allJobs(t, f, enrollment.ID))

	for i := 0; i < 10; i++ {
		stats, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID)
		if err != nil {
			t.Fatalf("ReconcileCampaign pass %d: %v", i, err)
		}
		if stats.JobsCreated != 0 || stats.SkipsRecorded != 0 || stats.JobsMoved != 0 {
			t.Errorf("pass %d changed a settled queue: created=%d skipped=%d moved=%d",
				i, stats.JobsCreated, stats.SkipsRecorded, stats.JobsMoved)
		}
	}

	if after := len(allJobs(t, f, enrollment.ID)); after != before {
		t.Errorf("job count moved from %d to %d across ten reconciliations", before, after)
	}
}

// TestStepEditMovesPendingJob covers editing a step after contacts are in the
// funnel: the queued message moves, and no second one appears.
func TestStepEditMovesPendingJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)
	steps, _ := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	target := steps[1]

	if _, err := f.campaignSvc.UpdateStep(ctx, target.ID, campaigns.StepInput{
		Name:          target.Name,
		OffsetSeconds: -2 * 3600,
		TemplateID:    target.TemplateID,
		Enabled:       true,
		ScheduleKind:  string(domain.ScheduleRelativeToEvent),
	}); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}

	live := liveJobs(t, f, enrollment.ID)
	if len(live) != 2 {
		t.Fatalf("%d live jobs after the edit, want 2 — an edit must not duplicate", len(live))
	}

	want := eventStart.Add(-2 * time.Hour)
	if got := live[target.ID].ScheduledAt; !got.Equal(want) {
		t.Errorf("edited step is scheduled at %s, want %s", got, want)
	}
}

// TestStepEditIntoFutureRevivesMissingJob is the second, independent defect.
//
// A step added while its moment has already passed is recorded as skipped and
// gets no live job. Moving it into the future must produce one — the old
// reschedule path was an UPDATE, so with no row to update the edit silently did
// nothing and the message was lost for good.
func TestStepEditIntoFutureRevivesMissingJob(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(2 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	// A step whose time is three hours before an event that is two hours away:
	// already in the past, so it is recorded as skipped rather than queued.
	late := addStep(t, f, campaign.ID, "өтіп кеткен", -3*3600)

	if _, ok := liveJobs(t, f, enrollment.ID)[late.ID]; ok {
		t.Fatal("a step whose moment has passed must not be queued")
	}
	var skipped bool
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.StepID == late.ID {
			skipped = true
			if job.CancelReason != domain.SkipStepExpired {
				t.Errorf("skip reason is %q, want %q", job.CancelReason, domain.SkipStepExpired)
			}
		}
	}
	if !skipped {
		t.Fatal("an expired step must still be recorded, not left absent")
	}

	// The admin corrects it to half an hour before the event.
	if _, err := f.campaignSvc.UpdateStep(ctx, late.ID, campaigns.StepInput{
		Name:          late.Name,
		OffsetSeconds: -1800,
		TemplateID:    late.TemplateID,
		Enabled:       true,
		ScheduleKind:  string(domain.ScheduleRelativeToEvent),
	}); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}

	live := liveJobs(t, f, enrollment.ID)
	job, ok := live[late.ID]
	if !ok {
		t.Fatal("a step corrected into the future must acquire a live job")
	}
	if job.Status != domain.JobPending {
		t.Errorf("revived job is %s, want PENDING", job.Status)
	}
	if want := eventStart.Add(-1800 * time.Second); !job.ScheduledAt.Equal(want) {
		t.Errorf("revived job is at %s, want %s", job.ScheduledAt, want)
	}

	// The revival must reuse the row, not add one.
	var rows int
	for _, j := range allJobs(t, f, enrollment.ID) {
		if j.StepID == late.ID {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d rows for the corrected step, want exactly 1", rows)
	}
}

// TestDisabledStepIsSkippedNotLost checks the disable path records a decision
// rather than deleting one.
func TestDisabledStepIsSkippedNotLost(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	steps, _ := f.campaignRepo.ListSteps(ctx, nil, campaign.ID)
	target := steps[1]

	if _, err := f.campaignSvc.UpdateStep(ctx, target.ID, campaigns.StepInput{
		Name:          target.Name,
		OffsetSeconds: target.OffsetSeconds,
		TemplateID:    target.TemplateID,
		Enabled:       false,
		ScheduleKind:  string(domain.ScheduleRelativeToEvent),
	}); err != nil {
		t.Fatalf("UpdateStep: %v", err)
	}

	if _, ok := liveJobs(t, f, enrollment.ID)[target.ID]; ok {
		t.Error("a disabled step must not keep a live job")
	}

	var found bool
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.StepID == target.ID {
			found = true
			if job.CancelReason != domain.SkipStepDisabled {
				t.Errorf("skip reason is %q, want %q", job.CancelReason, domain.SkipStepDisabled)
			}
		}
	}
	if !found {
		t.Error("the disabled step's row must remain, so the decision stays visible")
	}
}

// TestReconcileRepairsDeletedJobs is the self-healing guarantee. A job removed
// behind the application's back — the shape of every bug not yet found — must
// come back within one sweep.
func TestReconcileRepairsDeletedJobs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600, -3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	if _, err := testDB.Exec(ctx,
		`DELETE FROM scheduled_messages WHERE enrollment_id = $1`, enrollment.ID); err != nil {
		t.Fatalf("simulate lost jobs: %v", err)
	}
	if got := len(allJobs(t, f, enrollment.ID)); got != 0 {
		t.Fatalf("%d jobs survived the delete", got)
	}

	stats, err := f.campaignSvc.ReconcileAll(ctx)
	if err != nil {
		t.Fatalf("ReconcileAll: %v", err)
	}
	if stats.JobsCreated != 3 {
		t.Errorf("reconciliation created %d jobs, want 3", stats.JobsCreated)
	}
	if got := len(liveJobs(t, f, enrollment.ID)); got != 3 {
		t.Errorf("%d live jobs after repair, want 3", got)
	}
}

// TestSentJobsAreNeverRecreated is the anti-duplicate guarantee across a
// repair: a step that has already been delivered must stay delivered, once.
func TestSentJobsAreNeverRecreated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	var sentStep uuid.UUID
	for stepID, job := range liveJobs(t, f, enrollment.ID) {
		markSent(t, job.ID)
		sentStep = stepID
		break
	}

	for i := 0; i < 5; i++ {
		if _, err := f.campaignSvc.ReconcileCampaign(ctx, campaign.ID); err != nil {
			t.Fatalf("ReconcileCampaign: %v", err)
		}
	}

	var sentCount int
	for _, job := range allJobs(t, f, enrollment.ID) {
		if job.StepID == sentStep {
			sentCount++
			if job.Status != domain.JobSent {
				t.Errorf("the sent job is now %s; SENT must be immutable", job.Status)
			}
		}
	}
	if sentCount != 1 {
		t.Errorf("%d rows for the sent step, want exactly 1", sentCount)
	}
}

// TestConcurrentReconcileCreatesNoDuplicates puts the database's uniqueness
// guarantee under real contention, rather than trusting a read-then-write.
func TestConcurrentReconcileCreatesNoDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(6 * time.Hour)
	campaign := f.createCampaign(t, "Webinar", eventStart, []int{-5 * 3600, -3 * 3600, -3600})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	if _, err := testDB.Exec(ctx,
		`DELETE FROM scheduled_messages WHERE enrollment_id = $1`, enrollment.ID); err != nil {
		t.Fatalf("simulate lost jobs: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Errors are expected under SQLite write contention; the assertion
			// that matters is on the resulting rows.
			_, _ = f.campaignSvc.ReconcileCampaign(ctx, campaign.ID)
		}()
	}
	wg.Wait()

	perStep := map[uuid.UUID]int{}
	for _, job := range allJobs(t, f, enrollment.ID) {
		perStep[job.StepID]++
	}
	if len(perStep) != 3 {
		t.Errorf("%d steps have rows, want 3", len(perStep))
	}
	for stepID, n := range perStep {
		if n != 1 {
			t.Errorf("step %s has %d rows, want exactly 1", stepID, n)
		}
	}
}

// TestFullCampaignQueueIsComplete is the acceptance check from the brief: eight
// steps, ten contacts, eighty jobs, none missing and none duplicated.
func TestFullCampaignQueueIsComplete(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(8 * time.Hour)
	offsets := []int{
		-5 * 3600, // 16:00
		-3 * 3600, // 18:00
		-2 * 3600, // 19:00
		-3600,     // 20:00
		-1800,     // 20:30
		-900,      // 20:45
		-450,      // 20:52:30 — fractional minutes must survive
		0,         // 21:00
	}
	campaign := f.createCampaign(t, "Webinar", eventStart, offsets)
	f.addTrigger(t, campaign.ID, "Айран")

	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	for i := 0; i < 10; i++ {
		contact := f.createContact(t, fmt.Sprintf("7701123%04d", i))
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("HandleTrigger %d: %v", i, err)
		}
	}

	var total int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'PENDING'`,
		campaign.ID).Scan(&total); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if want := len(offsets) * 10; total != want {
		t.Errorf("%d pending jobs, want %d (%d steps x 10 contacts)", total, want, len(offsets))
	}

	// The exact-seconds step must land on its exact second.
	var at time.Time
	if err := testDB.QueryRow(ctx, `
		SELECT sm.scheduled_at FROM scheduled_messages sm
		JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
		WHERE sm.campaign_id = $1 AND cs.offset_seconds = -450 LIMIT 1`,
		campaign.ID).Scan(&at); err != nil {
		t.Fatalf("read fractional-minute step: %v", err)
	}
	if want := eventStart.Add(-450 * time.Second); !at.Equal(want) {
		t.Errorf("the -7m30s step is at %s, want %s", at, want)
	}

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

// campaignStepInput rebuilds a step's input with a new offset, which is what an
// operator editing the time in the panel produces.
func campaignStepInput(step domain.CampaignStep, offset int) campaigns.StepInput {
	return campaigns.StepInput{
		Name:          step.Name,
		OffsetSeconds: offset,
		TemplateID:    step.TemplateID,
		Enabled:       step.Enabled,
		ScheduleKind:  string(step.ScheduleKind),
	}
}
