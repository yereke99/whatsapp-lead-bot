// Package scheduler owns the persistent outbound job queue and the workers
// that drain it.
//
// Jobs live in SQLite, never in memory, so a restart, crash or deploy loses
// nothing. Workers claim rows with a single UPDATE that both selects and
// transitions them; SQLite runs one writer at a time, so two workers can never
// come away holding the same job.
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

const jobColumns = `
	sm.id, sm.campaign_id, sm.contact_id, sm.enrollment_id, sm.campaign_step_id,
	sm.run_number, sm.scheduled_at, sm.status, sm.attempt_count, sm.next_attempt_at,
	sm.locked_by, sm.locked_at, sm.sent_at, sm.cancelled_at, sm.cancel_reason,
	sm.last_error, sm.message_id, sm.created_at, sm.updated_at`

// SQLite's RETURNING clause names columns of the updated table directly and
// rejects a table alias, so the claim query needs its own unprefixed list.
const jobColumnsUnqualified = `
	id, campaign_id, contact_id, enrollment_id, campaign_step_id,
	run_number, scheduled_at, status, attempt_count, next_attempt_at,
	locked_by, locked_at, sent_at, cancelled_at, cancel_reason,
	last_error, message_id, created_at, updated_at`

type Repository struct {
	db *sqlite.DB
}

func NewRepository(db *sqlite.DB) *Repository { return &Repository{db: db} }

func (r *Repository) querier(q sqlite.Querier) sqlite.Querier {
	if q != nil {
		return q
	}
	return r.db
}

// NewJob describes one row to enqueue.
type NewJob struct {
	CampaignID   uuid.UUID
	ContactID    uuid.UUID
	EnrollmentID uuid.UUID
	StepID       uuid.UUID
	RunNumber    int
	ScheduledAt  time.Time
}

// Enqueue inserts jobs, ignoring any that already exist.
//
// The (enrollment_id, campaign_step_id, run_number) unique constraint is the
// anti-duplicate guarantee: re-running enrollment after a partial failure, or
// two webhook deliveries racing, can never produce a second delivery of the
// same step.
func (r *Repository) Enqueue(ctx context.Context, q sqlite.Querier, jobs []NewJob) (int, error) {
	if len(jobs) == 0 {
		return 0, nil
	}

	const query = `
		INSERT INTO scheduled_messages (
			campaign_id, contact_id, enrollment_id, campaign_step_id, run_number, scheduled_at
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (enrollment_id, campaign_step_id, run_number) DO NOTHING`

	querier := r.querier(q)
	created := 0
	for _, j := range jobs {
		if j.RunNumber < 1 {
			j.RunNumber = 1
		}
		tag, err := querier.Exec(ctx, query,
			j.CampaignID, j.ContactID, j.EnrollmentID, j.StepID, j.RunNumber, j.ScheduledAt)
		if err != nil {
			return created, fmt.Errorf("enqueue job: %w", err)
		}
		created += int(tag.RowsAffected())
	}
	return created, nil
}

