package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ayran/whatsapp-automation/internal/auth"
	"github.com/ayran/whatsapp-automation/internal/domain"
)

func TestIncomingRecoveryRequiresOwner(t *testing.T) {
	s := &Server{log: slog.Default()}
	for _, role := range []domain.AdminRole{domain.RoleViewer, domain.RoleAdmin} {
		for _, handler := range []http.HandlerFunc{s.handleRecoveryPreview, s.handleRecoveryRequeue} {
			r := httptest.NewRequest(http.MethodPost, "/api/incoming-recovery/requeue", strings.NewReader(`{"event_ids":[]}`))
			r = r.WithContext(context.WithValue(r.Context(), ctxKeySession, &auth.SessionWithAdmin{Admin: domain.Admin{Role: role}}))
			w := httptest.NewRecorder()
			handler(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("role %s got %d", role, w.Code)
			}
		}
	}
}

func TestIncomingRecoveryRequiresCSRFAndValidEventIDs(t *testing.T) {
	s := &Server{log: slog.Default()}
	owner := &auth.SessionWithAdmin{Admin: domain.Admin{Role: domain.RoleOwner}, Session: domain.Session{CSRFToken: "valid"}}
	r := httptest.NewRequest(http.MethodPost, "/api/incoming-recovery/requeue", strings.NewReader(`{"event_ids":["invalid"]}`))
	r = r.WithContext(context.WithValue(r.Context(), ctxKeySession, owner))
	w := httptest.NewRecorder()
	s.requireCSRF(http.HandlerFunc(s.handleRecoveryRequeue)).ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("missing csrf got %d", w.Code)
	}
	r.Header.Set("X-CSRF-Token", "valid")
	w = httptest.NewRecorder()
	s.requireCSRF(http.HandlerFunc(s.handleRecoveryRequeue)).ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid event id got %d", w.Code)
	}
}
