package api

import (
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/pkg/render"
)

// ------------------------------------------------------------- dashboard --

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.App.DefaultTimezone

	summary, err := s.deps.Analytics.Summarize(r.Context(), tz)
	if err != nil {
		httpx.Internal(w, s.log, err, "dashboard summary")
		return
	}

	queue, err := s.deps.Jobs.Stats(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "queue stats")
		return
	}

	campaignStats, err := s.deps.Analytics.CampaignStats(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "campaign stats")
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"summary":   summary,
		"queue":     queue,
		"campaigns": campaignStats,
		"timezone":  tz,
		"provider": map[string]any{
			"configured": s.cfg.WhatsAppConfigured(),
		},
	})
}

func (s *Server) handleContactSeries(w http.ResponseWriter, r *http.Request) {
	days := httpx.QueryInt(r, "days", 30, 1, 365)
	series, err := s.deps.Analytics.ContactsOverTime(r.Context(), s.cfg.App.DefaultTimezone, days)
	if err != nil {
		httpx.Internal(w, s.log, err, "contacts series")
		return
	}
	httpx.JSON(w, http.StatusOK, series)
}

func (s *Server) handleMessageSeries(w http.ResponseWriter, r *http.Request) {
	days := httpx.QueryInt(r, "days", 30, 1, 365)
	series, err := s.deps.Analytics.MessagesOverTime(r.Context(), s.cfg.App.DefaultTimezone, days)
	if err != nil {
		httpx.Internal(w, s.log, err, "messages series")
		return
	}
	httpx.JSON(w, http.StatusOK, series)
}

func (s *Server) handleDeliveryBreakdown(w http.ResponseWriter, r *http.Request) {
	days := httpx.QueryInt(r, "days", 30, 1, 365)
	breakdown, err := s.deps.Analytics.Delivery(r.Context(), days)
	if err != nil {
		httpx.Internal(w, s.log, err, "delivery breakdown")
		return
	}
	httpx.JSON(w, http.StatusOK, breakdown)
}

func (s *Server) handleCampaignStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.deps.Analytics.CampaignStats(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "campaign stats")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

func (s *Server) handleTriggerStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.deps.Analytics.TriggerStats(r.Context(), httpx.QueryInt(r, "limit", 20, 1, 100))
	if err != nil {
		httpx.Internal(w, s.log, err, "trigger stats")
		return
	}
	httpx.JSON(w, http.StatusOK, stats)
}

// ------------------------------------------------------ scheduled messages --

func (s *Server) handleListScheduled(w http.ResponseWriter, r *http.Request) {
	tz := s.cfg.App.DefaultTimezone

	filter := scheduler.ListFilter{
		CampaignID: httpx.QueryUUID(r, "campaign_id"),
		ContactID:  httpx.QueryUUID(r, "contact_id"),
		Status:     strings.ToUpper(httpx.QueryString(r, "status")),
		From:       httpx.QueryDate(r, "from", tz),
		To:         httpx.QueryDate(r, "to", tz),
		Limit:      httpx.QueryInt(r, "limit", 50, 1, 200),
		Offset:     httpx.QueryInt(r, "offset", 0, 0, 1_000_000),
	}

	switch domain.JobStatus(filter.Status) {
	case domain.JobPending, domain.JobProcessing, domain.JobSent, domain.JobFailed, domain.JobCancelled:
	default:
		filter.Status = ""
	}

	items, total, err := s.deps.Jobs.List(r.Context(), filter)
	if err != nil {
		httpx.Internal(w, s.log, err, "list scheduled messages")
		return
	}
	if items == nil {
		items = []domain.ScheduledMessage{}
	}
	httpx.Paged(w, items, total, filter.Limit, filter.Offset)
}

func (s *Server) handleRetryJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Jobs.Requeue(r.Context(), id); err != nil {
		httpx.Internal(w, s.log, err, "requeue job")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionJobRequeued,
		EntityType: "scheduled_message",
		EntityID:   id.String(),
		Summary:    "Жоспарланған хабарлама қайта кезекке қойылды",
	})

	httpx.JSON(w, http.StatusOK, map[string]string{"status": "PENDING"})
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Jobs.Cancel(r.Context(), id, "cancelled by administrator"); err != nil {
		httpx.Internal(w, s.log, err, "cancel job")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionJobCancelled,
		EntityType: "scheduled_message",
		EntityID:   id.String(),
		Summary:    "Жоспарланған хабарлама тоқтатылды",
	})

	httpx.JSON(w, http.StatusOK, map[string]string{"status": "CANCELLED"})
}

// ---------------------------------------------------------------- export --

func (s *Server) handleExportContacts(w http.ResponseWriter, r *http.Request) {
	filter := s.contactFilterFrom(r)
	// The export covers the whole filtered set, not the current page.
	filter.Limit = 0
	filter.Offset = 0

	tz := s.cfg.App.DefaultTimezone
	filename := exportsFilename("contacts", tz)

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")

	count, err := s.deps.Exports.Contacts(r.Context(), w, filter, tz)
	if err != nil {
		// Headers are already sent, so the error can only be logged.
		s.log.Error("contact export failed", "error", err, "rows_written", count)
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionExport,
		EntityType: "contact",
		Summary:    "Байланыстар экспортталды",
		New:        map[string]any{"rows": count, "filters": r.URL.RawQuery},
	})
}

