// Package campaigns owns campaign configuration, trigger routing, automation
// steps and contact enrollment.
package campaigns

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

var (
	ErrNotFound       = errors.New("campaign not found")
	ErrNameTaken      = errors.New("a campaign with this name already exists")
	ErrTriggerTaken   = errors.New("this trigger is already used by another campaign")
	ErrStepNotFound   = errors.New("campaign step not found")
	ErrNoEventStart   = errors.New("campaign has no event start time")
	ErrNoEnabledSteps = errors.New("campaign has no enabled steps")
)

const campaignColumns = `
	c.id, c.name, c.description, c.event_type, c.event_start_at, c.timezone,
	c.webinar_link, c.status, c.existing_contact_behavior, c.existing_contact_template_id,
	c.unsubscribe_keywords, c.catch_up_missed_steps, c.max_send_attempts,
	c.archived_at, c.created_by, c.created_at, c.updated_at`

const stepColumns = `
	s.id, s.campaign_id, s.name, s.offset_seconds, s.message_template_id,
	s.enabled, s.order_index, s.schedule_kind, s.created_at, s.updated_at`

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

// DB exposes the pool for service-level transactions that span repositories.
func (r *Repository) DB() *sqlite.DB { return r.db }

// ------------------------------------------------------------- campaigns --

type ListFilter struct {
	Search          string
	Status          string
	IncludeArchived bool
}

