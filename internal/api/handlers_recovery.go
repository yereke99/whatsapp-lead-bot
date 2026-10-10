package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/httpx"
)

func (s *Server) handleRecoveryPreview(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}
	preview, err := s.deps.Notifications.PreviewRecovery(r.Context(), int64(httpx.QueryInt(r, "instance_id", 0, 0, 1000000)))
	if err != nil {
		httpx.Internal(w, s.log, err, "preview incoming recovery")
		return
	}
	httpx.JSON(w, http.StatusOK, preview)
}

func (s *Server) handleRecoveryRequeue(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}
	var request struct {
		EventIDs []uuid.UUID `json:"event_ids"`
	}
	if err := httpx.DecodeJSON(w, r, &request); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	result, err := s.deps.Notifications.RequeueRecovery(r.Context(), request.EventIDs)
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}
	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action: audit.ActionIncomingRecovered, EntityType: "webhook_event",
		Summary: "Incoming trigger recovery queued", New: result,
	})
	httpx.JSON(w, http.StatusOK, result)
}
