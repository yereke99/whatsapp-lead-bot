package campaigns

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

// Reconciliation is what makes the schedule survive being edited.
//
// The original design created jobs at exactly two moments — when a contact
// enrolled, and when a step was added — and trusted both to be complete. They
// are not, and cannot be: a step created a minute after a contact enrolls, an
// offset edited while the queue drains, a completion sweep that ran between the
// two, a crash halfway through a transaction. Every one of those is a chance
// for a step to end up with no job, and an absent job looks exactly like a step
// that was never meant to run.
//
// So the queue is not treated as something that is built once and maintained by
// hand afterwards. It is *derived*. The invariant is:
//
//	for every enrollment that is still running,
//	  for every step of its campaign,
//	    there is exactly one job, in exactly one of two shapes:
//	      a live job with the time the step resolves to, or
//	      a recorded skip explaining why the step will not run.
//
// Reconcile re-derives that from campaign_steps and repairs any difference. It
// is idempotent by construction: the desired state is computed from stored
// configuration, not from history, so running it once and running it a hundred
// times reach the same place. Enrollment-time scheduling is now just the first
// reconciliation rather than the only one.
//
// The "recorded skip" half matters as much as the create half. A step that will
// not run for a contact is written down as a CANCELLED job carrying a skip
// reason, not left absent. That is what makes the invariant checkable at all:
// with skips recorded, "step has no row" means something is wrong, always.

// grace treats a step whose moment passed a moment ago as still on time, which
// absorbs clock skew and a slow tick. It matches the planner's default so
// enrollment and reconciliation agree on what "already past" means.
const reconcileGrace = time.Minute

// ReconcileStats reports what a reconciliation pass changed. The counters are
// what the admin action and the periodic log line report, and what the tests
// assert on.
type ReconcileStats struct {
	EnrollmentsChecked int `json:"enrollments_checked"`
	StepsChecked       int `json:"steps_checked"`
	ExistingJobs       int `json:"existing_jobs"`
	JobsCreated        int `json:"jobs_created"`
	SkipsRecorded      int `json:"skips_recorded"`
	JobsMoved          int `json:"jobs_moved"`
	JobsCancelled      int `json:"jobs_cancelled"`
	EnrollmentsReopen  int `json:"enrollments_reopened"`
	EnrollmentsDone    int `json:"enrollments_completed"`
}

// Changed reports whether the pass had to repair anything, which is what
// decides if it is worth logging.
func (s ReconcileStats) Changed() bool {
	return s.JobsCreated > 0 || s.SkipsRecorded > 0 || s.JobsMoved > 0 ||
		s.JobsCancelled > 0 || s.EnrollmentsReopen > 0 || s.EnrollmentsDone > 0
}

func (s *ReconcileStats) add(other ReconcileStats) {
	s.EnrollmentsChecked += other.EnrollmentsChecked
	s.StepsChecked += other.StepsChecked
	s.ExistingJobs += other.ExistingJobs
	s.JobsCreated += other.JobsCreated
	s.SkipsRecorded += other.SkipsRecorded
	s.JobsMoved += other.JobsMoved
	s.JobsCancelled += other.JobsCancelled
	s.EnrollmentsReopen += other.EnrollmentsReopen
	s.EnrollmentsDone += other.EnrollmentsDone
}

// desired is the state one step should be in for one enrollment.
//
// Exactly one of the two shapes is populated: a run time, or a skip reason.
type desired struct {
	runAt      time.Time
	skipReason string
}

func (d desired) isSkip() bool { return d.skipReason != "" }

