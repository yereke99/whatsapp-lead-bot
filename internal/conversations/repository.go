// Package conversations stores the message history that backs both the
// contact timeline and the live chat console.
package conversations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

const messageColumns = `
	m.id, m.contact_id, m.campaign_id, m.enrollment_id, m.campaign_step_id,
	m.scheduled_message_id, m.direction, m.type, m.text, m.media_file_id,
	m.media_url, m.file_name, m.mime_type, m.external_id, m.status, m.error,
	m.is_manual, m.sent_by_admin_id, m.template_id, m.template_version,
	m.media_download_status, m.sent_at, m.delivered_at, m.read_at,
	m.created_at, m.updated_at`

// ErrDuplicate reports that a message with the same provider id already exists.
var ErrDuplicate = errors.New("message already recorded")

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

// Create inserts a message.
//
// When the row carries a provider id, a conflict on that id means the same
// message reached us twice (a webhook replay, or a retry that actually
// succeeded the first time). That is reported as ErrDuplicate rather than an
// error, because it is a normal and expected outcome.
func (r *Repository) Create(ctx context.Context, q sqlite.Querier, m *domain.Message) error {
	const query = `
		INSERT INTO messages (
			contact_id, campaign_id, enrollment_id, campaign_step_id, scheduled_message_id,
			direction, type, text, media_file_id, media_url, file_name, mime_type,
			external_id, status, error, is_manual, sent_by_admin_id, template_id,
			template_version, media_download_status, sent_at, delivered_at, read_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23
		)
		RETURNING id, created_at, updated_at`

	downloadState := m.MediaDownloadState
	if downloadState == "" {
		downloadState = "NONE"
	}

	err := r.querier(q).QueryRow(ctx, query,
		m.ContactID, m.CampaignID, m.EnrollmentID, m.CampaignStepID, m.ScheduledMessageID,
		m.Direction, m.Type, m.Text, m.MediaFileID, m.MediaURL, m.FileName, m.MimeType,
		m.ExternalID, m.Status, m.Error, m.IsManual, m.SentByAdminID, m.TemplateID,
		m.TemplateVersion, downloadState, m.SentAt, m.DeliveredAt, m.ReadAt,
	).Scan(&m.ID, &m.CreatedAt, &m.UpdatedAt)

	if err != nil {
		if sqlite.IsUniqueViolation(err, "messages_external_id_key") {
			return ErrDuplicate
		}
		return fmt.Errorf("insert message: %w", err)
	}

	m.MediaDownloadState = downloadState
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.Message, error) {
	query := `SELECT ` + messageColumns + ` FROM messages m WHERE m.id = $1`
	row := r.db.QueryRow(ctx, query, id)

	var m domain.Message
	if err := scanMessage(row, &m); err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

func (r *Repository) ExistsByExternalID(ctx context.Context, q sqlite.Querier, externalID string) (bool, error) {
	var exists bool
	err := r.querier(q).QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE external_id = $1)`, externalID).Scan(&exists)
	return exists, err
}

// Timeline returns a contact's messages oldest-first within a window.
//
// beforeID pages backwards through history for infinite scroll; afterID pulls
// only what is newer, which is how the chat console reconciles after a dropped
// realtime connection.
func (r *Repository) Timeline(ctx context.Context, contactID uuid.UUID, beforeID, afterID *uuid.UUID, limit int) ([]domain.Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	args := []any{contactID}
	clauses := []string{"m.contact_id = $1"}

	if beforeID != nil {
		args = append(args, *beforeID)
		clauses = append(clauses, fmt.Sprintf(
			"m.created_at < (SELECT created_at FROM messages WHERE id = $%d)", len(args)))
	}
	if afterID != nil {
		args = append(args, *afterID)
		clauses = append(clauses, fmt.Sprintf(
			"m.created_at > (SELECT created_at FROM messages WHERE id = $%d)", len(args)))
	}

	// Newest-first with a limit selects the correct window; the caller sees it
	// in chronological order after the reversal below.
	args = append(args, limit)
	query := `SELECT ` + messageColumns + `,
			COALESCE(s.name, ''), COALESCE(a.name, a.email, '')
		FROM messages m
		LEFT JOIN campaign_steps s ON s.id = m.campaign_step_id
		LEFT JOIN admins a ON a.id = m.sent_by_admin_id
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY m.created_at DESC, m.id DESC
		LIMIT $` + fmt.Sprint(len(args))

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load timeline: %w", err)
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		if err := scanMessage(rows, &m, &m.StepName, &m.AdminName); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// MarkSent records a successful send and attaches the provider id.
func (r *Repository) MarkSent(ctx context.Context, q sqlite.Querier, id uuid.UUID, externalID string, sentAt time.Time) error {
	const query = `
		UPDATE messages
		SET status = 'SENT', external_id = $2, sent_at = $3, error = '', updated_at = now()
		WHERE id = $1`

	_, err := r.querier(q).Exec(ctx, query, id, externalID, sentAt)
	if err != nil && sqlite.IsUniqueViolation(err, "messages_external_id_key") {
		return ErrDuplicate
	}
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, q sqlite.Querier, id uuid.UUID, reason string) error {
	const query = `
		UPDATE messages SET status = 'FAILED', error = $2, updated_at = now() WHERE id = $1`
	_, err := r.querier(q).Exec(ctx, query, id, truncate(reason, 2000))
	return err
}

// ApplyStatus advances an outbound message's delivery state.
//
// Providers do not guarantee ordering, so a READ notification can arrive
// before its DELIVERED counterpart. The rank comparison makes the transition
// monotonic and keeps the timeline honest.
func (r *Repository) ApplyStatus(ctx context.Context, q sqlite.Querier, externalID string, status whatsapp.DeliveryStatus, at time.Time, description string) (bool, error) {
	if status == "" {
		return false, nil
	}

	const query = `
		UPDATE messages SET
			status = CASE WHEN $3 = 'FAILED' THEN 'FAILED' ELSE $2 END,
			delivered_at = CASE WHEN $2 IN ('DELIVERED', 'READ') THEN COALESCE(delivered_at, $4) ELSE delivered_at END,
			read_at      = CASE WHEN $2 = 'READ' THEN COALESCE(read_at, $4) ELSE read_at END,
			error        = CASE WHEN $3 = 'FAILED' THEN $5 ELSE error END,
			updated_at   = now()
		WHERE external_id = $1
		  AND direction = 'OUTGOING'
		  AND $6 > CASE status
				WHEN 'SENT' THEN 1
				WHEN 'DELIVERED' THEN 2
				WHEN 'READ' THEN 3
				WHEN 'FAILED' THEN 4
				ELSE 0
			END`

	tag, err := r.querier(q).Exec(ctx, query,
		externalID, string(status), string(status), at,
		truncate(description, 1000), status.Rank())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ContactIDForExternalID finds which conversation a status update belongs to,
// so the realtime layer knows which chat to refresh.
func (r *Repository) ContactIDForExternalID(ctx context.Context, externalID string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := r.db.QueryRow(ctx,
		`SELECT contact_id FROM messages WHERE external_id = $1`, externalID).Scan(&id)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// --------------------------------------------------------- inbound media --

// PendingDownload is an inbound attachment awaiting local capture.
type PendingDownload struct {
	MessageID uuid.UUID
	URL       string
	FileName  string
	MimeType  string
}

func (r *Repository) PendingMediaDownloads(ctx context.Context, limit int) ([]PendingDownload, error) {
	const query = `
		SELECT id, media_url, file_name, mime_type
		FROM messages
		WHERE media_download_status = 'PENDING' AND media_url <> ''
		ORDER BY created_at
		LIMIT $1`

	rows, err := r.db.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingDownload
	for rows.Next() {
		var p PendingDownload
		if err := rows.Scan(&p.MessageID, &p.URL, &p.FileName, &p.MimeType); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) AttachDownloadedMedia(ctx context.Context, messageID, mediaFileID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE messages SET media_file_id = $2, media_download_status = 'DONE',
			media_download_error = '', updated_at = now()
		 WHERE id = $1`, messageID, mediaFileID)
	return err
}

