//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
)

// The end-to-end verification from the brief: a full campaign queue drained
// from database state alone, across a simulated restart and a provider outage,
// with nothing lost and nothing sent twice.
//
// These drive the real claim/transition machinery rather than a mock of it,
// because the guarantees being checked — atomic claiming, lease recovery,
// retry — live in the SQL, not in the Go around it.

// drainQueue claims and completes due jobs the way a worker does, until the
// queue stops producing them. deliver decides each job's fate, which is how a
// provider failure is injected without a network.
func drainQueue(t *testing.T, f *fixture, worker string, deliver func(domain.ScheduledMessage) error) (sent, failed int) {
	t.Helper()
	ctx := context.Background()

	for pass := 0; pass < 50; pass++ {
		jobs := f.claim(t, worker, 25)
		if len(jobs) == 0 {
			return sent, failed
		}

		for _, job := range jobs {
			if err := deliver(job); err != nil {
				// Mirrors the worker's permanent-failure path.
				if failErr := f.jobRepo.Fail(ctx, job.ID, err.Error()); failErr != nil {
					t.Fatalf("Fail: %v", failErr)
				}
				failed++
				continue
			}
			markSent(t, job.ID)
			sent++
		}
	}

	t.Fatal("queue did not drain within 50 passes")
	return sent, failed
}

// advanceQueue brings still-future jobs forward so a test does not have to
// wait out real time.
//
// It rewrites scheduled_at only, which is exactly what the passage of time
// would change; every status transition still goes through the real claim and
// completion path. Catch-up messages land a few seconds after enrolment by
// design, and a test that slept for them would be slow for no extra coverage.
func advanceQueue(t *testing.T, campaignID uuid.UUID) int64 {
	t.Helper()

	tag, err := testDB.Exec(context.Background(), `
		UPDATE scheduled_messages SET scheduled_at = now()
		WHERE campaign_id = $1 AND status = 'PENDING' AND scheduled_at > now()`, campaignID)
	if err != nil {
		t.Fatalf("advance queue: %v", err)
	}
	return tag.RowsAffected()
}