func (r *Repository) List(ctx context.Context, f ListFilter) ([]domain.Campaign, error) {
	var args []any
	var clauses []string

	if !f.IncludeArchived {
		clauses = append(clauses, "c.archived_at IS NULL")
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		args = append(args, "%"+strings.ToLower(s)+"%")
		clauses = append(clauses, fmt.Sprintf("lower(c.name) LIKE $%d", len(args)))
	}
	if f.Status != "" {
		args = append(args, f.Status)
		clauses = append(clauses, fmt.Sprintf("c.status = $%d", len(args)))
	}

	where := "TRUE"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}

	query := `SELECT ` + campaignColumns + `,
			COALESCE((SELECT count(*) FROM campaign_contacts cc WHERE cc.campaign_id = c.id), 0),
			COALESCE((SELECT count(*) FROM scheduled_messages sm WHERE sm.campaign_id = c.id AND sm.status = 'PENDING'), 0),
			COALESCE((SELECT count(*) FROM scheduled_messages sm WHERE sm.campaign_id = c.id AND sm.status = 'SENT'), 0)
		FROM campaigns c
		WHERE ` + where + `
		ORDER BY
			CASE c.status WHEN 'ACTIVE' THEN 0 WHEN 'PAUSED' THEN 1 WHEN 'DRAFT' THEN 2 ELSE 3 END,
			c.event_start_at DESC NULLS LAST,
			c.created_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list campaigns: %w", err)
	}
	defer rows.Close()

	var out []domain.Campaign
	for rows.Next() {
		var c domain.Campaign
		dest := campaignScanDest(&c)
		dest = append(dest, &c.ContactCount, &c.PendingJobs, &c.SentCount)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Triggers are shown inline in the campaign list, so they are fetched in
	// one extra query rather than N.
	if err := r.attachTriggers(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) attachTriggers(ctx context.Context, list []domain.Campaign) error {
	if len(list) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, 0, len(list))
	index := make(map[uuid.UUID]int, len(list))
	for i, c := range list {
		ids = append(ids, c.ID)
		index[c.ID] = i
	}

	const query = `
		SELECT id, campaign_id, keyword, normalized_keyword, match_mode, is_active, created_at, updated_at
		FROM campaign_triggers
		WHERE campaign_id IN (SELECT value FROM json_each($1))
		ORDER BY created_at`

	rows, err := r.db.Query(ctx, query, ids)
	if err != nil {
		return fmt.Errorf("load triggers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var t domain.CampaignTrigger
		if err := rows.Scan(&t.ID, &t.CampaignID, &t.Keyword, &t.Normalized,
			&t.MatchMode, &t.IsActive, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return err
		}
		if i, ok := index[t.CampaignID]; ok {
			list[i].Triggers = append(list[i].Triggers, t)
		}
	}
	return rows.Err()
}

func (r *Repository) GetByID(ctx context.Context, q sqlite.Querier, id uuid.UUID) (*domain.Campaign, error) {
	query := `SELECT ` + campaignColumns + `,
			COALESCE((SELECT count(*) FROM campaign_contacts cc WHERE cc.campaign_id = c.id), 0),
			COALESCE((SELECT count(*) FROM scheduled_messages sm WHERE sm.campaign_id = c.id AND sm.status = 'PENDING'), 0),
			COALESCE((SELECT count(*) FROM scheduled_messages sm WHERE sm.campaign_id = c.id AND sm.status = 'SENT'), 0)
		FROM campaigns c WHERE c.id = $1`

	var c domain.Campaign
	dest := campaignScanDest(&c)
	dest = append(dest, &c.ContactCount, &c.PendingJobs, &c.SentCount)

	if err := r.querier(q).QueryRow(ctx, query, id).Scan(dest...); err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get campaign: %w", err)
	}
	return &c, nil
}

// GetFull loads a campaign with its triggers and ordered steps.
func (r *Repository) GetFull(ctx context.Context, id uuid.UUID) (*domain.Campaign, error) {
	campaign, err := r.GetByID(ctx, nil, id)
	if err != nil || campaign == nil {
		return nil, err
	}

	list := []domain.Campaign{*campaign}
	if err := r.attachTriggers(ctx, list); err != nil {
		return nil, err
	}
	campaign.Triggers = list[0].Triggers

	steps, err := r.ListSteps(ctx, nil, id)
	if err != nil {
		return nil, err
	}
	campaign.Steps = steps
	return campaign, nil
}

func (r *Repository) Create(ctx context.Context, q sqlite.Querier, c *domain.Campaign) error {
	const query = `
		INSERT INTO campaigns (
			name, description, event_type, event_start_at, timezone, webinar_link,
			status, existing_contact_behavior, existing_contact_template_id,
			unsubscribe_keywords, catch_up_missed_steps, max_send_attempts, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id, created_at, updated_at`

	err := r.querier(q).QueryRow(ctx, query,
		c.Name, c.Description, c.EventType, c.EventStartAt, c.Timezone, c.WebinarLink,
		c.Status, c.ExistingContactBehavior, c.ExistingContactTemplate,
		c.UnsubscribeKeywords, c.CatchUpMissedSteps, c.MaxSendAttempts, c.CreatedBy,
	).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if sqlite.IsUniqueViolation(err, "campaigns_name_key") {
			return ErrNameTaken
		}
		return fmt.Errorf("insert campaign: %w", err)
	}
	return nil
}

func (r *Repository) Update(ctx context.Context, q sqlite.Querier, c *domain.Campaign) error {
	const query = `
		UPDATE campaigns SET
			name = $2, description = $3, event_type = $4, event_start_at = $5,
			timezone = $6, webinar_link = $7, existing_contact_behavior = $8,
			existing_contact_template_id = $9, unsubscribe_keywords = $10,
			catch_up_missed_steps = $11, max_send_attempts = $12
		WHERE id = $1
		RETURNING updated_at`

	err := r.querier(q).QueryRow(ctx, query,
		c.ID, c.Name, c.Description, c.EventType, c.EventStartAt, c.Timezone,
		c.WebinarLink, c.ExistingContactBehavior, c.ExistingContactTemplate,
		c.UnsubscribeKeywords, c.CatchUpMissedSteps, c.MaxSendAttempts,
	).Scan(&c.UpdatedAt)

	if err != nil {
		if sqlite.IsNoRows(err) {
			return ErrNotFound
		}
		if sqlite.IsUniqueViolation(err, "campaigns_name_key") {
			return ErrNameTaken
		}
		return fmt.Errorf("update campaign: %w", err)
	}
	return nil
}

func (r *Repository) SetStatus(ctx context.Context, q sqlite.Querier, id uuid.UUID, status domain.CampaignStatus) error {
	tag, err := r.querier(q).Exec(ctx,
		`UPDATE campaigns SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return fmt.Errorf("set campaign status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Archive hides a campaign and deactivates its triggers so the keyword becomes
// available to another campaign.
func (r *Repository) Archive(ctx context.Context, id uuid.UUID) error {
	return r.db.InTx(ctx, func(tx sqlite.Querier) error {
		if _, err := tx.Exec(ctx,
			`UPDATE campaigns SET status = 'ARCHIVED', archived_at = now() WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE campaign_triggers SET is_active = false WHERE campaign_id = $1`, id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE scheduled_messages SET status = 'CANCELLED', cancelled_at = now(),
				cancel_reason = 'campaign archived', updated_at = now()
			 WHERE campaign_id = $1 AND status = 'PENDING'`, id)
		return err
	})
}

func (r *Repository) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM campaigns WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// -------------------------------------------------------------- triggers --

// TriggerMatch is an active trigger considered during routing.
type TriggerMatch struct {
	TriggerID    uuid.UUID
	CampaignID   uuid.UUID
	CampaignName string
	Keyword      string
	Normalized   string
	MatchMode    string
}

// ActiveTriggers returns every trigger belonging to a campaign that is
// currently accepting enrollments.
//
// Ordering is deterministic: exact matches first, then longer keywords before
// shorter ones, so an ambiguous message always resolves the same way.
func (r *Repository) ActiveTriggers(ctx context.Context, q sqlite.Querier) ([]TriggerMatch, error) {
	const query = `
		SELECT t.id, t.campaign_id, c.name, t.keyword, t.normalized_keyword, t.match_mode
		FROM campaign_triggers t
		JOIN campaigns c ON c.id = t.campaign_id
		WHERE t.is_active
		  AND c.status = 'ACTIVE'
		  AND c.archived_at IS NULL
		ORDER BY
			CASE t.match_mode WHEN 'EXACT' THEN 0 WHEN 'STARTS_WITH' THEN 1 ELSE 2 END,
			length(t.normalized_keyword) DESC,
			t.created_at`

	rows, err := r.querier(q).Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("load active triggers: %w", err)
	}
	defer rows.Close()

	var out []TriggerMatch
	for rows.Next() {
		var t TriggerMatch
		if err := rows.Scan(&t.TriggerID, &t.CampaignID, &t.CampaignName,
			&t.Keyword, &t.Normalized, &t.MatchMode); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) ListTriggers(ctx context.Context, campaignID uuid.UUID) ([]domain.CampaignTrigger, error) {
	const query = `
		SELECT id, campaign_id, keyword, normalized_keyword, match_mode, is_active, created_at, updated_at
		FROM campaign_triggers WHERE campaign_id = $1 ORDER BY created_at`

	rows, err := r.db.Query(ctx, query, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.CampaignTrigger
	for rows.Next() {
		var t domain.CampaignTrigger
		if err := rows.Scan(&t.ID, &t.CampaignID, &t.Keyword, &t.Normalized,
			&t.MatchMode, &t.IsActive, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AllTriggers lists triggers across campaigns for the trigger admin page.
func (r *Repository) AllTriggers(ctx context.Context) ([]domain.CampaignTrigger, error) {
	const query = `
		SELECT t.id, t.campaign_id, t.keyword, t.normalized_keyword, t.match_mode,
		       t.is_active, t.created_at, t.updated_at, c.name
		FROM campaign_triggers t
		JOIN campaigns c ON c.id = t.campaign_id
		WHERE c.archived_at IS NULL
		ORDER BY c.name, t.created_at`

	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.CampaignTrigger
	for rows.Next() {
		var t domain.CampaignTrigger
		if err := rows.Scan(&t.ID, &t.CampaignID, &t.Keyword, &t.Normalized,
			&t.MatchMode, &t.IsActive, &t.CreatedAt, &t.UpdatedAt, &t.CampaignRef); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) CreateTrigger(ctx context.Context, q sqlite.Querier, t *domain.CampaignTrigger) error {
	const query = `
		INSERT INTO campaign_triggers (campaign_id, keyword, normalized_keyword, match_mode, is_active)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, created_at, updated_at`

	err := r.querier(q).QueryRow(ctx, query,
		t.CampaignID, t.Keyword, t.Normalized, t.MatchMode, t.IsActive,
	).Scan(&t.ID, &t.CreatedAt, &t.UpdatedAt)

	if err != nil {
		if sqlite.IsUniqueViolation(err, "campaign_triggers_unique_active") {
			return ErrTriggerTaken
		}
		return fmt.Errorf("insert trigger: %w", err)
	}
	return nil
}

func (r *Repository) UpdateTrigger(ctx context.Context, t *domain.CampaignTrigger) error {
	const query = `
		UPDATE campaign_triggers
		SET keyword = $2, normalized_keyword = $3, match_mode = $4, is_active = $5
		WHERE id = $1
		RETURNING campaign_id, created_at, updated_at`

	err := r.db.QueryRow(ctx, query,
		t.ID, t.Keyword, t.Normalized, t.MatchMode, t.IsActive,
	).Scan(&t.CampaignID, &t.CreatedAt, &t.UpdatedAt)

	if err != nil {
		if sqlite.IsNoRows(err) {
			return ErrNotFound
		}
		if sqlite.IsUniqueViolation(err, "campaign_triggers_unique_active") {
			return ErrTriggerTaken
		}
		return fmt.Errorf("update trigger: %w", err)
	}
	return nil
}

func (r *Repository) DeleteTrigger(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM campaign_triggers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ----------------------------------------------------------------- steps --

func (r *Repository) ListSteps(ctx context.Context, q sqlite.Querier, campaignID uuid.UUID) ([]domain.CampaignStep, error) {
	query := `SELECT ` + stepColumns + `, t.name, t.type,
			CASE WHEN length(t.body) > 120 THEN substr(t.body, 1, 120) || '…' ELSE t.body END
		FROM campaign_steps s
		JOIN message_templates t ON t.id = s.message_template_id
		WHERE s.campaign_id = $1
		ORDER BY s.order_index`

	rows, err := r.querier(q).Query(ctx, query, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}
	defer rows.Close()

	var out []domain.CampaignStep
	for rows.Next() {
		var s domain.CampaignStep
		if err := rows.Scan(&s.ID, &s.CampaignID, &s.Name, &s.OffsetSeconds,
			&s.TemplateID, &s.Enabled, &s.OrderIndex, &s.ScheduleKind,
			&s.CreatedAt, &s.UpdatedAt,
			&s.TemplateName, &s.TemplateType, &s.TemplatePreview); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) GetStep(ctx context.Context, id uuid.UUID) (*domain.CampaignStep, error) {
	query := `SELECT ` + stepColumns + ` FROM campaign_steps s WHERE s.id = $1`

	var s domain.CampaignStep
	err := r.db.QueryRow(ctx, query, id).Scan(
		&s.ID, &s.CampaignID, &s.Name, &s.OffsetSeconds, &s.TemplateID,
		&s.Enabled, &s.OrderIndex, &s.ScheduleKind, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &s, nil
}

func (r *Repository) CreateStep(ctx context.Context, q sqlite.Querier, s *domain.CampaignStep) error {
	querier := r.querier(q)

	if s.OrderIndex <= 0 {
		if err := querier.QueryRow(ctx,
			`SELECT COALESCE(max(order_index), 0) + 1 FROM campaign_steps WHERE campaign_id = $1`,
			s.CampaignID).Scan(&s.OrderIndex); err != nil {
			return fmt.Errorf("next step order: %w", err)
		}
	}

	const query = `
		INSERT INTO campaign_steps (campaign_id, name, offset_seconds, message_template_id, enabled, order_index, schedule_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, created_at, updated_at`

	err := querier.QueryRow(ctx, query,
		s.CampaignID, s.Name, s.OffsetSeconds, s.TemplateID, s.Enabled, s.OrderIndex, s.ScheduleKind,
	).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert step: %w", err)
	}
	return nil
}

func (r *Repository) UpdateStep(ctx context.Context, q sqlite.Querier, s *domain.CampaignStep) error {
	const query = `
		UPDATE campaign_steps SET
			name = $2, offset_seconds = $3, message_template_id = $4,
			enabled = $5, schedule_kind = $6
		WHERE id = $1
		RETURNING campaign_id, order_index, created_at, updated_at`

	err := r.querier(q).QueryRow(ctx, query,
		s.ID, s.Name, s.OffsetSeconds, s.TemplateID, s.Enabled, s.ScheduleKind,
	).Scan(&s.CampaignID, &s.OrderIndex, &s.CreatedAt, &s.UpdatedAt)

	if err != nil {
		if sqlite.IsNoRows(err) {
			return ErrStepNotFound
		}
		return fmt.Errorf("update step: %w", err)
	}
	return nil
}

func (r *Repository) DeleteStep(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM campaign_steps WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrStepNotFound
	}
	return nil
}

// ReorderSteps rewrites order_index for a campaign in one transaction.
//
// SQLite cannot defer a unique constraint to commit time, so the shuffle may
// never pass through a state where two steps share an index. Renumbering
// happens in two phases instead: every step is first parked in the negative
// range, which no valid arrangement uses, and then given its final position.
// Any step left negative at the end was missing from orderedIDs, which would
// otherwise leave the campaign with a hole in its ordering.
func (r *Repository) ReorderSteps(ctx context.Context, campaignID uuid.UUID, orderedIDs []uuid.UUID) error {
	return r.db.InTx(ctx, func(tx sqlite.Querier) error {
		if _, err := tx.Exec(ctx,
			`UPDATE campaign_steps SET order_index = -order_index
			 WHERE campaign_id = $1 AND order_index > 0`, campaignID); err != nil {
			return fmt.Errorf("stage step reorder: %w", err)
		}

		for i, id := range orderedIDs {
			tag, err := tx.Exec(ctx,
				`UPDATE campaign_steps SET order_index = $3 WHERE id = $1 AND campaign_id = $2`,
				id, campaignID, i+1)
			if err != nil {
				return fmt.Errorf("reorder step %s: %w", id, err)
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("%w: %s", ErrStepNotFound, id)
			}
		}

		var unplaced int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM campaign_steps WHERE campaign_id = $1 AND order_index < 0`,
			campaignID).Scan(&unplaced); err != nil {
			return fmt.Errorf("verify step reorder: %w", err)
		}
		if unplaced > 0 {
			return fmt.Errorf("reorder must list every step: %d missing", unplaced)
		}
		return nil
	})
}

// ----------------------------------------------------------- enrollments --

func (r *Repository) FindEnrollment(ctx context.Context, q sqlite.Querier, campaignID, contactID uuid.UUID) (*domain.Enrollment, error) {
	const query = `
		SELECT id, campaign_id, contact_id, trigger_id, trigger_keyword, status,
		       run_number, restart_count, enrolled_at, completed_at, cancelled_at,
		       cancel_reason, created_at, updated_at
		FROM campaign_contacts
		WHERE campaign_id = $1 AND contact_id = $2`

	var e domain.Enrollment
	err := r.querier(q).QueryRow(ctx, query, campaignID, contactID).Scan(
		&e.ID, &e.CampaignID, &e.ContactID, &e.TriggerID, &e.TriggerKeyword, &e.Status,
		&e.RunNumber, &e.RestartCount, &e.EnrolledAt, &e.CompletedAt, &e.CancelledAt,
		&e.CancelReason, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("find enrollment: %w", err)
	}
	return &e, nil
}

func (r *Repository) CreateEnrollment(ctx context.Context, q sqlite.Querier, e *domain.Enrollment) error {
	const query = `
		INSERT INTO campaign_contacts (campaign_id, contact_id, trigger_id, trigger_keyword, status, run_number)
		VALUES ($1,$2,$3,$4,'ACTIVE',1)
		ON CONFLICT (campaign_id, contact_id) DO NOTHING
		RETURNING id, status, run_number, restart_count, enrolled_at, created_at, updated_at`

	err := r.querier(q).QueryRow(ctx, query,
		e.CampaignID, e.ContactID, e.TriggerID, e.TriggerKeyword,
	).Scan(&e.ID, &e.Status, &e.RunNumber, &e.RestartCount, &e.EnrolledAt, &e.CreatedAt, &e.UpdatedAt)

	if err != nil {
		if sqlite.IsNoRows(err) {
			// Another delivery of the same trigger won the race; the caller
			// re-reads the existing row.
			return ErrAlreadyEnrolled
		}
		return fmt.Errorf("create enrollment: %w", err)
	}
	return nil
}

// ErrAlreadyEnrolled reports a concurrent enrollment for the same pair.
var ErrAlreadyEnrolled = errors.New("contact is already enrolled in this campaign")

// RestartEnrollment bumps the run counter so a fresh set of jobs can be
// created without colliding with the previous run's unique keys.
func (r *Repository) RestartEnrollment(ctx context.Context, q sqlite.Querier, enrollmentID uuid.UUID, keyword string) (*domain.Enrollment, error) {
	const query = `
		UPDATE campaign_contacts SET
			run_number    = run_number + 1,
			restart_count = restart_count + 1,
			status        = 'ACTIVE',
			trigger_keyword = $2,
			enrolled_at   = now(),
			completed_at  = NULL,
			cancelled_at  = NULL,
			cancel_reason = ''
		WHERE id = $1
		RETURNING id, campaign_id, contact_id, trigger_id, trigger_keyword, status,
		          run_number, restart_count, enrolled_at, completed_at, cancelled_at,
		          cancel_reason, created_at, updated_at`

	var e domain.Enrollment
	err := r.querier(q).QueryRow(ctx, query, enrollmentID, keyword).Scan(
		&e.ID, &e.CampaignID, &e.ContactID, &e.TriggerID, &e.TriggerKeyword, &e.Status,
		&e.RunNumber, &e.RestartCount, &e.EnrolledAt, &e.CompletedAt, &e.CancelledAt,
		&e.CancelReason, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("restart enrollment: %w", err)
	}
	return &e, nil
}

func (r *Repository) SetEnrollmentStatus(ctx context.Context, q sqlite.Querier, id uuid.UUID, status domain.EnrollmentStatus, reason string) error {
	const query = `
		UPDATE campaign_contacts SET
			status       = $2,
			completed_at = CASE WHEN $2 = 'COMPLETED' THEN COALESCE(completed_at, now()) ELSE completed_at END,
			cancelled_at = CASE WHEN $2 IN ('CANCELLED','UNSUBSCRIBED') THEN COALESCE(cancelled_at, now()) ELSE cancelled_at END,
			cancel_reason = CASE WHEN $3 <> '' THEN $3 ELSE cancel_reason END
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, id, status, reason)
	return err
}

// StopAllForContact ends every active enrollment, used when a contact
// unsubscribes or is blocked.
func (r *Repository) StopAllForContact(ctx context.Context, q sqlite.Querier, contactID uuid.UUID, status domain.EnrollmentStatus, reason string) error {
	const query = `
		UPDATE campaign_contacts SET
			status = $2, cancelled_at = COALESCE(cancelled_at, now()), cancel_reason = $3
		WHERE contact_id = $1 AND status = 'ACTIVE'`

	_, err := r.querier(q).Exec(ctx, query, contactID, status, reason)
	return err
}

func (r *Repository) ListEnrollmentsForContact(ctx context.Context, contactID uuid.UUID) ([]domain.Enrollment, error) {
	const query = `
		SELECT cc.id, cc.campaign_id, cc.contact_id, cc.trigger_id, cc.trigger_keyword,
		       cc.status, cc.run_number, cc.restart_count, cc.enrolled_at, cc.completed_at,
		       cc.cancelled_at, cc.cancel_reason, cc.created_at, cc.updated_at, c.name,
		       COALESCE((SELECT count(*) FROM scheduled_messages sm
		                 WHERE sm.enrollment_id = cc.id AND sm.status = 'PENDING'), 0),
		       COALESCE((SELECT count(*) FROM scheduled_messages sm
		                 WHERE sm.enrollment_id = cc.id AND sm.status = 'SENT'), 0)
		FROM campaign_contacts cc
		JOIN campaigns c ON c.id = cc.campaign_id
		WHERE cc.contact_id = $1
		ORDER BY cc.enrolled_at DESC`

	rows, err := r.db.Query(ctx, query, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Enrollment
	for rows.Next() {
		var e domain.Enrollment
		if err := rows.Scan(&e.ID, &e.CampaignID, &e.ContactID, &e.TriggerID,
			&e.TriggerKeyword, &e.Status, &e.RunNumber, &e.RestartCount, &e.EnrolledAt,
			&e.CompletedAt, &e.CancelledAt, &e.CancelReason, &e.CreatedAt, &e.UpdatedAt,
			&e.CampaignName, &e.PendingJobs, &e.SentJobs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ActiveEnrollmentIDs lists enrollments still running in a campaign, used when
// a newly enabled step must be back-filled.
func (r *Repository) ActiveEnrollmentIDs(ctx context.Context, q sqlite.Querier, campaignID uuid.UUID) ([]domain.Enrollment, error) {
	const query = `
		SELECT cc.id, cc.contact_id, cc.run_number, cc.enrolled_at
		FROM campaign_contacts cc
		JOIN contacts c ON c.id = cc.contact_id
		WHERE cc.campaign_id = $1
		  AND cc.status = 'ACTIVE'
		  AND NOT c.opted_out
		  AND c.blocked_at IS NULL`

	rows, err := r.querier(q).Query(ctx, query, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Enrollment
	for rows.Next() {
		var e domain.Enrollment
		e.CampaignID = campaignID
		if err := rows.Scan(&e.ID, &e.ContactID, &e.RunNumber, &e.EnrolledAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func campaignScanDest(c *domain.Campaign) []any {
	return []any{
		&c.ID, &c.Name, &c.Description, &c.EventType, &c.EventStartAt, &c.Timezone,
		&c.WebinarLink, &c.Status, &c.ExistingContactBehavior, &c.ExistingContactTemplate,
		&c.UnsubscribeKeywords, &c.CatchUpMissedSteps, &c.MaxSendAttempts,
		&c.ArchivedAt, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt,
	}
}

// UnsubscribeKeywordsFor returns every configured stop word, normalized by the
// caller, across the campaigns a contact belongs to plus the global defaults.
func (r *Repository) UnsubscribeKeywordsFor(ctx context.Context, q sqlite.Querier, contactID uuid.UUID) ([]string, error) {
	// unsubscribe_keywords is a JSON array; json_each expands it into rows the
	// same way unnest did.
	const query = `
		SELECT DISTINCT keyword.value
		FROM campaigns c, json_each(c.unsubscribe_keywords) AS keyword
		WHERE c.archived_at IS NULL
		  AND (
			c.status = 'ACTIVE'
			OR EXISTS (SELECT 1 FROM campaign_contacts cc
			           WHERE cc.campaign_id = c.id AND cc.contact_id = $1)
		  )`

	rows, err := r.querier(q).Query(ctx, query, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			return nil, err
		}
		out = append(out, kw)
	}
	return out, rows.Err()
}

// Touch updates a campaign's timestamp without changing content, so the list
// ordering reflects recent administrative activity.
func (r *Repository) Touch(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE campaigns SET updated_at = now() WHERE id = $1`, id)
	return err
}
