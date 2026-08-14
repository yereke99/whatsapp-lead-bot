package media

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/postgres"
)

const mediaColumns = `
	id, original_name, stored_name, relative_path, mime_type, size_bytes, kind,
	checksum_sha256, duration_ms, width, height, source_media_file_id,
	uploaded_by, created_at`

type Repository struct {
	db *postgres.DB
}

func NewRepository(db *postgres.DB) *Repository { return &Repository{db: db} }

func (r *Repository) Create(ctx context.Context, q postgres.Querier, m *domain.MediaFile) error {
	if q == nil {
		q = r.db.Pool
	}

	const query = `
		INSERT INTO media_files (
			original_name, stored_name, relative_path, mime_type, size_bytes, kind,
			checksum_sha256, duration_ms, width, height, source_media_file_id, uploaded_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id, created_at`

	err := q.QueryRow(ctx, query,
		m.OriginalName, m.StoredName, m.RelativePath, m.MimeType, m.SizeBytes, m.Kind,
		m.Checksum, m.DurationMS, m.Width, m.Height, m.SourceFileID, m.UploadedBy,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert media file: %w", err)
	}
	return nil
}

func (r *Repository) GetByID(ctx context.Context, id uuid.UUID) (*domain.MediaFile, error) {
	query := `SELECT ` + mediaColumns + ` FROM media_files WHERE id = $1`
	return scanMedia(r.db.Pool.QueryRow(ctx, query, id))
}

func (r *Repository) List(ctx context.Context, kind string, limit, offset int) ([]domain.MediaFile, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	countQuery := `SELECT count(*) FROM media_files WHERE ($1 = '' OR kind = $1)`
	var total int
	if err := r.db.Pool.QueryRow(ctx, countQuery, kind).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count media: %w", err)
	}

	query := `SELECT ` + mediaColumns + `
		FROM media_files
		WHERE ($1 = '' OR kind = $1)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`

	rows, err := r.db.Pool.Query(ctx, query, kind, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list media: %w", err)
	}
	defer rows.Close()

	var out []domain.MediaFile
	for rows.Next() {
		m, err := scanMediaRow(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *m)
	}
	return out, total, rows.Err()
}

// Delete removes the row. Templates reference media with ON DELETE RESTRICT,
// so a file still in use reports a foreign key violation instead of silently
// breaking a scheduled send.
func (r *Repository) Delete(ctx context.Context, id uuid.UUID) (string, error) {
	var relPath string
	err := r.db.Pool.QueryRow(ctx,
		`DELETE FROM media_files WHERE id = $1 RETURNING relative_path`, id).Scan(&relPath)
	if err != nil {
		return "", err
	}
	return relPath, nil
}

// InUse reports how many templates reference the file.
func (r *Repository) InUse(ctx context.Context, id uuid.UUID) (int, error) {
	var count int
	err := r.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM message_templates WHERE media_file_id = $1 AND archived_at IS NULL`,
		id).Scan(&count)
	return count, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMedia(row pgx.Row) (*domain.MediaFile, error) {
	m, err := scanMediaRow(row)
	if err != nil {
		if postgres.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

func scanMediaRow(row rowScanner) (*domain.MediaFile, error) {
	var m domain.MediaFile
	err := row.Scan(
		&m.ID, &m.OriginalName, &m.StoredName, &m.RelativePath, &m.MimeType,
		&m.SizeBytes, &m.Kind, &m.Checksum, &m.DurationMS, &m.Width, &m.Height,
		&m.SourceFileID, &m.UploadedBy, &m.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &m, nil
}
