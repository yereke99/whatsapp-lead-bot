package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/realtime"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

// handleChatList powers the inbox sidebar.
func (s *Server) handleChatList(w http.ResponseWriter, r *http.Request) {
	search := httpx.QueryString(r, "search")
	unreadOnly := httpx.QueryBool(r, "unread")
	limit := httpx.QueryInt(r, "limit", 40, 1, 100)
	offset := httpx.QueryInt(r, "offset", 0, 0, 1_000_000)

	items, total, err := s.deps.Contacts.ChatList(r.Context(), search, unreadOnly, limit, offset)
	if err != nil {
		httpx.Internal(w, s.log, err, "chat list")
		return
	}
	if items == nil {
		// Encode an empty inbox as [] rather than null.
		items = []contacts.ChatListItem{}
	}
	httpx.Paged(w, items, total, limit, offset)
}

// handleContactMessages returns a conversation window.
func (s *Server) handleContactMessages(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	limit := httpx.QueryInt(r, "limit", 50, 1, 200)
	before := httpx.QueryUUID(r, "before")
	after := httpx.QueryUUID(r, "after")

	messages, err := s.deps.Messages.Timeline(r.Context(), id, before, after, limit)
	if err != nil {
		httpx.Internal(w, s.log, err, "load timeline")
		return
	}
	if messages == nil {
		messages = []domain.Message{}
	}

	// Attach a browser-usable url for any attachment stored locally.
	for i := range messages {
		if messages[i].MediaFileID != nil {
			messages[i].MediaAccess = s.deps.Media.Store().PublicURL(*messages[i].MediaFileID)
		}
	}

	httpx.JSON(w, http.StatusOK, messages)
}

func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Contacts.MarkRead(r.Context(), id); err != nil {
		httpx.Internal(w, s.log, err, "mark read")
		return
	}

	if s.deps.Hub != nil {
		s.deps.Hub.Publish(realtime.Event{
			Type:      realtime.EventContactUpdated,
			ContactID: id.String(),
			Data:      map[string]any{"unread_count": 0},
		})
	}
	httpx.JSON(w, http.StatusOK, map[string]int{"unread_count": 0})
}

// sendMessageRequest is the operator's manual reply.
type sendMessageRequest struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	MediaFileID string `json:"media_file_id,omitempty"`
	FileName    string `json:"file_name,omitempty"`
	LinkPreview *bool  `json:"link_preview,omitempty"`
}

