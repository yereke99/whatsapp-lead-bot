// Package templates manages reusable message templates and their revision
// history.
//
// Versioning policy: campaign steps reference a template by id and the content
// is resolved at send time (late binding). Editing a template therefore
// changes every message that has not gone out yet, which is what an operator
// expects when they fix a typo an hour before a webinar. Each edit also writes
// an immutable row to message_template_versions, and every sent message
// records the template id and version it rendered from, so history stays
// accurate even though future sends move forward.
package templates

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

const templateColumns = `
	t.id, t.name, t.description, t.type, t.body, t.media_file_id, t.file_name,
	t.link_preview, t.version, t.archived_at, t.created_by, t.created_at, t.updated_at`

var (
	ErrNameTaken = errors.New("a template with this name already exists")
	ErrNotFound  = errors.New("template not found")
	ErrInUse     = errors.New("template is used by campaign steps")
)

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

func (r *Repository) List(ctx context.Context, search, typeFilter string, includeArchived bool) ([]domain.MessageTemplate, error) {
	args := []any{}
	clauses := []string{}

	if !includeArchived {
		clauses = append(clauses, "t.archived_at IS NULL")
	}
	if s := strings.TrimSpace(search); s != "" {
		args = append(args, "%"+strings.ToLower(s)+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(lower(t.name) LIKE $%d OR lower(t.body) LIKE $%d)", n, n))
	}
	if typeFilter != "" {
		args = append(args, typeFilter)
		clauses = append(clauses, fmt.Sprintf("t.type = $%d", len(args)))
	}

	where := "TRUE"
	if len(clauses) > 0 {
		where = strings.Join(clauses, " AND ")
	}

	query := `SELECT ` + templateColumns + `,
			COALESCE((SELECT count(*) FROM campaign_steps cs WHERE cs.message_template_id = t.id), 0),
			mf.id, mf.original_name, mf.mime_type, mf.size_bytes, mf.kind, mf.duration_ms
		FROM message_templates t
		LEFT JOIN media_files mf ON mf.id = t.media_file_id
		WHERE ` + where + `
		ORDER BY t.updated_at DESC`

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w", err)
	}
	defer rows.Close()

	var out []domain.MessageTemplate
	for rows.Next() {
		tpl, err := scanTemplateWithMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *tpl)
	}
	return out, rows.Err()
}

