package api

import (
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/pkg/phone"
	"github.com/ayran/whatsapp-automation/pkg/textnorm"
)

func (s *Server) contactFilterFrom(r *http.Request) contacts.Filter {
	tz := s.cfg.App.DefaultTimezone

	return contacts.Filter{
		Search:      httpx.QueryString(r, "search"),
		Status:      normalizeStatus(httpx.QueryString(r, "status")),
		CampaignID:  httpx.QueryUUID(r, "campaign_id"),
		Trigger:     httpx.QueryString(r, "trigger"),
		OptedOut:    httpx.QueryBoolPtr(r, "opted_out"),
		TagID:       httpx.QueryUUID(r, "tag_id"),
		CreatedFrom: httpx.QueryDate(r, "created_from", tz),
		CreatedTo:   httpx.QueryDate(r, "created_to", tz),
		Sort:        httpx.QueryString(r, "sort"),
		Limit:       httpx.QueryInt(r, "limit", 25, 1, 200),
		Offset:      httpx.QueryInt(r, "offset", 0, 0, 1_000_000),
	}
}

func normalizeStatus(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if raw == "" || !domain.ValidContactStatus(raw) {
		return ""
	}
	return raw
}

func (s *Server) handleListContacts(w http.ResponseWriter, r *http.Request) {
	filter := s.contactFilterFrom(r)

	items, total, err := s.deps.Contacts.List(r.Context(), filter)
	if err != nil {
		httpx.Internal(w, s.log, err, "list contacts")
		return
	}
	if items == nil {
		items = []domain.Contact{}
	}
	httpx.Paged(w, items, total, filter.Limit, filter.Offset)
}

func (s *Server) handleGetContact(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	contact, err := s.deps.Contacts.GetByID(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "get contact")
		return
	}
	if contact == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Байланыс табылмады")
		return
	}

	tags, err := s.deps.Contacts.TagsFor(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "contact tags")
		return
	}
	contact.Tags = tags

	enrollments, err := s.deps.Campaigns.Repo().ListEnrollmentsForContact(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "contact enrollments")
		return
	}

	jobs, _, err := s.deps.Jobs.List(r.Context(), schedulerFilterForContact(id))
	if err != nil {
		httpx.Internal(w, s.log, err, "contact jobs")
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"contact":     contact,
		"enrollments": enrollments,
		"scheduled":   jobs,
	})
}

type updateContactRequest struct {
	Name  string `json:"name"`
	Notes string `json:"notes"`
}

func (s *Server) handleUpdateContact(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req updateContactRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	name := textnorm.NormalizeName(req.Name)
	if len([]rune(name)) > 200 {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Аты тым ұзын")
		return
	}
	notes := strings.TrimSpace(req.Notes)
	if len([]rune(notes)) > 5000 {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Ескертпе тым ұзын")
		return
	}

	if err := s.deps.Contacts.UpdateProfile(r.Context(), id, name, notes); err != nil {
		httpx.Internal(w, s.log, err, "update contact")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionContactUpdated,
		EntityType: "contact",
		EntityID:   id.String(),
		Summary:    "Байланыс деректері жаңартылды",
		New:        map[string]string{"name": name},
	})

	contact, err := s.deps.Contacts.GetByID(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "reload contact")
		return
	}
	httpx.JSON(w, http.StatusOK, contact)
}

func (s *Server) handleDeleteContact(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Contacts.Delete(r.Context(), id); err != nil {
		httpx.Internal(w, s.log, err, "delete contact")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionContactDeleted,
		EntityType: "contact",
		EntityID:   id.String(),
		Summary:    "Байланыс жойылды",
	})

	httpx.NoContent(w)
}

type blockRequest struct {
	Blocked bool `json:"blocked"`
}

func (s *Server) handleBlockContact(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req blockRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Contacts.SetBlocked(r.Context(), id, req.Blocked); err != nil {
		httpx.Internal(w, s.log, err, "block contact")
		return
	}

	if req.Blocked {
		// Blocking must also stop anything already queued for this contact.
		if err := s.deps.Campaigns.StopForContact(r.Context(), id,
			domain.EnrollmentCancelled, "contact blocked by administrator"); err != nil {
			httpx.Internal(w, s.log, err, "stop enrollments")
			return
		}
	}

	action := audit.ActionContactUnblocked
	summary := "Байланыс бұғаттаудан шығарылды"
	if req.Blocked {
		action = audit.ActionContactBlocked
		summary = "Байланыс бұғатталды"
	}
	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action: action, EntityType: "contact", EntityID: id.String(), Summary: summary,
	})

	httpx.JSON(w, http.StatusOK, map[string]bool{"blocked": req.Blocked})
}

type unsubscribeRequest struct {
	OptedOut bool `json:"opted_out"`
}

