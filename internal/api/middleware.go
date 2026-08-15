package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/auth"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/logging"
)

type ctxKey string

const (
	ctxKeySession   ctxKey = "session"
	ctxKeyRequestID ctxKey = "request_id"
)

// sessionFrom returns the authenticated session, if any.
func sessionFrom(ctx context.Context) *auth.SessionWithAdmin {
	s, _ := ctx.Value(ctxKeySession).(*auth.SessionWithAdmin)
	return s
}

// actorFrom builds an audit actor from the request.
func (s *Server) actorFrom(r *http.Request) audit.Actor {
	actor := audit.Actor{
		IPAddress: httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies),
		UserAgent: r.UserAgent(),
	}
	if session := sessionFrom(r.Context()); session != nil {
		id := session.Admin.ID
		actor.ID = &id
		actor.Email = session.Admin.Email
	}
	return actor
}

// adminID returns the caller's id, or nil for unauthenticated requests.
func adminID(r *http.Request) *uuid.UUID {
	if session := sessionFrom(r.Context()); session != nil {
		id := session.Admin.ID
		return &id
	}
	return nil
}

// ------------------------------------------------------------ middleware --

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// http.ErrAbortHandler is the documented way to abandon a
				// response; it is not a bug and must not be logged as one.
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(debug.Stack())))
				httpx.Fail(w, http.StatusInternalServerError, httpx.CodeInternal,
					"Ішкі қате орын алды.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.NewString()
		w.Header().Set("X-Request-ID", id)

		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		ctx = logging.WithLogger(ctx, s.log.With(slog.String("request_id", id)))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// statusRecorder captures the response code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (rec *statusRecorder) WriteHeader(code int) {
	rec.status = code
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so Server-Sent Events keep working
// through the logging wrapper.
func (rec *statusRecorder) Flush() {
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The event stream is long-lived; logging it on completion would
		// produce one entry per disconnect with a meaningless duration.
		if r.URL.Path == "/api/stream" {
			next.ServeHTTP(w, r)
			return
		}

		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		level := slog.LevelInfo
		switch {
		case rec.status >= 500:
			level = slog.LevelError
		case rec.status >= 400:
			level = slog.LevelWarn
		// Static asset noise is not worth an info line per request.
		case !strings.HasPrefix(r.URL.Path, "/api/"):
			level = slog.LevelDebug
		}

		logging.FromContext(r.Context()).Log(r.Context(), level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("took", time.Since(started)),
			slog.String("ip", httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies)))
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(self), camera=()")

		// The dashboard ships no third-party code, so everything is locked to
		// the same origin. 'unsafe-inline' for styles covers the small number
		// of dynamic style attributes the chat view sets.
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"media-src 'self' blob:; "+
				"font-src 'self'; "+
				"connect-src 'self'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'self'; "+
				"form-action 'self'")

		if s.cfg.IsProduction() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// cors permits the configured origins. With no origins configured the API is
// same-origin only, which is the correct default for a bundled dashboard.
func (s *Server) cors(next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(s.cfg.HTTP.AllowedOrigins))
	for _, o := range s.cfg.HTTP.AllowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimRight(r.Header.Get("Origin"), "/")

		if origin != "" && allowed[origin] {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			h.Add("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth rejects requests without a valid session cookie.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(s.cfg.Auth.SessionCookieName)
		if err != nil || cookie.Value == "" {
			httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Кіру қажет")
			return
		}

		session, err := s.deps.Auth.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			if errors.Is(err, auth.ErrSessionInvalid) || errors.Is(err, auth.ErrAccountDisabled) {
				s.clearSessionCookies(w)
				httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Сессия аяқталды, қайта кіріңіз")
				return
			}
			httpx.Internal(w, s.log, err, "authenticate")
			return
		}

		ctx := context.WithValue(r.Context(), ctxKeySession, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireCSRF protects cookie-authenticated state changes.
//
// The token is issued at login, stored server-side with the session, and must
// be echoed in a header. A cross-site form post cannot read it, so it cannot
// forge a request even though the browser attaches the cookie.
func (s *Server) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		session := sessionFrom(r.Context())
		if session == nil {
			httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Кіру қажет")
			return
		}

		provided := r.Header.Get("X-CSRF-Token")
		if provided == "" ||
			subtle.ConstantTimeCompare([]byte(provided), []byte(session.Session.CSRFToken)) != 1 {
			s.log.Warn("csrf token rejected",
				slog.String("path", r.URL.Path),
				slog.String("admin", session.Admin.Email))
			httpx.Fail(w, http.StatusForbidden, httpx.CodeCSRF, "CSRF токені жарамсыз. Бетті жаңартыңыз.")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requireWriter blocks read-only operators from mutating endpoints.
func (s *Server) requireWriter(w http.ResponseWriter, r *http.Request) bool {
	session := sessionFrom(r.Context())
	if session == nil {
		httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "Кіру қажет")
		return false
	}
	if !session.Admin.Role.CanWrite() {
		httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden, "Бұл әрекетке рұқсатыңыз жоқ")
		return false
	}
	return true
}

func (s *Server) requireOwner(w http.ResponseWriter, r *http.Request) bool {
	session := sessionFrom(r.Context())
	if session == nil || !session.Admin.Role.CanManageAdmins() {
		httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden, "Тек иесі бұл әрекетті орындай алады")
		return false
	}
	return true
}

// ------------------------------------------------------- login throttling --

// loginLimiter is an in-process burst limiter in front of the database-backed
// lockout. It absorbs a flood cheaply; the durable counters in SQLite are
// what actually lock an account out.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

const (
	loginBurst      = 10
	loginRefillRate = 0.2 // tokens per second, i.e. one attempt every 5s sustained
)

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*bucket)}
}

func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		// Opportunistic cleanup keeps the map from growing unbounded without
		// needing a dedicated sweeper goroutine.
		if len(l.buckets) > 10000 {
			for k, v := range l.buckets {
				if now.Sub(v.lastSeen) > 10*time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		l.buckets[key] = &bucket{tokens: loginBurst - 1, lastSeen: now}
		return true
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens = minFloat(loginBurst, b.tokens+elapsed*loginRefillRate)
	b.lastSeen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func (s *Server) rateLimitLogin(next http.Handler) http.Handler {
	limiter := newLoginLimiter()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies)
		if !limiter.allow(ip) {
			w.Header().Set("Retry-After", "30")
			httpx.Fail(w, http.StatusTooManyRequests, httpx.CodeRateLimited,
				"Тым көп әрекет жасалды. Біраз күтіп, қайталаңыз.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------- cookies --

func (s *Server) setSessionCookies(w http.ResponseWriter, token, csrf string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.Auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		Domain:   s.cfg.Auth.CookieDomain,
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cfg.Auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})

	// The CSRF cookie is deliberately readable by scripts: the page reads it
	// and echoes it back in a header, which a cross-origin page cannot do.
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.Auth.CSRFCookieName,
		Value:    csrf,
		Path:     "/",
		Domain:   s.cfg.Auth.CookieDomain,
		Expires:  expires,
		HttpOnly: false,
		Secure:   s.cfg.Auth.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{s.cfg.Auth.SessionCookieName, s.cfg.Auth.CSRFCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			Domain:   s.cfg.Auth.CookieDomain,
			MaxAge:   -1,
			HttpOnly: name == s.cfg.Auth.SessionCookieName,
			Secure:   s.cfg.Auth.SecureCookies,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// currentAdmin is a small helper for handlers that need the caller.
func currentAdmin(r *http.Request) *domain.Admin {
	if session := sessionFrom(r.Context()); session != nil {
		return &session.Admin
	}
	return nil
}