// handleSendMessage sends a one-to-one reply from the chat console.
//
// The contact must already exist and must have written to us first: the
// platform has no path that opens a conversation with someone who did not.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req sendMessageRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	msgType := strings.ToUpper(strings.TrimSpace(req.Type))
	if msgType == "" {
		msgType = string(domain.TemplateText)
	}
	if !domain.ValidTemplateType(msgType) {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Хабарлама түрі жарамсыз")
		return
	}
	templateType := domain.TemplateType(msgType)

	contact, err := s.deps.Contacts.GetByID(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "get contact")
		return
	}
	if contact == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Байланыс табылмады")
		return
	}

	// Consent is checked before provider availability so the operator is told
	// the real reason: "this contact may not be messaged" is a permanent fact,
	// while a missing provider is a temporary configuration gap.
	if contact.FirstContactAt == nil {
		httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden,
			"Бұл байланысқа жазуға болмайды: ол бізге әлі жазбаған. Платформа ешкімге бірінші жазбайды.")
		return
	}
	if contact.OptedOut || contact.BlockedAt != nil {
		httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden,
			"Байланыс жазылымнан шыққан немесе бұғатталған")
		return
	}

	if !s.cfg.WhatsAppConfigured() {
		httpx.Fail(w, http.StatusServiceUnavailable, httpx.CodeNotConfigured,
			"Green API баптаулары енгізілмеген")
		return
	}

	out := messaging.Outbound{
		Type:     templateType,
		Text:     strings.TrimSpace(req.Text),
		IsManual: true,
		AdminID:  adminID(r),
		FileName: strings.TrimSpace(req.FileName),
	}
	out.LinkPreview = req.LinkPreview == nil || *req.LinkPreview

	if templateType.RequiresMedia() {
		if strings.TrimSpace(req.MediaFileID) == "" {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Файл таңдалмаған")
			return
		}
		mediaID, parseErr := uuid.Parse(strings.TrimSpace(req.MediaFileID))
		if parseErr != nil {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Файл идентификаторы жарамсыз")
			return
		}

		file, mediaErr := s.deps.Media.Get(r.Context(), mediaID)
		if mediaErr != nil {
			httpx.Internal(w, s.log, mediaErr, "load media")
			return
		}
		if file == nil {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Файл табылмады")
			return
		}
		if want := templateType.MediaKind(); want != "" && file.Kind != want {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation,
				fmt.Sprintf("Файл түрі сәйкес емес: %s қажет", want))
			return
		}

		abs, pathErr := s.deps.Media.Store().AbsPath(file.RelativePath)
		if pathErr != nil {
			httpx.Internal(w, s.log, pathErr, "resolve media path")
			return
		}

		out.MediaFileID = &file.ID
		out.MediaPath = abs
		out.MediaMIME = file.MimeType
		if out.FileName == "" {
			out.FileName = file.OriginalName
		}
	}

	if !templateType.AllowsCaption() {
		out.Text = ""
	}

	message, err := s.deps.Sender.Send(r.Context(), contact, out)
	if err != nil {
		switch {
		case errors.Is(err, messaging.ErrNoConsent):
			httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden,
				"Бұл байланысқа жазуға болмайды: ол бізге әлі жазбаған. Платформа ешкімге бірінші жазбайды.")
		case errors.Is(err, messaging.ErrOptedOut):
			httpx.Fail(w, http.StatusForbidden, httpx.CodeForbidden,
				"Байланыс жазылымнан шыққан немесе бұғатталған")
		case errors.Is(err, whatsapp.ErrNotConfigured):
			httpx.Fail(w, http.StatusServiceUnavailable, httpx.CodeNotConfigured,
				"Green API баптаулары енгізілмеген")
		default:
			s.log.Error("manual send failed",
				"contact_id", id, "error", err)
			httpx.FailWithDetails(w, http.StatusBadGateway, httpx.CodeUnavailable,
				"Хабарлама жіберілмеді", map[string]string{"reason": err.Error()})
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionManualMessage,
		EntityType: "contact",
		EntityID:   id.String(),
		Summary:    "Қолмен хабарлама жіберілді (" + msgType + ")",
		New:        map[string]any{"type": msgType, "has_media": out.MediaFileID != nil},
	})

	if message.MediaFileID != nil {
		message.MediaAccess = s.deps.Media.Store().PublicURL(*message.MediaFileID)
	}
	httpx.JSON(w, http.StatusCreated, message)
}

// handleEventStream serves the Server-Sent Events feed.
//
// The dashboard uses it to show inbound messages and delivery ticks the moment
// they happen, instead of polling.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Fail(w, http.StatusInternalServerError, httpx.CodeInternal,
			"Ағынды жіберу қолдау таппайды")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Disable proxy buffering, which would otherwise hold events until the
	// buffer fills and defeat the whole point of the stream.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Tell the browser how long to wait before reconnecting, and open the
	// stream immediately so the client's onopen fires.
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ctx := r.Context()
	events := s.deps.Hub.Subscribe(ctx)

	// Comment frames keep idle connections alive through proxies that close
	// silent sockets.
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case event, open := <-events:
			if !open {
				return
			}
			frame, err := realtime.Encode(event)
			if err != nil {
				s.log.Warn("encoding stream event failed", "error", err)
				continue
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()

		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func schedulerFilterForContact(contactID uuid.UUID) scheduler.ListFilter {
	return scheduler.ListFilter{ContactID: &contactID, Limit: 100}
}