func (s *Server) handleUnsubscribeContact(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req unsubscribeRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Contacts.SetOptOut(r.Context(), nil, id, req.OptedOut); err != nil {
		httpx.Internal(w, s.log, err, "set opt out")
		return
	}
	if req.OptedOut {
		if err := s.deps.Campaigns.StopForContact(r.Context(), id,
			domain.EnrollmentUnsubscribed, "unsubscribed by administrator"); err != nil {
			httpx.Internal(w, s.log, err, "stop enrollments")
			return
		}
	}

	action := audit.ActionContactResubscribed
	summary := "Жазылым қалпына келтірілді"
	if req.OptedOut {
		action = audit.ActionContactUnsubscribed
		summary = "Жазылымнан шығарылды"
	}
	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action: action, EntityType: "contact", EntityID: id.String(), Summary: summary,
	})

	httpx.JSON(w, http.StatusOK, map[string]bool{"opted_out": req.OptedOut})
}

func (s *Server) handleRefreshProfile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	contact, err := s.deps.Contacts.GetByID(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "get contact")
		return
	}
	if contact == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Байланыс табылмады")
		return
	}

	avatarURL, err := s.deps.Enricher.RefreshContactProfile(r.Context(), contact)
	if err != nil {
		httpx.Fail(w, http.StatusBadGateway, httpx.CodeUnavailable,
			"Профильді жаңарту мүмкін болмады: "+err.Error())
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]string{"avatar_url": avatarURL})
}

// ------------------------------------------------------------ bulk actions --

type bulkRequest struct {
	ContactIDs []string `json:"contact_ids"`
	Action     string   `json:"action"`
	TagName    string   `json:"tag_name,omitempty"`
	TagColor   string   `json:"tag_color,omitempty"`
}

// handleBulkAction applies an administrative action to many contacts.
//
// Bulk messaging is intentionally absent: the only outbound paths are campaign
// automation and a one-to-one manual reply, both of which enforce consent.
func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	var req bulkRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if len(req.ContactIDs) == 0 {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Байланыстар таңдалмаған")
		return
	}
	if len(req.ContactIDs) > 5000 {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Бір әрекетте 5000-нан көп байланыс болмауы керек")
		return
	}

	ids := make([]uuid.UUID, 0, len(req.ContactIDs))
	for _, raw := range req.ContactIDs {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Байланыс идентификаторы жарамсыз: "+raw)
			return
		}
		ids = append(ids, id)
	}

	var (
		affected int64
		err      error
		summary  string
	)

	switch strings.ToLower(strings.TrimSpace(req.Action)) {
	case "unsubscribe":
		affected, err = s.deps.Contacts.BulkSetOptOut(r.Context(), ids, true)
		summary = "Топтық жазылымнан шығару"
		if err == nil {
			for _, id := range ids {
				if stopErr := s.deps.Campaigns.StopForContact(r.Context(), id,
					domain.EnrollmentUnsubscribed, "bulk unsubscribe"); stopErr != nil {
					s.log.Warn("bulk unsubscribe: stopping enrollments failed",
						"contact_id", id, "error", stopErr)
				}
			}
		}

	case "resubscribe":
		affected, err = s.deps.Contacts.BulkSetOptOut(r.Context(), ids, false)
		summary = "Топтық жазылымды қалпына келтіру"

	case "block":
		affected, err = s.deps.Contacts.BulkSetBlocked(r.Context(), ids, true)
		summary = "Топтық бұғаттау"
		if err == nil {
			for _, id := range ids {
				if stopErr := s.deps.Campaigns.StopForContact(r.Context(), id,
					domain.EnrollmentCancelled, "bulk block"); stopErr != nil {
					s.log.Warn("bulk block: stopping enrollments failed",
						"contact_id", id, "error", stopErr)
				}
			}
		}

	case "unblock":
		affected, err = s.deps.Contacts.BulkSetBlocked(r.Context(), ids, false)
		summary = "Топтық бұғаттаудан шығару"

	case "remove_from_automation":
		summary = "Автоматтандырудан шығару"
		for _, id := range ids {
			if stopErr := s.deps.Campaigns.StopForContact(r.Context(), id,
				domain.EnrollmentCancelled, "removed from automation"); stopErr != nil {
				err = stopErr
				break
			}
			affected++
		}

	case "tag":
		if strings.TrimSpace(req.TagName) == "" {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Тег атауы қажет")
			return
		}
		color := req.TagColor
		if color == "" {
			color = "#4b5563"
		}
		var tag *domain.Tag
		tag, err = s.deps.Contacts.EnsureTag(r.Context(), req.TagName, color)
		if err == nil {
			affected, err = s.deps.Contacts.AttachTag(r.Context(), ids, tag.ID)
		}
		summary = "Топтық тег: " + req.TagName

	default:
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Белгісіз әрекет: "+req.Action)
		return
	}

	if err != nil {
		httpx.Internal(w, s.log, err, "bulk action")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionBulkAction,
		EntityType: "contact",
		Summary:    summary,
		New:        map[string]any{"action": req.Action, "count": len(ids), "affected": affected},
	})

	httpx.JSON(w, http.StatusOK, map[string]any{
		"affected": affected,
		"action":   req.Action,
	})
}

// ---------------------------------------------------------------- tags --

func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.deps.Contacts.ListTags(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "list tags")
		return
	}
	if tags == nil {
		tags = []domain.Tag{}
	}
	httpx.JSON(w, http.StatusOK, tags)
}

// displayPhone is used by handlers that echo a contact back to the UI.
func displayPhone(digits string) string { return phone.Display(digits) }