func (r *Repository) MarkDownloadFailed(ctx context.Context, messageID uuid.UUID, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE messages SET media_download_status = 'FAILED', media_download_error = $2, updated_at = now()
		 WHERE id = $1`, messageID, truncate(reason, 500))
	return err
}

// ---------------------------------------------------------------- queries --

// RecentFailures lists outbound messages that ended in a permanent error.
func (r *Repository) RecentFailures(ctx context.Context, limit int) ([]domain.Message, error) {
	query := `SELECT ` + messageColumns + `
		FROM messages m
		WHERE m.direction = 'OUTGOING' AND m.status = 'FAILED'
		ORDER BY m.created_at DESC
		LIMIT $1`

	rows, err := r.db.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Message
	for rows.Next() {
		var m domain.Message
		if err := scanMessage(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// HasIncomingMessages reports whether a contact has ever written to us. This
// is the consent check used before any manual send.
func (r *Repository) HasIncomingMessages(ctx context.Context, contactID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE contact_id = $1 AND direction = 'INCOMING')`,
		contactID).Scan(&exists)
	return exists, err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanMessage(row scanner, m *domain.Message, extra ...any) error {
	dest := []any{
		&m.ID, &m.ContactID, &m.CampaignID, &m.EnrollmentID, &m.CampaignStepID,
		&m.ScheduledMessageID, &m.Direction, &m.Type, &m.Text, &m.MediaFileID,
		&m.MediaURL, &m.FileName, &m.MimeType, &m.ExternalID, &m.Status, &m.Error,
		&m.IsManual, &m.SentByAdminID, &m.TemplateID, &m.TemplateVersion,
		&m.MediaDownloadState, &m.SentAt, &m.DeliveredAt, &m.ReadAt,
		&m.CreatedAt, &m.UpdatedAt,
	}
	dest = append(dest, extra...)
	return row.Scan(dest...)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