// Claim atomically moves up to limit due jobs into PROCESSING and returns them.
//
// The whole selection and state transition happens in one statement, so a
// crash between "found" and "claimed" is impossible. The statement runs under
// SQLite's write lock, so a second worker polling at the same moment sees the
// rows already in PROCESSING and picks up the next batch instead.
func (r *Repository) Claim(ctx context.Context, workerID string, limit int, now time.Time) ([]domain.ScheduledMessage, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
		UPDATE scheduled_messages AS sm
		SET status = 'PROCESSING', locked_by = $1, locked_at = $2, updated_at = now()
		FROM (
			SELECT id FROM scheduled_messages
			WHERE status = 'PENDING'
			  AND scheduled_at <= $2
			  AND (next_attempt_at IS NULL OR next_attempt_at <= $2)
			ORDER BY scheduled_at
			LIMIT $3
		) due
		WHERE sm.id = due.id
		RETURNING ` + jobColumnsUnqualified

	rows, err := r.db.Query(ctx, query, workerID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("claim jobs: %w", err)
	}
	defer rows.Close()

	var out []domain.ScheduledMessage
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *job)
	}
	return out, rows.Err()
}

// JobContext is everything a worker needs to render and deliver one job,
// fetched in a single query to keep the send path cheap.
type JobContext struct {
	Job domain.ScheduledMessage

	CampaignName        string
	CampaignStatus      domain.CampaignStatus
	CampaignTimezone    string
	CampaignLink        string
	CampaignEventStart  *time.Time
	CampaignMaxAttempts int

	StepName   string
	StepOffset int
	TemplateID uuid.UUID

	ContactPhone     string
	ContactChatID    string
	ContactName      string
	ContactPushName  string
	ContactOptedOut  bool
	ContactBlocked   bool
	ContactStatus    domain.ContactStatus
	ContactConsentAt *time.Time

	EnrollmentStatus domain.EnrollmentStatus
	StepEnabled      bool
}

func (r *Repository) LoadContext(ctx context.Context, jobID uuid.UUID) (*JobContext, error) {
	query := `
		SELECT ` + jobColumns + `,
			camp.name, camp.status, camp.timezone, camp.webinar_link, camp.event_start_at, camp.max_send_attempts,
			cs.name, cs.offset_seconds, cs.message_template_id, cs.enabled,
			c.phone, c.chat_id, c.name, c.push_name, c.opted_out, (c.blocked_at IS NOT NULL), c.status, c.first_contact_at,
			cc.status
		FROM scheduled_messages sm
		JOIN campaigns camp        ON camp.id = sm.campaign_id
		JOIN campaign_steps cs     ON cs.id = sm.campaign_step_id
		JOIN contacts c            ON c.id = sm.contact_id
		JOIN campaign_contacts cc  ON cc.id = sm.enrollment_id
		WHERE sm.id = $1`

	var jc JobContext
	row := r.db.QueryRow(ctx, query, jobID)

	dest := jobScanDest(&jc.Job)
	dest = append(dest,
		&jc.CampaignName, &jc.CampaignStatus, &jc.CampaignTimezone, &jc.CampaignLink,
		&jc.CampaignEventStart, &jc.CampaignMaxAttempts,
		&jc.StepName, &jc.StepOffset, &jc.TemplateID, &jc.StepEnabled,
		&jc.ContactPhone, &jc.ContactChatID, &jc.ContactName, &jc.ContactPushName,
		&jc.ContactOptedOut, &jc.ContactBlocked, &jc.ContactStatus, &jc.ContactConsentAt,
		&jc.EnrollmentStatus,
	)

	if err := row.Scan(dest...); err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("load job context: %w", err)
	}
	return &jc, nil
}

func (r *Repository) MarkSent(ctx context.Context, q sqlite.Querier, jobID, messageID uuid.UUID, sentAt time.Time) error {
	const query = `
		UPDATE scheduled_messages SET
			status = 'SENT', sent_at = $2, message_id = $3, locked_by = NULL, locked_at = NULL,
			last_error = '', attempt_count = attempt_count + 1, updated_at = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, jobID, sentAt, messageID)
	return err
}

// Retry returns a job to PENDING with a future next_attempt_at.
func (r *Repository) Retry(ctx context.Context, jobID uuid.UUID, nextAttempt time.Time, reason string) error {
	const query = `
		UPDATE scheduled_messages SET
			status = 'PENDING', attempt_count = attempt_count + 1, next_attempt_at = $2,
			last_error = $3, locked_by = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, jobID, nextAttempt, truncate(reason, 2000))
	return err
}

func (r *Repository) Fail(ctx context.Context, jobID uuid.UUID, reason string) error {
	const query = `
		UPDATE scheduled_messages SET
			status = 'FAILED', attempt_count = attempt_count + 1, last_error = $2,
			locked_by = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, query, jobID, truncate(reason, 2000))
	return err
}

