package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/auth"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
)

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	ip := httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies)
	result, err := s.deps.Auth.Login(r.Context(), req.Email, req.Password, ip, r.UserAgent())
	if err != nil {
		s.deps.Audit.Record(r.Context(),
			audit.Actor{Email: strings.TrimSpace(req.Email), IPAddress: ip, UserAgent: r.UserAgent()},
			audit.Entry{
				Action:  audit.ActionLoginFailed,
				Summary: "Кіру әрекеті сәтсіз аяқталды",
			})

		switch {
		case errors.Is(err, auth.ErrTooManyAttempts):
			w.Header().Set("Retry-After", "300")
			httpx.Fail(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
				"Тым көп сәтсіз әрекет. 15 минуттан кейін қайталаңыз.")
		case errors.Is(err, auth.ErrAccountDisabled):
			httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden, "Тіркелгі өшірілген")
		case errors.Is(err, auth.ErrInvalidCredentials):
			httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Email немесе құпия сөз қате")
		default:
			httpx.Internal(w, s.log, err, "login")
		}
		return
	}

	s.setSessionCookies(w, result.SessionToken, result.CSRFToken, result.ExpiresAt)

	id := result.Admin.ID
	s.deps.Audit.Record(r.Context(),
		audit.Actor{ID: &id, Email: result.Admin.Email, IPAddress: ip, UserAgent: r.UserAgent()},
		audit.Entry{Action: audit.ActionLogin, EntityType: "admin", EntityID: id.String(), Summary: "Жүйеге кірді"})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"admin":      result.Admin,
		"csrf_token": result.CSRFToken,
		"expires_at": result.ExpiresAt,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(s.cfg.Auth.SessionCookieName)
	if err == nil && cookie.Value != "" {
		if session, authErr := s.deps.Auth.Authenticate(r.Context(), cookie.Value); authErr == nil && session != nil {
			if err := s.deps.Auth.Logout(r.Context(), session.Session.ID); err != nil {
				s.log.Warn("logout failed", "error", err)
			}
			id := session.Admin.ID
			s.deps.Audit.Record(r.Context(),
				audit.Actor{
					ID:        &id,
					Email:     session.Admin.Email,
					IPAddress: httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies),
					UserAgent: r.UserAgent(),
				},
				audit.Entry{Action: audit.ActionLogout, EntityType: "admin", EntityID: id.String(), Summary: "Жүйеден шықты"})
		}
	}

	s.clearSessionCookies(w)
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if session == nil {
		httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Кіру қажет")
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"admin":      session.Admin,
		"csrf_token": session.Session.CSRFToken,
		"expires_at": session.Session.ExpiresAt,
		"timezone":   s.cfg.App.DefaultTimezone,
	})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	session := sessionFrom(r.Context())
	if session == nil {
		httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Кіру қажет")
		return
	}

	var req changePasswordRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Auth.ChangePassword(r.Context(), session.Admin.ID, req.CurrentPassword, req.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrInvalidCredentials):
			httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Ағымдағы құпия сөз қате")
		case errors.Is(err, auth.ErrWeakPassword):
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		default:
			httpx.Internal(w, s.log, err, "change password")
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionPasswordChanged,
		EntityType: "admin",
		EntityID:   session.Admin.ID.String(),
		Summary:    "Құпия сөзін өзгертті",
	})

	// Every session was revoked, including this one.
	s.clearSessionCookies(w)
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ----------------------------------------------------------- admin users --

func (s *Server) handleListAdmins(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	admins, err := s.deps.Auth.ListAdmins(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "list admins")
		return
	}
	httpx.JSON(w, http.StatusOK, admins)
}

type createAdminRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) handleCreateAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	var req createAdminRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	admin, err := s.deps.Auth.CreateAdmin(r.Context(), req.Email, req.Name, req.Password, domain.AdminRole(req.Role))
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrEmailTaken):
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Бұл email тіркелген")
		case errors.Is(err, auth.ErrWeakPassword):
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		default:
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionAdminCreated,
		EntityType: "admin",
		EntityID:   admin.ID.String(),
		Summary:    "Жаңа әкімші қосылды: " + admin.Email,
		New:        map[string]any{"email": admin.Email, "role": admin.Role},
	})

	httpx.JSON(w, http.StatusCreated, admin)
}

type updateAdminRequest struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	IsActive bool   `json:"is_active"`
}

func (s *Server) handleUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req updateAdminRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Auth.UpdateAdmin(r.Context(), id, req.Name, req.Role, req.IsActive); err != nil {
		if errors.Is(err, auth.ErrAdminNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Әкімші табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "update admin")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionAdminUpdated,
		EntityType: "admin",
		EntityID:   id.String(),
		Summary:    "Әкімші деректері жаңартылды",
		New:        req,
	})

	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleDeleteAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if admin := currentAdmin(r); admin != nil && admin.ID == id {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, "Өз тіркелгіңізді жоя алмайсыз")
		return
	}

	if err := s.deps.Auth.DeleteAdmin(r.Context(), id); err != nil {
		switch {
		case errors.Is(err, auth.ErrAdminNotFound):
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Әкімші табылмады")
		case errors.Is(err, auth.ErrLastOwner):
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Соңғы иесін жою мүмкін емес")
		default:
			httpx.Internal(w, s.log, err, "delete admin")
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionAdminDeleted,
		EntityType: "admin",
		EntityID:   id.String(),
		Summary:    "Әкімші жойылды",
	})

	httpx.NoContent(w)
}