// countByStatus reports the campaign's queue as the admin panel would show it.
func countByStatus(t *testing.T, campaignID uuid.UUID) map[string]int {
	t.Helper()

	rows, err := testDB.Query(context.Background(),
		`SELECT status, count(*) FROM scheduled_messages WHERE campaign_id = $1 GROUP BY status`,
		campaignID)
	if err != nil {
		t.Fatalf("count by status: %v", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[status] = n
	}
	return out
}

// TestFullPipelineDeliversEveryStep is the acceptance run: eight steps, ten
// contacts, eighty jobs, every one of them delivered exactly once and every
// enrollment closed only when its steps are all resolved.
func TestFullPipelineDeliversEveryStep(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Every step is already due, so the whole queue is drainable in one run
	// while still exercising the real ordering and claim rules.
	eventStart := time.Now().UTC().Add(-time.Second)
	offsets := []int{-18000, -10800, -7200, -3600, -1800, -900, -450, 0}
	campaign := f.createCampaign(t, "Webinar", eventStart, offsets)
	f.addTrigger(t, campaign.ID, "Айран")

	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	const contacts = 10
	for i := 0; i < contacts; i++ {
		contact := f.createContact(t, fmt.Sprintf("7701555%04d", i))
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
			t.Fatalf("HandleTrigger %d: %v", i, err)
		}
	}

	// Catch-up is on for this campaign, so steps already past collapse to one
	// immediate message per contact and the rest are recorded as skipped. What
	// must hold is that every step is accounted for, not that every step sends.
	want := len(offsets) * contacts
	var rows int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1`, campaign.ID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != want {
		t.Fatalf("%d job rows, want %d (%d steps x %d contacts)", rows, want, len(offsets), contacts)
	}

	delivered := map[uuid.UUID]int{}
	deliver := func(job domain.ScheduledMessage) error {
		delivered[job.ID]++
		return nil
	}

	sent, failed := drainQueue(t, f, "worker-1", deliver)

	// Catch-up messages are placed a moment after the contact wrote in, so they
	// are not due on the first pass. Let that moment arrive and drain again.
	if advanceQueue(t, campaign.ID) > 0 {
		more, moreFailed := drainQueue(t, f, "worker-1", deliver)
		sent += more
		failed += moreFailed
	}

	if failed != 0 {
		t.Errorf("%d jobs failed with a healthy provider", failed)
	}
	for id, n := range delivered {
		if n != 1 {
			t.Errorf("job %s was delivered %d times, want exactly 1", id, n)
		}
	}

	status := countByStatus(t, campaign.ID)
	if status["PENDING"]+status["PROCESSING"] != 0 {
		t.Errorf("queue not drained: %d pending, %d processing",
			status["PENDING"], status["PROCESSING"])
	}
	if status["SENT"] != sent {
		t.Errorf("%d rows marked SENT, drained %d", status["SENT"], sent)
	}
	if total := status["SENT"] + status["CANCELLED"] + status["FAILED"]; total != want {
		t.Errorf("%d rows in a terminal state, want all %d", total, want)
	}

	// Only now may the contacts be closed out.
	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}

	var active int
	if err := testDB.QueryRow(ctx,
		`SELECT count(*) FROM campaign_contacts WHERE campaign_id = $1 AND status = 'ACTIVE'`,
		campaign.ID).Scan(&active); err != nil {
		t.Fatalf("count active: %v", err)
	}
	if active != 0 {
		t.Errorf("%d enrollments still ACTIVE after the queue drained", active)
	}

	report, err := f.campaignSvc.Consistency(ctx)
	if err != nil {
		t.Fatalf("Consistency: %v", err)
	}
	for _, check := range report.Checks {
		if !check.Healthy {
			t.Errorf("consistency check %q: %d problems (%s)", check.Name, check.Count, check.Detail)
		}
	}
}

// TestRestartResumesQueueWithoutResending is the restart guarantee: work
// already done stays done, work outstanding continues.
func TestRestartResumesQueueWithoutResending(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Second)
	campaign := f.createCampaign(t, "Restart", eventStart,
		[]int{-8, -7, -6, -5, -4, -3, -2, -1})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	pending := 0
	for _, job := range liveJobs(t, f, enrollment.ID) {
		if job.Status == domain.JobPending {
			pending++
		}
	}
	if pending < 3 {
		t.Fatalf("%d pending jobs, need at least 3 to test a partial run", pending)
	}

	// Process three, then "crash": the claims are left holding a lease.
	firstRun := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		jobs := f.claim(t, "worker-before-restart", 1)
		if len(jobs) == 0 {
			break
		}
		markSent(t, jobs[0].ID)
		firstRun[jobs[0].ID] = true
	}
	if len(firstRun) == 0 {
		t.Fatal("no jobs were processed before the restart")
	}

	// Restart: a fresh worker, and the startup recovery sweep.
	if _, err := f.jobRepo.ReleaseStale(ctx, 0); err != nil {
		t.Fatalf("ReleaseStale: %v", err)
	}

	secondRun := map[uuid.UUID]bool{}
	drainQueue(t, f, "worker-after-restart", func(job domain.ScheduledMessage) error {
		secondRun[job.ID] = true
		return nil
	})

	for id := range firstRun {
		if secondRun[id] {
			t.Errorf("job %s was sent again after the restart", id)
		}
	}

	status := countByStatus(t, campaign.ID)
	if status["PENDING"]+status["PROCESSING"] != 0 {
		t.Errorf("after restart: %d pending, %d processing — the queue must finish",
			status["PENDING"], status["PROCESSING"])
	}
}

// TestCrashDuringProcessingIsRecovered covers the hard case: the process dies
// with jobs held in PROCESSING. The lease is what gets them back.
func TestCrashDuringProcessingIsRecovered(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Second)
	campaign := f.createCampaign(t, "Crash", eventStart, []int{-4, -3, -2, -1})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}

	// Claim without completing: this is a worker that died mid-send.
	claimed := f.claim(t, "doomed-worker", 5)
	if len(claimed) == 0 {
		t.Fatal("nothing was claimed")
	}
	for _, job := range claimed {
		if job.Status != domain.JobProcessing {
			t.Fatalf("claimed job is %s, want PROCESSING", job.Status)
		}
	}

	// Before the lease expires the jobs stay held — they are not lost, and they
	// are not handed to anyone else.
	if got := countByStatus(t, campaign.ID)["PROCESSING"]; got != len(claimed) {
		t.Errorf("%d jobs PROCESSING, want %d", got, len(claimed))
	}

	// The lease expires and the recovery sweep returns them.
	released, err := f.jobRepo.ReleaseStale(ctx, 0)
	if err != nil {
		t.Fatalf("ReleaseStale: %v", err)
	}
	if int(released) != len(claimed) {
		t.Errorf("recovered %d jobs, want %d", released, len(claimed))
	}

	sent, _ := drainQueue(t, f, "replacement-worker", func(domain.ScheduledMessage) error { return nil })
	if sent == 0 {
		t.Error("no jobs were delivered after recovery")
	}
	if got := countByStatus(t, campaign.ID)["PROCESSING"]; got != 0 {
		t.Errorf("%d jobs still stuck in PROCESSING", got)
	}
}

// TestProviderFailureRetainsJobAndBlocksCompletion is the Green API outage
// case: a failure must not lose the message and must not let the campaign
// declare itself finished.
func TestProviderFailureRetainsJobAndBlocksCompletion(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Second)
	campaign := f.createCampaign(t, "Outage", eventStart, []int{-2, -1})
	f.addTrigger(t, campaign.ID, "Айран")

	contact := f.createContact(t, "77011234567")
	match, _ := f.campaignSvc.MatchTrigger(ctx, nil, "Айран")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, time.Now().UTC()); err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	enrollment := enrollmentOf(t, f, campaign.ID, contact.ID)

	// One job, one provider error, retried with backoff rather than dropped.
	jobs := f.claim(t, "worker-1", 1)
	if len(jobs) == 0 {
		t.Fatal("nothing was claimed")
	}
	job := jobs[0]

	next := time.Now().UTC().Add(5 * time.Second)
	if err := f.jobRepo.Retry(ctx, job.ID, next, "green api: 502 bad gateway"); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	after := allJobs(t, f, enrollment.ID)
	var found bool
	for _, j := range after {
		if j.ID != job.ID {
			continue
		}
		found = true
		if j.Status != domain.JobPending {
			t.Errorf("after a retryable failure the job is %s, want PENDING", j.Status)
		}
	}
	if !found {
		t.Fatal("the failed job disappeared from the queue")
	}

	// The error and the attempt must be persisted, so the operator can see it.
	var attempts int
	var lastError string
	if err := testDB.QueryRow(ctx,
		`SELECT attempt_count, last_error FROM scheduled_messages WHERE id = $1`,
		job.ID).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempt_count = %d, want 1", attempts)
	}
	if lastError == "" {
		t.Error("last_error is empty; a failure must leave an explanation")
	}

	// And the campaign must not consider this contact finished.
	if _, err := f.campaignSvc.CompleteFinishedEnrollments(ctx); err != nil {
		t.Fatalf("CompleteFinishedEnrollments: %v", err)
	}
	if got := enrollmentOf(t, f, campaign.ID, contact.ID).Status; got != domain.EnrollmentActive {
		t.Errorf("enrollment is %s during a provider outage, want ACTIVE", got)
	}
}