// resolveStep decides what one step means for one enrollment.
//
// This is deliberately the same arithmetic BuildPlan performs, applied to a
// single step: the two must never disagree, or a contact's schedule would
// depend on whether it was written at enrollment or by a later repair. The
// difference is catch-up, which belongs to enrollment alone — see reconcile
// below.
func resolveStep(campaign *domain.Campaign, step domain.CampaignStep, enrolledAt time.Time, triggerDelay time.Duration) desired {
	// A finished or archived campaign will not send again — the worker refuses
	// such jobs on the way out. Recording the step as skipped rather than
	// queueing it keeps reconciliation from creating work that would only be
	// cancelled a moment later, and lets the enrollment settle.
	if campaign.Status == domain.CampaignCompleted || campaign.Status == domain.CampaignArchived {
		return desired{skipReason: domain.SkipCampaignClosed}
	}
	if !step.Enabled {
		return desired{skipReason: domain.SkipStepDisabled}
	}

	// The audience cutoff. An enrolment's entry time never moves, so a contact
	// who was too early for this step stays too early for as long as the cutoff
	// stands — but the operator can withdraw or lower the cutoff itself, and
	// then the message is owed to them again. That is why the skip is recorded
	// as revivable like any other: the decision belongs to the configuration,
	// not to the contact.
	if !step.EligibleFor(enrolledAt) {
		return desired{skipReason: domain.SkipNotEligible}
	}

	if step.ScheduleKind == domain.ScheduleOnTrigger {
		delay := time.Duration(step.OffsetSeconds) * time.Second
		if delay < triggerDelay {
			delay = triggerDelay
		}
		return desired{runAt: enrolledAt.Add(delay)}
	}

	if campaign.EventStartAt == nil {
		return desired{skipReason: domain.SkipNoEventAnchor}
	}
	return desired{runAt: campaign.EventStartAt.Add(time.Duration(step.OffsetSeconds) * time.Second)}
}

// ReconcileEnrollment brings one enrollment's queue in line with its campaign.
//
// It runs inside the caller's transaction so a step edit and the job changes it
// implies commit together: an operator never observes a step that moved while
// its queue did not.
//
// Rules, in the order they matter:
//
//   - A terminal job is history. SENT is never rewritten, never recreated and
//     never resent; a recorded skip stays recorded; an operator's cancellation
//     stays cancelled. Reconciliation only ever fills gaps and moves live work.
//   - A PROCESSING job belongs to the worker holding it. Touching it would race
//     the send, so it is left alone and picked up on the next pass.
//   - A PENDING job may move, because that is what editing a step means.
//   - A missing job is created — as a live job if its moment is still ahead, as
//     a recorded skip if it is not. Reconciliation never applies catch-up: a
//     step added an hour late must not fire instantly at everyone who is
//     already in the funnel. Catch-up is a decision about a contact who has
//     just arrived, and only enrollment makes it.
func (s *Service) ReconcileEnrollment(
	ctx context.Context, tx sqlite.Querier,
	campaign *domain.Campaign, enrollment *domain.Enrollment,
	steps []domain.CampaignStep, now time.Time,
) (ReconcileStats, error) {
	existing, err := s.jobs.JobsForEnrollment(ctx, tx, enrollment.ID)
	if err != nil {
		return ReconcileStats{}, err
	}
	return s.reconcileEnrollmentWith(ctx, tx, campaign, enrollment, steps, existing, now)
}