func (r *Repository) GetByID(ctx context.Context, q sqlite.Querier, id uuid.UUID) (*domain.MessageTemplate, error) {
	query := `SELECT ` + templateColumns + `,
			COALESCE((SELECT count(*) FROM campaign_steps cs WHERE cs.message_template_id = t.id), 0),
			mf.id, mf.original_name, mf.mime_type, mf.size_bytes, mf.kind, mf.duration_ms
		FROM message_templates t
		LEFT JOIN media_files mf ON mf.id = t.media_file_id
		WHERE t.id = $1`

	rows, err := r.querier(q).Query(ctx, query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return scanTemplateWithMedia(rows)
}

// ResolveForSend loads the fields needed to render and send, including the
// media path. It runs on the scheduler's hot path, so it is a single join
// rather than several round trips.
type SendSpec struct {
	TemplateID  uuid.UUID
	Version     int
	Name        string
	Type        domain.TemplateType
	Body        string
	LinkPreview bool
	FileName    string
	MediaID     *uuid.UUID
	MediaPath   string
	MediaMIME   string
	MediaName   string
}

func (r *Repository) ResolveForSend(ctx context.Context, q sqlite.Querier, id uuid.UUID) (*SendSpec, error) {
	const query = `
		SELECT t.id, t.version, t.name, t.type, t.body, t.link_preview, t.file_name,
		       t.media_file_id, COALESCE(mf.relative_path, ''), COALESCE(mf.mime_type, ''),
		       COALESCE(mf.original_name, '')
		FROM message_templates t
		LEFT JOIN media_files mf ON mf.id = t.media_file_id
		WHERE t.id = $1`

	var spec SendSpec
	err := r.querier(q).QueryRow(ctx, query, id).Scan(
		&spec.TemplateID, &spec.Version, &spec.Name, &spec.Type, &spec.Body,
		&spec.LinkPreview, &spec.FileName, &spec.MediaID, &spec.MediaPath,
		&spec.MediaMIME, &spec.MediaName,
	)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &spec, nil
}

func (r *Repository) Create(ctx context.Context, t *domain.MessageTemplate) error {
	return r.db.InTx(ctx, func(tx sqlite.Querier) error {
		const query = `
			INSERT INTO message_templates (name, description, type, body, media_file_id, file_name, link_preview, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			RETURNING id, version, created_at, updated_at`

		err := tx.QueryRow(ctx, query,
			t.Name, t.Description, t.Type, t.Body, t.MediaFileID, t.FileName, t.LinkPreview, t.CreatedBy,
		).Scan(&t.ID, &t.Version, &t.CreatedAt, &t.UpdatedAt)
		if err != nil {
			if sqlite.IsUniqueViolation(err, "message_templates_name_key") {
				return ErrNameTaken
			}
			return fmt.Errorf("insert template: %w", err)
		}

		return insertVersion(ctx, tx, t)
	})
}

// Update writes a new revision. The version counter increments on every save
// so a sent message can always be traced back to exact content.
func (r *Repository) Update(ctx context.Context, t *domain.MessageTemplate) error {
	return r.db.InTx(ctx, func(tx sqlite.Querier) error {
		const query = `
			UPDATE message_templates SET
				name = $2, description = $3, type = $4, body = $5,
				media_file_id = $6, file_name = $7, link_preview = $8,
				version = version + 1
			WHERE id = $1
			RETURNING version, created_at, updated_at`

		err := tx.QueryRow(ctx, query,
			t.ID, t.Name, t.Description, t.Type, t.Body, t.MediaFileID, t.FileName, t.LinkPreview,
		).Scan(&t.Version, &t.CreatedAt, &t.UpdatedAt)
		if err != nil {
			if sqlite.IsNoRows(err) {
				return ErrNotFound
			}
			if sqlite.IsUniqueViolation(err, "message_templates_name_key") {
				return ErrNameTaken
			}
			return fmt.Errorf("update template: %w", err)
		}

		return insertVersion(ctx, tx, t)
	})
}

func insertVersion(ctx context.Context, tx sqlite.Querier, t *domain.MessageTemplate) error {
	const query = `
		INSERT INTO message_template_versions (template_id, version, name, type, body, media_file_id, file_name, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (template_id, version) DO NOTHING`

	_, err := tx.Exec(ctx, query,
		t.ID, t.Version, t.Name, t.Type, t.Body, t.MediaFileID, t.FileName, t.CreatedBy)
	if err != nil {
		return fmt.Errorf("record template version: %w", err)
	}
	return nil
}

// Delete removes a template outright when nothing references it, and archives
// it otherwise so historical steps and sent messages keep resolving.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) (archived bool, err error) {
	var uses int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM campaign_steps WHERE message_template_id = $1`, id).Scan(&uses); err != nil {
		return false, err
	}

	if uses > 0 {
		tag, err := r.db.Exec(ctx,
			`UPDATE message_templates SET archived_at = now() WHERE id = $1 AND archived_at IS NULL`, id)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return true, nil
		}
		return true, nil
	}

	tag, err := r.db.Exec(ctx, `DELETE FROM message_templates WHERE id = $1`, id)
	if err != nil {
		if sqlite.IsForeignKeyViolation(err) {
			return false, ErrInUse
		}
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, ErrNotFound
	}
	return false, nil
}

// Versions returns the revision history, newest first.
func (r *Repository) Versions(ctx context.Context, templateID uuid.UUID, limit int) ([]TemplateVersion, error) {
	const query = `
		SELECT v.version, v.name, v.type, v.body, v.file_name, v.created_at,
		       COALESCE(a.name, a.email, '')
		FROM message_template_versions v
		LEFT JOIN admins a ON a.id = v.created_by
		WHERE v.template_id = $1
		ORDER BY v.version DESC
		LIMIT $2`

	rows, err := r.db.Query(ctx, query, templateID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TemplateVersion
	for rows.Next() {
		var v TemplateVersion
		if err := rows.Scan(&v.Version, &v.Name, &v.Type, &v.Body, &v.FileName, &v.CreatedAt, &v.Author); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// UsageCount reports how many campaign steps reference the template.
func (r *Repository) UsageCount(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM campaign_steps WHERE message_template_id = $1`, id).Scan(&n)
	return n, err
}

func scanTemplateWithMedia(row interface{ Scan(...any) error }) (*domain.MessageTemplate, error) {
	var t domain.MessageTemplate
	var (
		mediaID   *uuid.UUID
		mediaName *string
		mediaMIME *string
		mediaSize *int64
		mediaKind *string
		mediaDur  *int
	)

	err := row.Scan(
		&t.ID, &t.Name, &t.Description, &t.Type, &t.Body, &t.MediaFileID, &t.FileName,
		&t.LinkPreview, &t.Version, &t.ArchivedAt, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt,
		&t.UsedBy,
		&mediaID, &mediaName, &mediaMIME, &mediaSize, &mediaKind, &mediaDur,
	)
	if err != nil {
		return nil, err
	}

	if mediaID != nil {
		t.Media = &domain.MediaFile{
			ID:           *mediaID,
			OriginalName: derefStr(mediaName),
			MimeType:     derefStr(mediaMIME),
			SizeBytes:    derefInt64(mediaSize),
			Kind:         domain.MediaKind(derefStr(mediaKind)),
			DurationMS:   mediaDur,
		}
	}
	return &t, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}
