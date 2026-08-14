package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/domain"
)

var (
	ErrInvalidCredentials = errors.New("email or password is incorrect")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrTooManyAttempts    = errors.New("too many failed attempts; try again later")
	ErrEmailTaken         = errors.New("an account with this email already exists")
	ErrAdminNotFound      = errors.New("admin not found")
	ErrSessionInvalid     = errors.New("session is invalid or has expired")
	ErrLastOwner          = errors.New("the last active owner cannot be removed")
)

type Service struct {
	cfg  config.Auth
	repo *Repository
	log  *slog.Logger
}

func NewService(cfg config.Auth, repo *Repository, log *slog.Logger) *Service {
	return &Service{cfg: cfg, repo: repo, log: log.With(slog.String("component", "auth"))}
}

// LoginResult carries what the handler needs to set cookies.
type LoginResult struct {
	Admin        *domain.Admin
	SessionToken string
	CSRFToken    string
	ExpiresAt    time.Time
}

// Login authenticates an administrator.
//
// The failure path is deliberately uniform: a missing account, a wrong
// password and a malformed stored hash all return ErrInvalidCredentials after
// comparable work, so the response cannot be used to enumerate accounts.
func (s *Service) Login(ctx context.Context, email, password, ip, userAgent string) (*LoginResult, error) {
	email = strings.TrimSpace(strings.ToLower(email))

	byEmail, byIP, err := s.repo.FailedAttempts(ctx, email, ip, s.cfg.LoginLockWindow)
	if err != nil {
		return nil, fmt.Errorf("check login attempts: %w", err)
	}
	if byEmail >= s.cfg.LoginMaxAttempts || byIP >= s.cfg.LoginMaxAttempts*3 {
		s.log.Warn("login blocked by rate limit",
			slog.String("email", email),
			slog.String("ip", ip),
			slog.Int("failures_email", byEmail),
			slog.Int("failures_ip", byIP))
		return nil, ErrTooManyAttempts
	}

	admin, err := s.repo.FindByEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	if admin == nil {
		// Spend comparable time on a dummy verification so the absence of an
		// account is not detectable by response timing.
		_ = VerifyPassword(password, dummyHash)
		s.recordFailure(ctx, email, ip, "unknown account")
		return nil, ErrInvalidCredentials
	}

	if err := VerifyPassword(password, admin.PasswordHash); err != nil {
		s.recordFailure(ctx, email, ip, "bad password")
		return nil, ErrInvalidCredentials
	}

	if !admin.IsActive {
		s.recordFailure(ctx, email, ip, "disabled account")
		return nil, ErrAccountDisabled
	}

	sessionToken, err := GenerateToken(32)
	if err != nil {
		return nil, err
	}
	csrfToken, err := GenerateToken(32)
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().UTC().Add(s.cfg.SessionTTL)
	session := &domain.Session{
		AdminID:   admin.ID,
		CSRFToken: csrfToken,
		IPAddress: ip,
		UserAgent: truncate(userAgent, 500),
		ExpiresAt: expiresAt,
	}
	if err := s.repo.CreateSession(ctx, session, sessionToken); err != nil {
		return nil, err
	}

	if err := s.repo.RecordLoginAttempt(ctx, email, ip, true); err != nil {
		s.log.Warn("recording login attempt failed", slog.String("error", err.Error()))
	}
	if err := s.repo.ClearFailedAttempts(ctx, email, ip); err != nil {
		s.log.Warn("clearing login attempts failed", slog.String("error", err.Error()))
	}
	if err := s.repo.RecordLogin(ctx, admin.ID); err != nil {
		s.log.Warn("recording last login failed", slog.String("error", err.Error()))
	}

	s.log.Info("admin signed in",
		slog.String("admin_id", admin.ID.String()),
		slog.String("email", admin.Email),
		slog.String("ip", ip))

	return &LoginResult{
		Admin:        admin,
		SessionToken: sessionToken,
		CSRFToken:    csrfToken,
		ExpiresAt:    expiresAt,
	}, nil
}

func (s *Service) recordFailure(ctx context.Context, email, ip, reason string) {
	if err := s.repo.RecordLoginAttempt(ctx, email, ip, false); err != nil {
		s.log.Warn("recording failed login failed", slog.String("error", err.Error()))
	}
	s.log.Warn("login failed",
		slog.String("email", email),
		slog.String("ip", ip),
		slog.String("reason", reason))
}

// Authenticate resolves a session cookie to an administrator.
func (s *Service) Authenticate(ctx context.Context, token string) (*SessionWithAdmin, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrSessionInvalid
	}

	found, err := s.repo.FindSession(ctx, token)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrSessionInvalid
	}
	if found.Session.RevokedAt != nil {
		return nil, ErrSessionInvalid
	}
	if time.Now().UTC().After(found.Session.ExpiresAt) {
		return nil, ErrSessionInvalid
	}
	if !found.Admin.IsActive {
		return nil, ErrAccountDisabled
	}

	// Slide the expiry, but only once a minute: every request rewriting the
	// row would turn authentication into a write-heavy path for no benefit.
	if time.Since(found.Session.LastSeenAt) > time.Minute {
		newExpiry := time.Now().UTC().Add(s.cfg.SessionTTL)
		if err := s.repo.TouchSession(ctx, found.Session.ID, newExpiry); err != nil {
			s.log.Warn("touching session failed", slog.String("error", err.Error()))
		}
	}

	return found, nil
}