// reconcileEnrollmentWith is the engine proper, working from jobs the caller
// has already loaded. The periodic sweep reads a whole campaign's jobs in one
// query and calls this per enrollment; the single-enrollment entry point above
// loads them itself.
func (s *Service) reconcileEnrollmentWith(
	ctx context.Context, tx sqlite.Querier,
	campaign *domain.Campaign, enrollment *domain.Enrollment,
	steps []domain.CampaignStep, existing []scheduler.ExistingJob, now time.Time,
) (ReconcileStats, error) {
	var stats ReconcileStats

	// A contact who unsubscribed or was cancelled out of the campaign is not
	// waiting for anything. Reconciliation must never resurrect them.
	if enrollment.Status != domain.EnrollmentActive && enrollment.Status != domain.EnrollmentCompleted {
		return stats, nil
	}

	stats.EnrollmentsChecked = 1
	stats.StepsChecked = len(steps)

	// Index by step for this run only. A restarted enrollment carries a higher
	// run_number, and its earlier run's jobs must not be mistaken for the
	// current one's — that is exactly what the run_number half of the unique
	// constraint is for.
	byStep := make(map[uuid.UUID]scheduler.ExistingJob, len(existing))
	for _, job := range existing {
		if job.RunNumber != enrollment.RunNumber {
			continue
		}
		byStep[job.StepID] = job
		stats.ExistingJobs++
	}

	cutoff := now.Add(-reconcileGrace)
	var missing []scheduler.NewJob

	// finished tracks whether every step ends this pass in a state the queue
	// will no longer act on. It is accumulated here rather than re-read
	// afterwards, because this loop already knows the outcome of each step —
	// and a second read would be one more query per enrollment per sweep.
	finished := true

	for _, step := range steps {
		want := resolveStep(campaign, step, enrollment.EnrolledAt, s.triggerDelay)
		job, found := byStep[step.ID]

		if !found {
			// A step with no row at all. This is the failure the whole engine
			// exists for: it is filled in, one way or the other, never left
			// absent.
			reason := want.skipReason
			runAt := want.runAt
			if reason == "" && runAt.Before(cutoff) {
				reason = domain.SkipStepExpired
			}
			if reason != "" {
				// A skip still needs a time on the row: the column is NOT NULL
				// and the panel sorts the queue by it, so the step appears in
				// the operator's timeline where it belongs rather than at the
				// epoch.
				if runAt.IsZero() {
					runAt = now
				}
				stats.SkipsRecorded++
			} else {
				stats.JobsCreated++
				finished = false
			}

			missing = append(missing, scheduler.NewJob{
				CampaignID:   campaign.ID,
				ContactID:    enrollment.ContactID,
				EnrollmentID: enrollment.ID,
				StepID:       step.ID,
				RunNumber:    enrollment.RunNumber,
				ScheduledAt:  runAt,
				SkipReason:   reason,
			})
			continue
		}

		// A recorded skip is the one terminal state that can be undone, because
		// it is a conclusion about the configuration rather than an event that
		// happened. If the step has since become applicable — moved into the
		// future, re-enabled, given an event anchor — the skip is revoked and
		// the step goes back in the queue.
		//
		// Nothing else terminal is reconsidered: a send happened, a failure
		// exhausted its attempts, an operator's cancellation was a decision.
		if job.Status == domain.JobCancelled && domain.IsSkipReason(job.CancelReason) {
			if want.isSkip() || want.runAt.Before(cutoff) {
				continue // still not applicable; leave the record as it stands
			}
			revived, err := s.jobs.ReviveSkipped(ctx, tx, job.ID, want.runAt)
			if err != nil {
				return stats, err
			}
			if revived {
				stats.JobsCreated++
				finished = false
				s.log.Info("JOB_REVIVED",
					slog.String("scheduled_message_id", job.ID.String()),
					slog.String("campaign_id", campaign.ID.String()),
					slog.String("enrollment_id", enrollment.ID.String()),
					slog.String("campaign_step_id", step.ID.String()),
					slog.String("was", job.CancelReason),
					slog.Time("scheduled_at", want.runAt))
			}
			continue
		}

		// Anything else not PENDING is either finished or in someone else's
		// hands. A PROCESSING job is still moving, so the enrollment is not.
		if job.Status != domain.JobPending {
			if !job.Status.IsTerminal() {
				finished = false
			}
			continue
		}

		// A live job that stays live keeps the enrollment open, whether or not
		// this pass moves it.
		if !want.isSkip() {
			finished = false
		}

		if want.isSkip() {
			ok, err := s.jobs.SkipPending(ctx, tx, job.ID, want.skipReason)
			if err != nil {
				return stats, err
			}
			if ok {
				stats.JobsCancelled++
				s.log.Info("JOB_SKIPPED",
					slog.String("scheduled_message_id", job.ID.String()),
					slog.String("campaign_id", campaign.ID.String()),
					slog.String("enrollment_id", enrollment.ID.String()),
					slog.String("campaign_step_id", step.ID.String()),
					slog.String("reason", want.skipReason))
			}
			continue
		}

		// The step's time changed under a job that has not gone out yet. Moving
		// it is the literal meaning of the edit. A job whose new time is in the
		// past is deliberately not skipped here — it is due, and due work gets
		// sent; the queue's own stale-job policy decides when something is too
		// old to bother with, in one place rather than two.
		if !job.ScheduledAt.Equal(want.runAt) {
			moved, err := s.jobs.MovePending(ctx, tx, job.ID, want.runAt)
			if err != nil {
				return stats, err
			}
			if moved {
				stats.JobsMoved++
				s.log.Info("JOB_RESCHEDULED",
					slog.String("scheduled_message_id", job.ID.String()),
					slog.String("campaign_id", campaign.ID.String()),
					slog.String("enrollment_id", enrollment.ID.String()),
					slog.String("campaign_step_id", step.ID.String()),
					slog.Time("from", job.ScheduledAt),
					slog.Time("to", want.runAt))
			}
		}
	}

	// One batched insert for everything that was missing. Enqueue resolves
	// conflicts in the database, so a concurrent enrollment or a second
	// reconciler racing on the same rows loses harmlessly instead of
	// duplicating.
	if len(missing) > 0 {
		inserted, err := s.jobs.Enqueue(ctx, tx, missing)
		if err != nil {
			return stats, err
		}
		if inserted != len(missing) {
			// Lost a race for some rows. The other writer created them, so the
			// invariant holds; only our accounting was optimistic.
			lost := len(missing) - inserted
			stats.JobsCreated = maxInt(0, stats.JobsCreated-lost)
		}
		for _, job := range missing {
			if job.SkipReason != "" {
				continue
			}
			s.log.Info("JOB_CREATED",
				slog.String("campaign_id", job.CampaignID.String()),
				slog.String("enrollment_id", job.EnrollmentID.String()),
				slog.String("campaign_step_id", job.StepID.String()),
				slog.String("contact_id", job.ContactID.String()),
				slog.Time("scheduled_at", job.ScheduledAt),
				slog.String("source", "reconcile"))
		}
	}

	// Settle the enrollment's own state against what the queue now says.
	if err := s.settleEnrollment(ctx, tx, enrollment, finished, &stats); err != nil {
		return stats, err
	}

	return stats, nil
}