// ------------------------------------------------------------ diagnostics --

func (s *Server) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	filter := audit.ListFilter{
		Action:     httpx.QueryString(r, "action"),
		EntityType: httpx.QueryString(r, "entity_type"),
		EntityID:   httpx.QueryString(r, "entity_id"),
		AdminID:    httpx.QueryUUID(r, "admin_id"),
		Search:     httpx.QueryString(r, "search"),
		Limit:      httpx.QueryInt(r, "limit", 50, 1, 200),
		Offset:     httpx.QueryInt(r, "offset", 0, 0, 1_000_000),
	}

	items, total, err := s.deps.Audit.List(r.Context(), filter)
	if err != nil {
		httpx.Internal(w, s.log, err, "list audit logs")
		return
	}
	if items == nil {
		items = []domain.AuditLog{}
	}
	httpx.Paged(w, items, total, filter.Limit, filter.Offset)
}

func (s *Server) handleWebhookEvents(w http.ResponseWriter, r *http.Request) {
	limit := httpx.QueryInt(r, "limit", 50, 1, 200)
	offset := httpx.QueryInt(r, "offset", 0, 0, 1_000_000)
	status := strings.ToUpper(httpx.QueryString(r, "status"))

	items, total, err := s.deps.WebhookRepo.List(r.Context(), status, limit, offset)
	if err != nil {
		httpx.Internal(w, s.log, err, "list webhook events")
		return
	}
	if items == nil {
		items = []domain.WebhookEvent{}
	}
	httpx.Paged(w, items, total, limit, offset)
}

func (s *Server) handleProviderState(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.WhatsAppConfigured() {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"configured": false,
			"state":      "not_configured",
			"message":    "GREEN_API_INSTANCE_ID және GREEN_API_TOKEN енгізілмеген",
		})
		return
	}

	ctx, cancel := contextWithTimeout(r, 10*time.Second)
	defer cancel()

	state, err := s.deps.Provider.State(ctx)
	if err != nil {
		httpx.JSON(w, http.StatusOK, map[string]any{
			"configured": true,
			"state":      "error",
			"message":    err.Error(),
		})
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"configured": true,
		"state":      state.State,
		"authorized": state.Authorized,
	})
}

func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]any{
		"timezone":            s.cfg.App.DefaultTimezone,
		"environment":         s.cfg.App.Env,
		"provider_configured": s.cfg.WhatsAppConfigured(),
		"voice_supported":     s.deps.Media.Transcoder().Available(),
		"max_upload_mb":       s.cfg.Media.MaxUploadMB,
		"scheduler_enabled":   s.cfg.Scheduler.Enabled,
		"template_variables":  render.Catalog,
		"realtime_clients":    s.deps.Hub.Clients(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 3*time.Second)
	defer cancel()

	status := "ok"
	dbStatus := "ok"
	code := http.StatusOK

	if err := s.deps.DB.Health(ctx); err != nil {
		status = "degraded"
		dbStatus = "unreachable"
		code = http.StatusServiceUnavailable
	}

	httpx.JSON(w, code, map[string]any{
		"status":           status,
		"database":         dbStatus,
		"provider_ready":   s.cfg.WhatsAppConfigured(),
		"realtime_clients": s.deps.Hub.Clients(),
		"time":             time.Now().UTC(),
	})
}

// --------------------------------------------------------------- webhook --

// handleGreenAPIWebhook receives provider notifications.
//
// It always answers 200 for a payload it has stored or already seen: a
// non-2xx makes Green API retry, and retrying a message we have already
// queued only adds load.
func (s *Server) handleGreenAPIWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.verifyWebhookToken(r) {
		s.log.Warn("webhook rejected: bad token",
			"ip", httpx.ClientIP(r, s.cfg.HTTP.TrustedProxies))
		httpx.Fail(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid webhook token")
		return
	}

	// 1 MiB is far above any Green API notification.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, "cannot read body")
		return
	}
	if len(body) == 0 {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "empty"})
		return
	}

	accepted, err := s.deps.Webhooks.Ingest(r.Context(), body)
	if err != nil {
		// A payload we cannot parse will not parse on retry either.
		s.log.Warn("webhook payload rejected", "error", err)
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	if accepted {
		httpx.JSON(w, http.StatusOK, map[string]string{"status": "accepted"})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
}

func (s *Server) handleWebhookProbe(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// verifyWebhookToken checks Green API's configured webhook token.
//
// The provider sends it as "Authorization: Bearer <token>". When no token is
// configured the endpoint stays open, which is only acceptable behind a
// firewall, so the server logs a warning about it at startup.
func (s *Server) verifyWebhookToken(r *http.Request) bool {
	expected := strings.TrimSpace(s.cfg.GreenAPI.WebhookToken)
	if expected == "" {
		return true
	}

	provided := strings.TrimSpace(r.Header.Get("Authorization"))
	provided = strings.TrimPrefix(provided, "Bearer ")
	provided = strings.TrimSpace(provided)

	if provided == "" {
		// Some deployments put the token in the url instead.
		provided = strings.TrimSpace(r.URL.Query().Get("token"))
	}

	return constantTimeEqual(provided, expected)
}

func exportsFilename(prefix, tz string) string {
	return exportsFilenameFn(prefix, tz)
}

// exportsFilenameFn is a variable so tests can pin the timestamp.
var exportsFilenameFn = defaultExportFilename
