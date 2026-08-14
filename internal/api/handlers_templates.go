package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/templates"
)

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	items, err := s.deps.Templates.List(r.Context(),
		httpx.QueryString(r, "search"),
		strings.ToUpper(httpx.QueryString(r, "type")),
		httpx.QueryBool(r, "include_archived"))
	if err != nil {
		httpx.Internal(w, s.log, err, "list templates")
		return
	}
	if items == nil {
		items = []domain.MessageTemplate{}
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	tpl, err := s.deps.Templates.Get(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "get template")
		return
	}
	if tpl == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Шаблон табылмады")
		return
	}
	httpx.JSON(w, http.StatusOK, tpl)
}

type templateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Body        string `json:"body"`
	MediaFileID string `json:"media_file_id"`
	FileName    string `json:"file_name"`
	LinkPreview *bool  `json:"link_preview"`
}

func (req templateRequest) toInput() (templates.Input, error) {
	in := templates.Input{
		Name:        req.Name,
		Description: req.Description,
		Type:        strings.ToUpper(strings.TrimSpace(req.Type)),
		Body:        req.Body,
		FileName:    req.FileName,
		LinkPreview: req.LinkPreview == nil || *req.LinkPreview,
	}

	if raw := strings.TrimSpace(req.MediaFileID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return in, errors.New("файл идентификаторы жарамсыз")
		}
		in.MediaFileID = &id
	}
	return in, nil
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	var req templateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	in, err := req.toInput()
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	tpl, err := s.deps.Templates.Create(r.Context(), in, adminID(r))
	if err != nil {
		s.writeTemplateError(w, err)
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTemplateCreated,
		EntityType: "template",
		EntityID:   tpl.ID.String(),
		Summary:    "Шаблон құрылды: " + tpl.Name,
		New:        map[string]any{"name": tpl.Name, "type": tpl.Type},
	})

	httpx.JSON(w, http.StatusCreated, tpl)
}

func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req templateRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	in, err := req.toInput()
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	before, err := s.deps.Templates.Get(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "load template")
		return
	}
	if before == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Шаблон табылмады")
		return
	}

	tpl, err := s.deps.Templates.Update(r.Context(), id, in, adminID(r))
	if err != nil {
		s.writeTemplateError(w, err)
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTemplateUpdated,
		EntityType: "template",
		EntityID:   id.String(),
		Summary:    "Шаблон жаңартылды: " + tpl.Name,
		Old:        map[string]any{"version": before.Version, "body": before.Body},
		New:        map[string]any{"version": tpl.Version, "body": tpl.Body},
	})

	httpx.JSON(w, http.StatusOK, tpl)
}

func (s *Server) handleDuplicateTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	tpl, err := s.deps.Templates.Duplicate(r.Context(), id, adminID(r))
	if err != nil {
		s.writeTemplateError(w, err)
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTemplateDuplicated,
		EntityType: "template",
		EntityID:   tpl.ID.String(),
		Summary:    "Шаблон көшірілді: " + tpl.Name,
	})

	httpx.JSON(w, http.StatusCreated, tpl)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	archived, err := s.deps.Templates.Delete(r.Context(), id)
	if err != nil {
		s.writeTemplateError(w, err)
		return
	}

	summary := "Шаблон жойылды"
	if archived {
		summary = "Шаблон мұрағатталды (қадамдарда қолданылуда)"
	}
	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTemplateDeleted,
		EntityType: "template",
		EntityID:   id.String(),
		Summary:    summary,
	})

	httpx.JSON(w, http.StatusOK, map[string]bool{"archived": archived})
}

func (s *Server) handlePreviewTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	// Any query parameter may override a sample value, which lets the preview
	// show a real campaign's link and time.
	values := map[string]string{}
	for key, vals := range r.URL.Query() {
		if len(vals) > 0 {
			values[strings.ToLower(key)] = vals[0]
		}
	}

	preview, err := s.deps.Templates.Preview(r.Context(), id, values)
	if err != nil {
		httpx.Internal(w, s.log, err, "preview template")
		return
	}
	if preview == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Шаблон табылмады")
		return
	}
	httpx.JSON(w, http.StatusOK, preview)
}

func (s *Server) handleTemplateVersions(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	versions, err := s.deps.Templates.Versions(r.Context(), id, httpx.QueryInt(r, "limit", 20, 1, 100))
	if err != nil {
		httpx.Internal(w, s.log, err, "template versions")
		return
	}
	if versions == nil {
		versions = []templates.TemplateVersion{}
	}
	httpx.JSON(w, http.StatusOK, versions)
}

func (s *Server) handleTemplateVariables(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, templates.Variables())
}

func (s *Server) writeTemplateError(w http.ResponseWriter, err error) {
	var validation templates.ValidationErrors
	switch {
	case errors.As(err, &validation):
		httpx.FailWithDetails(w, http.StatusBadRequest, httpx.CodeValidation,
			"Шаблон деректері дұрыс емес", validation)
	case errors.Is(err, templates.ErrNameTaken):
		httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Бұл атаумен шаблон бар")
	case errors.Is(err, templates.ErrNotFound):
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Шаблон табылмады")
	case errors.Is(err, templates.ErrInUse):
		httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Шаблон қадамдарда қолданылуда")
	default:
		httpx.Internal(w, s.log, err, "template operation")
	}
}