// settleEnrollment decides ACTIVE vs COMPLETED from the steps, not from an
// empty queue.
//
// This is the second half of the original bug. Completion used to mean "no
// pending jobs remain", which is true of an enrollment that finished and
// equally true of one whose jobs were never created. A contact was therefore
// closed out three minutes before the operator added the steps that were meant
// to reach them, and being COMPLETED then hid them from the back-fill that
// would have repaired it.
//
// Completion now requires every step to be accounted for: a job exists for it,
// and that job has stopped moving.
func (s *Service) settleEnrollment(
	ctx context.Context, tx sqlite.Querier,
	enrollment *domain.Enrollment, finished bool,
	stats *ReconcileStats,
) error {
	switch {
	case finished && enrollment.Status == domain.EnrollmentActive:
		if err := s.repo.SetEnrollmentStatus(ctx, tx, enrollment.ID, domain.EnrollmentCompleted, ""); err != nil {
			return err
		}
		enrollment.Status = domain.EnrollmentCompleted
		stats.EnrollmentsDone++
		s.log.Info("enrollment completed",
			slog.String("enrollment_id", enrollment.ID.String()))

	case !finished && enrollment.Status == domain.EnrollmentCompleted:
		// A campaign that gained work after this contact was closed out. The
		// contact is still in the funnel, so the funnel reopens for them —
		// without this, every step added after the last one was sent would be
		// permanently unreachable.
		if err := s.repo.SetEnrollmentStatus(ctx, tx, enrollment.ID, domain.EnrollmentActive, ""); err != nil {
			return err
		}
		enrollment.Status = domain.EnrollmentActive
		stats.EnrollmentsReopen++
		s.log.Info("enrollment reopened: campaign gained applicable steps",
			slog.String("enrollment_id", enrollment.ID.String()))
	}

	return nil
}

// ReconcileCampaign reconciles every enrollment of one campaign.
//
// Each enrollment gets its own transaction rather than sharing one. A campaign
// with ten thousand contacts must not hold SQLite's single write lock for the
// whole sweep, and one bad enrollment must not roll back the repairs made for
// everyone else.
func (s *Service) ReconcileCampaign(ctx context.Context, campaignID uuid.UUID) (ReconcileStats, error) {
	var stats ReconcileStats

	campaign, err := s.repo.GetByID(ctx, nil, campaignID)
	if err != nil {
		return stats, err
	}
	if campaign == nil {
		return stats, ErrNotFound
	}

	// An archived campaign is closed for good; a draft has never run. Neither
	// should acquire a queue.
	if campaign.Status == domain.CampaignArchived || campaign.Status == domain.CampaignDraft {
		return stats, nil
	}

	steps, err := s.repo.ListSteps(ctx, nil, campaignID)
	if err != nil {
		return stats, err
	}

	enrollments, err := s.repo.EnrollmentsForReconcile(ctx, nil, &campaignID)
	if err != nil {
		return stats, err
	}

	now := time.Now().UTC()

	// Work through the enrollments in batches, loading each batch's jobs in one
	// query. A campaign with ten thousand contacts is then a few dozen reads
	// rather than ten thousand, which is what makes running this every thirty
	// seconds affordable.
	const batchSize = 500
	for start := 0; start < len(enrollments); start += batchSize {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}

		end := start + batchSize
		if end > len(enrollments) {
			end = len(enrollments)
		}
		batch := enrollments[start:end]

		ids := make([]uuid.UUID, len(batch))
		for i, e := range batch {
			ids[i] = e.ID
		}
		jobsByEnrollment, err := s.jobs.JobsForEnrollments(ctx, nil, ids)
		if err != nil {
			return stats, err
		}

		for i := range batch {
			enrollment := batch[i]
			existing := jobsByEnrollment[enrollment.ID]

			// Decide whether anything needs writing before opening a
			// transaction. SQLite has a single writer, and this sweep runs
			// alongside the workers actually sending messages — taking the
			// write lock to discover there was nothing to do would put the
			// health check in contention with the queue it is protecting.
			if !s.needsRepair(campaign, &enrollment, steps, existing, now) {
				stats.EnrollmentsChecked++
				stats.StepsChecked += len(steps)
				stats.ExistingJobs += len(existing)
				continue
			}

			var one ReconcileStats
			err := s.repo.DB().InTx(ctx, func(tx sqlite.Querier) error {
				// Re-read inside the transaction: the decision above was made
				// on an unlocked snapshot, and a worker may have moved a job in
				// between. Every write is additionally guarded by its own WHERE
				// clause, so a stale read can only cost a wasted statement,
				// never a wrong one.
				var err error
				one, err = s.ReconcileEnrollment(ctx, tx, campaign, &enrollment, steps, now)
				return err
			})
			if err != nil {
				return stats, fmt.Errorf("reconcile enrollment %s: %w", enrollment.ID, err)
			}
			stats.add(one)
		}
	}

	return stats, nil
}

