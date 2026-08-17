// Package api exposes the REST surface and serves the admin single-page app.
package api

import (
	"log/slog"
	"net/http"

	"github.com/ayran/whatsapp-automation/internal/analytics"
	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/auth"
	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/exports"
	"github.com/ayran/whatsapp-automation/internal/inbound"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/storage/sqlite"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

// Dependencies collects everything the handlers need.
type Dependencies struct {
	Config           *config.Config
	Log              *slog.Logger
	DB               *sqlite.DB
	Auth             *auth.Service
	Audit            *audit.Logger
	Contacts         *contacts.Repository
	Messages         *conversations.Repository
	Campaigns        *campaigns.Service
	Templates        *templates.Service
	TemplateRepo     *templates.Repository
	Media            *media.Service
	Jobs             *scheduler.Repository
	Sender           *messaging.Sender
	Notifications    *inbound.Processor
	NotificationRepo *inbound.Repository
	Analytics        *analytics.Service
	Exports          *exports.Service
	Provider         *greenapi.Client
}

// Server owns the HTTP router.
type Server struct {
	deps Dependencies
	cfg  *config.Config
	log  *slog.Logger
	mux  *http.ServeMux
}

func NewServer(deps Dependencies) *Server {
	s := &Server{
		deps: deps,
		cfg:  deps.Config,
		log:  deps.Log,
		mux:  http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the fully wrapped HTTP handler.
func (s *Server) Handler() http.Handler {
	// Outermost first: recovery must survive anything the inner layers do, and
	// request logging should see the final status code.
	return s.recoverPanics(
		s.withRequestID(
			s.logRequests(
				s.securityHeaders(
					s.cors(s.mux)))))
}

func (s *Server) routes() {
	// ---- public -----------------------------------------------------------
	s.mux.HandleFunc("GET /api/health", s.handleHealth)

	s.mux.Handle("POST /api/auth/login", s.rateLimitLogin(http.HandlerFunc(s.handleLogin)))
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)

	// ---- authenticated ----------------------------------------------------
	protected := http.NewServeMux()

	protected.HandleFunc("GET /api/me", s.handleMe)
	protected.HandleFunc("POST /api/me/password", s.handleChangePassword)

	protected.HandleFunc("GET /api/dashboard", s.handleDashboard)
	protected.HandleFunc("GET /api/analytics/contacts", s.handleContactSeries)
	protected.HandleFunc("GET /api/analytics/messages", s.handleMessageSeries)
	protected.HandleFunc("GET /api/analytics/delivery", s.handleDeliveryBreakdown)
	protected.HandleFunc("GET /api/analytics/campaigns", s.handleCampaignStats)
	protected.HandleFunc("GET /api/analytics/triggers", s.handleTriggerStats)

	protected.HandleFunc("GET /api/contacts", s.handleListContacts)
	protected.HandleFunc("GET /api/contacts/{id}", s.handleGetContact)
	protected.HandleFunc("PUT /api/contacts/{id}", s.handleUpdateContact)
	protected.HandleFunc("DELETE /api/contacts/{id}", s.handleDeleteContact)
	protected.HandleFunc("GET /api/contacts/{id}/messages", s.handleContactMessages)
	protected.HandleFunc("POST /api/contacts/{id}/block", s.handleBlockContact)
	protected.HandleFunc("POST /api/contacts/{id}/unsubscribe", s.handleUnsubscribeContact)
	protected.HandleFunc("POST /api/contacts/bulk", s.handleBulkAction)
	protected.HandleFunc("GET /api/tags", s.handleListTags)

	protected.HandleFunc("GET /api/campaigns", s.handleListCampaigns)
	protected.HandleFunc("POST /api/campaigns", s.handleCreateCampaign)
	protected.HandleFunc("GET /api/campaigns/{id}", s.handleGetCampaign)
	protected.HandleFunc("PUT /api/campaigns/{id}", s.handleUpdateCampaign)
	protected.HandleFunc("DELETE /api/campaigns/{id}", s.handleDeleteCampaign)
	protected.HandleFunc("POST /api/campaigns/{id}/status", s.handleCampaignStatus)
	protected.HandleFunc("POST /api/campaigns/{id}/duplicate", s.handleDuplicateCampaign)
	// preview and timeline are the same projection: the campaign's steps
	// resolved to real times, in send order. Both names are served because the
	// panel reads it as a timeline and the API has always called it a preview.
	protected.HandleFunc("GET /api/campaigns/{id}/preview", s.handleCampaignPreview)
	protected.HandleFunc("GET /api/campaigns/{id}/timeline", s.handleCampaignPreview)
	protected.HandleFunc("GET /api/campaigns/{id}/validate", s.handleCampaignValidate)
	protected.HandleFunc("GET /api/campaigns/{id}/scheduled-messages", s.handleCampaignScheduled)
	protected.HandleFunc("POST /api/campaigns/{id}/reconcile", s.handleReconcileCampaign)

	protected.HandleFunc("GET /api/campaigns/{id}/steps", s.handleListSteps)
	protected.HandleFunc("POST /api/campaigns/{id}/steps", s.handleCreateStep)
	protected.HandleFunc("PUT /api/campaigns/{id}/steps/{stepId}", s.handleUpdateStep)
	protected.HandleFunc("DELETE /api/campaigns/{id}/steps/{stepId}", s.handleDeleteStep)
	protected.HandleFunc("POST /api/campaigns/{id}/steps/reorder", s.handleReorderSteps)

	protected.HandleFunc("GET /api/triggers", s.handleListTriggers)
	protected.HandleFunc("POST /api/campaigns/{id}/triggers", s.handleCreateTrigger)
	protected.HandleFunc("PUT /api/triggers/{id}", s.handleUpdateTrigger)
	protected.HandleFunc("DELETE /api/triggers/{id}", s.handleDeleteTrigger)

	protected.HandleFunc("GET /api/templates", s.handleListTemplates)
	protected.HandleFunc("POST /api/templates", s.handleCreateTemplate)
	protected.HandleFunc("GET /api/templates/variables", s.handleTemplateVariables)
	protected.HandleFunc("GET /api/templates/{id}", s.handleGetTemplate)
	protected.HandleFunc("PUT /api/templates/{id}", s.handleUpdateTemplate)
	protected.HandleFunc("DELETE /api/templates/{id}", s.handleDeleteTemplate)
	protected.HandleFunc("POST /api/templates/{id}/duplicate", s.handleDuplicateTemplate)
	protected.HandleFunc("GET /api/templates/{id}/preview", s.handlePreviewTemplate)
	protected.HandleFunc("GET /api/templates/{id}/versions", s.handleTemplateVersions)

	protected.HandleFunc("GET /api/media", s.handleListMedia)
	protected.HandleFunc("POST /api/media/upload", s.handleUploadMedia)
	protected.HandleFunc("GET /api/media/{id}/content", s.handleMediaContent)
	protected.HandleFunc("DELETE /api/media/{id}", s.handleDeleteMedia)

	protected.HandleFunc("GET /api/scheduled-messages", s.handleListScheduled)
	protected.HandleFunc("POST /api/scheduled-messages/{id}/retry", s.handleRetryJob)
	protected.HandleFunc("POST /api/scheduled-messages/{id}/cancel", s.handleCancelJob)

	protected.HandleFunc("GET /api/exports/contacts", s.handleExportContacts)

	protected.HandleFunc("GET /api/audit-logs", s.handleAuditLogs)
	protected.HandleFunc("GET /api/webhook-events", s.handleWebhookEvents)
	protected.HandleFunc("GET /api/system/consistency", s.handleConsistency)
	protected.HandleFunc("GET /api/system/provider", s.handleProviderState)
	protected.HandleFunc("GET /api/system/settings", s.handleSystemSettings)

	protected.HandleFunc("GET /api/admins", s.handleListAdmins)
	protected.HandleFunc("POST /api/admins", s.handleCreateAdmin)
	protected.HandleFunc("PUT /api/admins/{id}", s.handleUpdateAdmin)
	protected.HandleFunc("DELETE /api/admins/{id}", s.handleDeleteAdmin)

	s.mux.Handle("/api/", s.requireAuth(s.requireCSRF(protected)))

	// ---- admin single-page app -------------------------------------------
	s.mux.Handle("/", s.staticHandler())
}
