package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
)

type Repository struct {
	db *sqlite.DB
}

func NewRepository(db *sqlite.DB) *Repository { return &Repository{db: db} }

// ------------------------------------------------------------ admin users --

func (r *Repository) FindByEmail(ctx context.Context, email string) (*domain.Admin, error) {
	const query = `
		SELECT id, email, name, password_hash, role, is_active, last_login_at, created_at, updated_at
		FROM admins WHERE lower(email) = lower($1)`

	var a domain.Admin
	err := r.db.QueryRow(ctx, query, strings.TrimSpace(email)).Scan(
		&a.ID, &a.Email, &a.Name, &a.PasswordHash, &a.Role, &a.IsActive,
		&a.LastLoginAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("find admin: %w", err)
	}
	return &a, nil
}

func (r *Repository) FindByID(ctx context.Context, id uuid.UUID) (*domain.Admin, error) {
	const query = `
		SELECT id, email, name, password_hash, role, is_active, last_login_at, created_at, updated_at
		FROM admins WHERE id = $1`

	var a domain.Admin
	err := r.db.QueryRow(ctx, query, id).Scan(
		&a.ID, &a.Email, &a.Name, &a.PasswordHash, &a.Role, &a.IsActive,
		&a.LastLoginAt, &a.CreatedAt, &a.UpdatedAt)
	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (r *Repository) ListAdmins(ctx context.Context) ([]domain.Admin, error) {
	const query = `
		SELECT id, email, name, '', role, is_active, last_login_at, created_at, updated_at
		FROM admins ORDER BY created_at`

	rows, err := r.db.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Admin
	for rows.Next() {
		var a domain.Admin
		if err := rows.Scan(&a.ID, &a.Email, &a.Name, &a.PasswordHash, &a.Role,
			&a.IsActive, &a.LastLoginAt, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) CreateAdmin(ctx context.Context, a *domain.Admin) error {
	const query = `
		INSERT INTO admins (email, name, password_hash, role, is_active)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, created_at, updated_at`

	err := r.db.QueryRow(ctx, query,
		strings.TrimSpace(a.Email), strings.TrimSpace(a.Name), a.PasswordHash, a.Role, a.IsActive,
	).Scan(&a.ID, &a.CreatedAt, &a.UpdatedAt)

	if err != nil {
		if sqlite.IsUniqueViolation(err, "admins_email_key") {
			return ErrEmailTaken
		}
		return fmt.Errorf("create admin: %w", err)
	}
	return nil
}

func (r *Repository) UpdatePassword(ctx context.Context, adminID uuid.UUID, hash string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE admins SET password_hash = $2 WHERE id = $1`, adminID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAdminNotFound
	}
	return nil
}

func (r *Repository) UpdateAdmin(ctx context.Context, adminID uuid.UUID, name, role string, active bool) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE admins SET name = $2, role = $3, is_active = $4 WHERE id = $1`,
		adminID, name, role, active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAdminNotFound
	}
	return nil
}

func (r *Repository) DeleteAdmin(ctx context.Context, adminID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM admins WHERE id = $1`, adminID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAdminNotFound
	}
	return nil
}

func (r *Repository) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRow(ctx, `SELECT count(*) FROM admins WHERE is_active`).Scan(&n)
	return n, err
}

func (r *Repository) RecordLogin(ctx context.Context, adminID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admins SET last_login_at = now() WHERE id = $1`, adminID)
	return err
}

// -------------------------------------------------------------- sessions --

// hashToken stores only a digest of the session token. A database leak
// therefore does not hand an attacker usable session cookies.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *Repository) CreateSession(ctx context.Context, s *domain.Session, token string) error {
	const query = `
		INSERT INTO admin_sessions (admin_id, token_hash, csrf_token, ip_address, user_agent, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, created_at, last_seen_at`

	err := r.db.QueryRow(ctx, query,
		s.AdminID, hashToken(token), s.CSRFToken, s.IPAddress, s.UserAgent, s.ExpiresAt,
	).Scan(&s.ID, &s.CreatedAt, &s.LastSeenAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	s.TokenHash = hashToken(token)
	return nil
}

// SessionWithAdmin is the joined result used on every authenticated request.
type SessionWithAdmin struct {
	Session domain.Session
	Admin   domain.Admin
}

func (r *Repository) FindSession(ctx context.Context, token string) (*SessionWithAdmin, error) {
	const query = `
		SELECT s.id, s.admin_id, s.csrf_token, s.ip_address, s.user_agent,
		       s.expires_at, s.last_seen_at, s.revoked_at, s.created_at,
		       a.id, a.email, a.name, a.role, a.is_active, a.last_login_at, a.created_at, a.updated_at
		FROM admin_sessions s
		JOIN admins a ON a.id = s.admin_id
		WHERE s.token_hash = $1`

	var out SessionWithAdmin
	err := r.db.QueryRow(ctx, query, hashToken(token)).Scan(
		&out.Session.ID, &out.Session.AdminID, &out.Session.CSRFToken,
		&out.Session.IPAddress, &out.Session.UserAgent, &out.Session.ExpiresAt,
		&out.Session.LastSeenAt, &out.Session.RevokedAt, &out.Session.CreatedAt,
		&out.Admin.ID, &out.Admin.Email, &out.Admin.Name, &out.Admin.Role,
		&out.Admin.IsActive, &out.Admin.LastLoginAt, &out.Admin.CreatedAt, &out.Admin.UpdatedAt)

	if err != nil {
		if sqlite.IsNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("find session: %w", err)
	}
	return &out, nil
}

// TouchSession records activity and slides the expiry forward.
func (r *Repository) TouchSession(ctx context.Context, sessionID uuid.UUID, expiresAt time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admin_sessions SET last_seen_at = now(), expires_at = $2 WHERE id = $1`,
		sessionID, expiresAt)
	return err
}

func (r *Repository) RevokeSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, sessionID)
	return err
}

// RevokeAllForAdmin ends every session belonging to an account, used after a
// password change or when an account is deactivated.
func (r *Repository) RevokeAllForAdmin(ctx context.Context, adminID uuid.UUID) error {
	_, err := r.db.Exec(ctx,
		`UPDATE admin_sessions SET revoked_at = now() WHERE admin_id = $1 AND revoked_at IS NULL`, adminID)
	return err
}

func (r *Repository) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	cutoff := time.Now().UTC().Add(-7 * 24 * time.Hour)
	tag, err := r.db.Exec(ctx,
		`DELETE FROM admin_sessions WHERE expires_at < $1 OR revoked_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// -------------------------------------------------------- login throttling --

func (r *Repository) RecordLoginAttempt(ctx context.Context, email, ip string, success bool) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO login_attempts (email, ip_address, success) VALUES ($1,$2,$3)`,
		strings.TrimSpace(email), ip, success)
	return err
}

// FailedAttempts counts recent failures for an account and for a source
// address. Both are checked so neither a single account nor a single host can
// be used to grind through passwords.
func (r *Repository) FailedAttempts(ctx context.Context, email, ip string, window time.Duration) (byEmail, byIP int, err error) {
	const query = `
		SELECT
			count(*) FILTER (WHERE lower(email) = lower($1)),
			count(*) FILTER (WHERE ip_address = $2)
		FROM login_attempts
		WHERE NOT success AND created_at > $3`

	err = r.db.QueryRow(ctx, query,
		strings.TrimSpace(email), ip, time.Now().UTC().Add(-window)).Scan(&byEmail, &byIP)
	return byEmail, byIP, err
}

// ClearFailedAttempts wipes the failure history after a successful login so a
// legitimate user is not locked out by their own earlier typos.
func (r *Repository) ClearFailedAttempts(ctx context.Context, email, ip string) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM login_attempts WHERE NOT success AND (lower(email) = lower($1) OR ip_address = $2)`,
		strings.TrimSpace(email), ip)
	return err
}

func (r *Repository) PruneLoginAttempts(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM login_attempts WHERE created_at < $1`, time.Now().UTC().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
