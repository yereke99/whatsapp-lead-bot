package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// handleContactMessages returns one contact's message history.
//
// This is a read-only record of what automation sent and what the contact
// replied, not a chat client: there is no send endpoint and no live feed. It
// exists so an operator can answer "did the 20:45 reminder actually go out?"
// without querying the database.
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

func schedulerFilterForContact(contactID uuid.UUID) scheduler.ListFilter {
	return scheduler.ListFilter{ContactID: &contactID, Limit: 100}
}