// needsRepair reports whether an enrollment differs from what its campaign
// says it should be. It performs no writes and takes no locks.
//
// It deliberately mirrors the reconciler's own rules rather than approximating
// them: a mismatch it fails to notice is a message that never gets sent, which
// is the entire failure this system exists to prevent. Being wrong in the other
// direction merely costs one redundant transaction.
func (s *Service) needsRepair(
	campaign *domain.Campaign, enrollment *domain.Enrollment,
	steps []domain.CampaignStep, existing []scheduler.ExistingJob, now time.Time,
) bool {
	if enrollment.Status != domain.EnrollmentActive && enrollment.Status != domain.EnrollmentCompleted {
		return false
	}

	byStep := make(map[uuid.UUID]scheduler.ExistingJob, len(existing))
	for _, job := range existing {
		if job.RunNumber == enrollment.RunNumber {
			byStep[job.StepID] = job
		}
	}

	cutoff := now.Add(-reconcileGrace)
	finished := true

	for _, step := range steps {
		want := resolveStep(campaign, step, enrollment.EnrolledAt, s.triggerDelay)
		job, found := byStep[step.ID]

		if !found {
			return true // a step with no row: always a repair
		}

		if job.Status == domain.JobCancelled && domain.IsSkipReason(job.CancelReason) {
			if !want.isSkip() && !want.runAt.Before(cutoff) {
				return true // a skip that has become applicable again
			}
			continue
		}

		if job.Status != domain.JobPending {
			if !job.Status.IsTerminal() {
				finished = false
			}
			continue
		}

		if want.isSkip() {
			return true // a live job for a step that no longer applies
		}
		if !job.ScheduledAt.Equal(want.runAt) {
			return true // the step moved and the job has not
		}
		finished = false
	}

	// Nothing to create or move, but the enrollment's own status may still
	// disagree with its steps.
	return (finished && enrollment.Status == domain.EnrollmentActive) ||
		(!finished && enrollment.Status == domain.EnrollmentCompleted)
}

// ReconcileAll is the safety net.
//
// Event-driven reconciliation covers every path the code knows about. This
// covers the ones it does not: a step written by a migration or by hand, a
// transaction that committed the step and lost the jobs, a bug not yet found.
// It is cheap — a campaign whose queue is already correct performs reads and no
// writes — and it is what turns "we fixed the paths we found" into "a missing
// job repairs itself within one interval".
func (s *Service) ReconcileAll(ctx context.Context) (ReconcileStats, error) {
	var stats ReconcileStats

	ids, err := s.repo.ReconcilableCampaignIDs(ctx)
	if err != nil {
		return stats, err
	}

	for _, id := range ids {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		one, err := s.ReconcileCampaign(ctx, id)
		if err != nil {
			// One broken campaign must not stop the sweep for the rest.
			s.log.Error("reconciling campaign failed",
				slog.String("campaign_id", id.String()),
				slog.String("error", err.Error()))
			continue
		}
		stats.add(one)
	}

	return stats, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