func (s *Service) Logout(ctx context.Context, sessionID uuid.UUID) error {
	return s.repo.RevokeSession(ctx, sessionID)
}

// ChangePassword updates the caller's own password and ends their other
// sessions, which is the expected behaviour after a suspected compromise.
func (s *Service) ChangePassword(ctx context.Context, adminID uuid.UUID, currentPassword, newPassword string) error {
	admin, err := s.repo.FindByID(ctx, adminID)
	if err != nil {
		return err
	}
	if admin == nil {
		return ErrAdminNotFound
	}
	if err := VerifyPassword(currentPassword, admin.PasswordHash); err != nil {
		return ErrInvalidCredentials
	}

	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.repo.UpdatePassword(ctx, adminID, hash); err != nil {
		return err
	}

	if err := s.repo.RevokeAllForAdmin(ctx, adminID); err != nil {
		s.log.Warn("revoking sessions after password change failed", slog.String("error", err.Error()))
	}

	s.log.Info("password changed", slog.String("admin_id", adminID.String()))
	return nil
}

// CreateAdmin adds an operator account.
func (s *Service) CreateAdmin(ctx context.Context, email, name, password string, role domain.AdminRole) (*domain.Admin, error) {
	email = strings.TrimSpace(strings.ToLower(email))
	if !strings.Contains(email, "@") || len(email) < 5 {
		return nil, errors.New("email мекенжайы жарамсыз")
	}
	if role == "" {
		role = domain.RoleAdmin
	}
	if role != domain.RoleOwner && role != domain.RoleAdmin && role != domain.RoleViewer {
		return nil, fmt.Errorf("рөл жарамсыз: %s", role)
	}

	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}

	admin := &domain.Admin{
		Email:        email,
		Name:         strings.TrimSpace(name),
		PasswordHash: hash,
		Role:         role,
		IsActive:     true,
	}
	if err := s.repo.CreateAdmin(ctx, admin); err != nil {
		return nil, err
	}

	s.log.Info("admin account created",
		slog.String("admin_id", admin.ID.String()),
		slog.String("email", admin.Email),
		slog.String("role", string(admin.Role)))
	return admin, nil
}

func (s *Service) ListAdmins(ctx context.Context) ([]domain.Admin, error) {
	return s.repo.ListAdmins(ctx)
}

func (s *Service) UpdateAdmin(ctx context.Context, id uuid.UUID, name, role string, active bool) error {
	if err := s.repo.UpdateAdmin(ctx, id, name, role, active); err != nil {
		return err
	}
	if !active {
		// A deactivated operator must lose access immediately, not when their
		// cookie happens to expire.
		if err := s.repo.RevokeAllForAdmin(ctx, id); err != nil {
			s.log.Warn("revoking sessions for disabled admin failed", slog.String("error", err.Error()))
		}
	}
	return nil
}

// DeleteAdmin removes an operator, refusing to strand the installation without
// an active owner.
func (s *Service) DeleteAdmin(ctx context.Context, id uuid.UUID) error {
	admins, err := s.repo.ListAdmins(ctx)
	if err != nil {
		return err
	}

	owners := 0
	var target *domain.Admin
	for i := range admins {
		if admins[i].Role == domain.RoleOwner && admins[i].IsActive {
			owners++
		}
		if admins[i].ID == id {
			target = &admins[i]
		}
	}
	if target == nil {
		return ErrAdminNotFound
	}
	if target.Role == domain.RoleOwner && owners <= 1 {
		return ErrLastOwner
	}

	if err := s.repo.RevokeAllForAdmin(ctx, id); err != nil {
		s.log.Warn("revoking sessions before delete failed", slog.String("error", err.Error()))
	}
	return s.repo.DeleteAdmin(ctx, id)
}

// EnsureBootstrapAdmin creates the first owner account from configuration when
// the installation has none.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context) error {
	count, err := s.repo.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	email := strings.TrimSpace(s.cfg.BootstrapEmail)
	password := s.cfg.BootstrapPassword

	if email == "" || password == "" {
		s.log.Warn("no administrator exists and ADMIN_EMAIL/ADMIN_PASSWORD are not set; " +
			"set them and restart to create the first account")
		return nil
	}

	admin, err := s.CreateAdmin(ctx, email, "Administrator", password, domain.RoleOwner)
	if err != nil {
		return fmt.Errorf("create bootstrap admin: %w", err)
	}

	s.log.Info("bootstrap administrator created", slog.String("email", admin.Email))
	return nil
}

// StartMaintenance prunes expired sessions and old login attempts.
func (s *Service) StartMaintenance(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := s.repo.PurgeExpiredSessions(ctx); err != nil {
					s.log.Error("purging sessions failed", slog.String("error", err.Error()))
				} else if n > 0 {
					s.log.Debug("purged expired sessions", slog.Int64("count", n))
				}
				if _, err := s.repo.PruneLoginAttempts(ctx, 30*24*time.Hour); err != nil {
					s.log.Error("pruning login attempts failed", slog.String("error", err.Error()))
				}
			}
		}
	}()
}

// dummyHash is a real Argon2id hash of an unguessable value, used to keep the
// unknown-account path computationally similar to the known-account path.
const dummyHash = "$argon2id$v=19$m=65536,t=1,p=2$YWJjZGVmZ2hpamtsbW5vcA$" +
	"C1YzOnh2wq0DDcPXHTLxZ0Nx4a4mL0h0Yy1RHhF2b3s"

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