func (r *Repository) Cancel(ctx context.Context, jobID uuid.UUID, reason string) error {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(), cancel_reason = $2,
			locked_by = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('PENDING', 'PROCESSING')`

	_, err := r.db.Exec(ctx, query, jobID, truncate(reason, 500))
	return err
}

// ReleaseStale returns jobs abandoned by a dead worker to PENDING.
//
// This is what makes a hard crash recoverable: a PROCESSING row whose lock has
// aged past the timeout is assumed orphaned and re-queued. The unique
// constraint plus the provider message id check keep the retry from producing
// a second WhatsApp message if the original send actually got through.
func (r *Repository) ReleaseStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'PENDING', locked_by = NULL, locked_at = NULL,
			last_error = CASE WHEN last_error = '' THEN 'worker lock expired; job requeued' ELSE last_error END,
			updated_at = now()
		WHERE status = 'PROCESSING' AND locked_at < $1`

	tag, err := r.db.Exec(ctx, query, time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CancelStale drops jobs whose send window has long passed, so an outage does
// not release a burst of obsolete "starts in 5 hours" messages hours late.
func (r *Repository) CancelStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(),
			cancel_reason = 'send window expired', updated_at = now()
		WHERE status = 'PENDING' AND scheduled_at < $1`

	tag, err := r.db.Exec(ctx, query, time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RescheduleCampaign recomputes pending job times after the event moves.
//
// Only PENDING rows are touched. Sent, failed and cancelled jobs are history
// and must never be rewritten.
func (r *Repository) RescheduleCampaign(ctx context.Context, q sqlite.Querier, campaignID uuid.UUID, eventStart time.Time) (int64, error) {
	const query = `
		UPDATE scheduled_messages AS sm SET
			scheduled_at = ts_add($2, cs.offset_seconds),
			updated_at = now()
		FROM campaign_steps cs
		WHERE cs.id = sm.campaign_step_id
		  AND sm.campaign_id = $1
		  AND sm.status = 'PENDING'
		  AND cs.schedule_kind = 'RELATIVE_TO_EVENT'`

	tag, err := r.querier(q).Exec(ctx, query, campaignID, eventStart)
	if err != nil {
		return 0, fmt.Errorf("reschedule campaign: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RescheduleStep recomputes pending jobs for a single step after its offset
// changed.
func (r *Repository) RescheduleStep(ctx context.Context, q sqlite.Querier, stepID uuid.UUID) (int64, error) {
	const query = `
		UPDATE scheduled_messages AS sm SET
			scheduled_at = ts_add(camp.event_start_at, cs.offset_seconds),
			updated_at = now()
		FROM campaign_steps cs
		JOIN campaigns camp ON camp.id = cs.campaign_id
		WHERE cs.id = sm.campaign_step_id
		  AND sm.campaign_step_id = $1
		  AND sm.status = 'PENDING'
		  AND cs.schedule_kind = 'RELATIVE_TO_EVENT'
		  AND camp.event_start_at IS NOT NULL`

	tag, err := r.querier(q).Exec(ctx, query, stepID)
	if err != nil {
		return 0, fmt.Errorf("reschedule step: %w", err)
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) CancelPendingForStep(ctx context.Context, q sqlite.Querier, stepID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(), cancel_reason = $2, updated_at = now()
		WHERE campaign_step_id = $1 AND status = 'PENDING'`

	tag, err := r.querier(q).Exec(ctx, query, stepID, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) CancelPendingForEnrollment(ctx context.Context, q sqlite.Querier, enrollmentID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(), cancel_reason = $2, updated_at = now()
		WHERE enrollment_id = $1 AND status = 'PENDING'`

	tag, err := r.querier(q).Exec(ctx, query, enrollmentID, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) CancelPendingForContact(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(), cancel_reason = $2, updated_at = now()
		WHERE contact_id = $1 AND status = 'PENDING'`

	tag, err := r.querier(q).Exec(ctx, query, contactID, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Repository) CancelPendingForCampaign(ctx context.Context, q sqlite.Querier, campaignID uuid.UUID, reason string) (int64, error) {
	const query = `
		UPDATE scheduled_messages SET
			status = 'CANCELLED', cancelled_at = now(), cancel_reason = $2, updated_at = now()
		WHERE campaign_id = $1 AND status = 'PENDING'`

	tag, err := r.querier(q).Exec(ctx, query, campaignID, reason)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// Requeue puts a failed job back in line for one more attempt, used by the
// admin "retry" action.
func (r *Repository) Requeue(ctx context.Context, jobID uuid.UUID) error {
	const query = `
		UPDATE scheduled_messages SET
			status = 'PENDING', next_attempt_at = NULL, attempt_count = 0,
			last_error = '', locked_by = NULL, locked_at = NULL, updated_at = now()
		WHERE id = $1 AND status IN ('FAILED', 'CANCELLED')`

	_, err := r.db.Exec(ctx, query, jobID)
	return err
}

// ListFilter drives the scheduled-messages admin page.
type ListFilter struct {
	CampaignID *uuid.UUID
	ContactID  *uuid.UUID
	Status     string
	From       *time.Time
	To         *time.Time
	Limit      int
	Offset     int
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]domain.ScheduledMessage, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}

	var args []any
	var clauses []string

	if f.CampaignID != nil {
		args = append(args, *f.CampaignID)
		clauses = append(clauses, fmt.Sprintf("sm.campaign_id = $%d", len(args)))
	}
	if f.ContactID != nil {
		args = append(args, *f.ContactID)
		clauses = append(clauses, fmt.Sprintf("sm.contact_id = $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		clauses = append(clauses, fmt.Sprintf("sm.status = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		clauses = append(clauses, fmt.Sprintf("sm.scheduled_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		clauses = append(clauses, fmt.Sprintf("sm.scheduled_at < $%d", len(args)))
	}

	where := "TRUE"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}

	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM scheduled_messages sm WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count jobs: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	query := `SELECT ` + jobColumns + `,
			camp.name, COALESCE(cs.name, ''), COALESCE(NULLIF(c.name, ''), c.push_name, ''), c.phone
		FROM scheduled_messages sm
		JOIN campaigns camp ON camp.id = sm.campaign_id
		LEFT JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
		JOIN contacts c ON c.id = sm.contact_id
		WHERE ` + where + `
		ORDER BY sm.scheduled_at DESC
		LIMIT $` + fmt.Sprint(len(args)-1) + ` OFFSET $` + fmt.Sprint(len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []domain.ScheduledMessage
	for rows.Next() {
		var job domain.ScheduledMessage
		dest := jobScanDest(&job)
		dest = append(dest, &job.CampaignName, &job.StepName, &job.ContactName, &job.ContactPhone)
		if err := rows.Scan(dest...); err != nil {
			return nil, 0, err
		}
		out = append(out, job)
	}
	return out, total, rows.Err()
}

// Stats summarises queue health for the dashboard.
type Stats struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Sent       int `json:"sent"`
	Failed     int `json:"failed"`
	Cancelled  int `json:"cancelled"`
	DueNow     int `json:"due_now"`
}

func (r *Repository) Stats(ctx context.Context) (*Stats, error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE status = 'PENDING'),
			count(*) FILTER (WHERE status = 'PROCESSING'),
			count(*) FILTER (WHERE status = 'SENT'),
			count(*) FILTER (WHERE status = 'FAILED'),
			count(*) FILTER (WHERE status = 'CANCELLED'),
			count(*) FILTER (WHERE status = 'PENDING' AND scheduled_at <= now())
		FROM scheduled_messages`

	var s Stats
	err := r.db.QueryRow(ctx, query).Scan(
		&s.Pending, &s.Processing, &s.Sent, &s.Failed, &s.Cancelled, &s.DueNow)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// EnrollmentProgress reports how far a contact is through a campaign, used to
// close out enrollments once every step has resolved.
func (r *Repository) EnrollmentProgress(ctx context.Context, q sqlite.Querier, enrollmentID uuid.UUID) (pending, total int, err error) {
	const query = `
		SELECT count(*) FILTER (WHERE status IN ('PENDING', 'PROCESSING')), count(*)
		FROM scheduled_messages WHERE enrollment_id = $1`

	err = r.querier(q).QueryRow(ctx, query, enrollmentID).Scan(&pending, &total)
	return pending, total, err
}

func jobScanDest(job *domain.ScheduledMessage) []any {
	return []any{
		&job.ID, &job.CampaignID, &job.ContactID, &job.EnrollmentID, &job.StepID,
		&job.RunNumber, &job.ScheduledAt, &job.Status, &job.AttemptCount, &job.NextAttemptAt,
		&job.LockedBy, &job.LockedAt, &job.SentAt, &job.CancelledAt, &job.CancelReason,
		&job.LastError, &job.MessageID, &job.CreatedAt, &job.UpdatedAt,
	}
}

func scanJob(row interface{ Scan(...any) error }) (*domain.ScheduledMessage, error) {
	var job domain.ScheduledMessage
	if err := row.Scan(jobScanDest(&job)...); err != nil {
		return nil, err
	}
	return &job, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
