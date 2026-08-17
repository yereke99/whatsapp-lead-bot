package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/audit"
	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/httpx"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	filter := campaigns.ListFilter{
		Search:          httpx.QueryString(r, "search"),
		Status:          strings.ToUpper(httpx.QueryString(r, "status")),
		IncludeArchived: httpx.QueryBool(r, "include_archived"),
	}
	if filter.Status != "" && !domain.ValidCampaignStatus(filter.Status) {
		filter.Status = ""
	}

	items, err := s.deps.Campaigns.Repo().List(r.Context(), filter)
	if err != nil {
		httpx.Internal(w, s.log, err, "list campaigns")
		return
	}
	if items == nil {
		items = []domain.Campaign{}
	}
	httpx.JSON(w, http.StatusOK, items)
}

func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	campaign, err := s.deps.Campaigns.Repo().GetFull(r.Context(), id)
	if err != nil {
		httpx.Internal(w, s.log, err, "get campaign")
		return
	}
	if campaign == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		return
	}
	httpx.JSON(w, http.StatusOK, campaign)
}

type campaignRequest struct {
	Name                    string   `json:"name"`
	Description             string   `json:"description"`
	EventType               string   `json:"event_type"`
	EventDate               string   `json:"event_date"`
	EventTime               string   `json:"event_time"`
	Timezone                string   `json:"timezone"`
	WebinarLink             string   `json:"webinar_link"`
	ExistingContactBehavior string   `json:"existing_contact_behavior"`
	ExistingContactTemplate string   `json:"existing_contact_template_id"`
	UnsubscribeKeywords     []string `json:"unsubscribe_keywords"`
	CatchUpMissedSteps      bool     `json:"catch_up_missed_steps"`
	MaxSendAttempts         int      `json:"max_send_attempts"`
	ResumePolicy            string   `json:"resume_policy"`
	PinTemplateVersion      bool     `json:"pin_template_version"`
	// Nil means "no limit", which is why these are pointers rather than a
	// zero value that would be indistinguishable from "allow nothing".
	MaxMessagesPerHour *int `json:"max_messages_per_hour"`
	MaxMessagesPerDay  *int `json:"max_messages_per_day"`
	MaxActiveContacts  *int `json:"max_active_contacts"`
}

func (req campaignRequest) toInput() (campaigns.SaveInput, error) {
	in := campaigns.SaveInput{
		Name:                    req.Name,
		Description:             req.Description,
		EventType:               req.EventType,
		EventDate:               req.EventDate,
		EventTime:               req.EventTime,
		Timezone:                req.Timezone,
		WebinarLink:             req.WebinarLink,
		ExistingContactBehavior: req.ExistingContactBehavior,
		UnsubscribeKeywords:     req.UnsubscribeKeywords,
		CatchUpMissedSteps:      req.CatchUpMissedSteps,
		MaxSendAttempts:         req.MaxSendAttempts,
		ResumePolicy:            req.ResumePolicy,
		PinTemplateVersion:      req.PinTemplateVersion,
		MaxMessagesPerHour:      req.MaxMessagesPerHour,
		MaxMessagesPerDay:       req.MaxMessagesPerDay,
		MaxActiveContacts:       req.MaxActiveContacts,
	}

	if strings.TrimSpace(req.ExistingContactTemplate) != "" {
		id, err := uuid.Parse(strings.TrimSpace(req.ExistingContactTemplate))
		if err != nil {
			return in, errors.New("шаблон идентификаторы жарамсыз")
		}
		in.ExistingContactTemplate = &id
	}

	if link := strings.TrimSpace(req.WebinarLink); link != "" {
		if !strings.HasPrefix(link, "http://") && !strings.HasPrefix(link, "https://") {
			return in, errors.New("сілтеме http:// немесе https:// деп басталуы керек")
		}
	}
	return in, nil
}

func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	var req campaignRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	in, err := req.toInput()
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	campaign, err := s.deps.Campaigns.Create(r.Context(), in, adminID(r))
	if err != nil {
		if errors.Is(err, campaigns.ErrNameTaken) {
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Бұл атаумен кампания бар")
			return
		}
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionCampaignCreated,
		EntityType: "campaign",
		EntityID:   campaign.ID.String(),
		Summary:    "Кампания құрылды: " + campaign.Name,
		New:        map[string]any{"name": campaign.Name, "timezone": campaign.Timezone},
	})

	httpx.JSON(w, http.StatusCreated, campaign)
}

func (s *Server) handleUpdateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req campaignRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	in, err := req.toInput()
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	before, err := s.deps.Campaigns.Repo().GetByID(r.Context(), nil, id)
	if err != nil {
		httpx.Internal(w, s.log, err, "load campaign")
		return
	}
	if before == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		return
	}

	result, err := s.deps.Campaigns.Update(r.Context(), id, in)
	if err != nil {
		switch {
		case errors.Is(err, campaigns.ErrNotFound):
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		case errors.Is(err, campaigns.ErrNameTaken):
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict, "Бұл атаумен кампания бар")
		default:
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		}
		return
	}

	action := audit.ActionCampaignUpdated
	summary := "Кампания жаңартылды: " + result.Campaign.Name
	if result.TimeChanged {
		action = audit.ActionEventTimeChanged
		summary = fmt.Sprintf("Іс-шара уақыты өзгертілді (%d жоспарланған хабарлама қайта есептелді)",
			result.Rescheduled)
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     action,
		EntityType: "campaign",
		EntityID:   id.String(),
		Summary:    summary,
		Old: map[string]any{
			"event_start_at": formatEvent(before),
			"name":           before.Name,
		},
		New: map[string]any{
			"event_start_at":   formatEvent(result.Campaign),
			"name":             result.Campaign.Name,
			"rescheduled_jobs": result.Rescheduled,
		},
	})

	httpx.JSON(w, http.StatusOK, result)
}

func formatEvent(c *domain.Campaign) string {
	if c == nil || c.EventStartAt == nil {
		return ""
	}
	return timex.FormatIn(*c.EventStartAt, c.Timezone, "2006-01-02 15:04") + " " + c.Timezone
}

type statusRequest struct {
	Status string `json:"status"`
}

func (s *Server) handleCampaignStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req statusRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	status := domain.CampaignStatus(strings.ToUpper(strings.TrimSpace(req.Status)))
	if status == domain.CampaignArchived {
		if err := s.deps.Campaigns.Archive(r.Context(), id); err != nil {
			httpx.Internal(w, s.log, err, "archive campaign")
			return
		}
		s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
			Action:     audit.ActionCampaignArchived,
			EntityType: "campaign",
			EntityID:   id.String(),
			Summary:    "Кампания мұрағатталды",
		})
		httpx.JSON(w, http.StatusOK, map[string]string{"status": string(status)})
		return
	}

	campaign, err := s.deps.Campaigns.SetStatus(r.Context(), id, status)
	if err != nil {
		var invalid campaigns.ValidationError
		switch {
		case errors.Is(err, campaigns.ErrNotFound):
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		case errors.As(err, &invalid):
			// Every blocking problem at once, so the operator fixes the
			// campaign in one pass instead of one error per attempt.
			httpx.FailWithDetails(w, http.StatusBadRequest, httpx.CodeValidation,
				"Кампанияны іске қосу мүмкін емес", map[string]any{"problems": invalid.Problems})
		default:
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		}
		return
	}

	action := audit.ActionCampaignUpdated
	switch status {
	case domain.CampaignActive:
		action = audit.ActionCampaignActivated
	case domain.CampaignPaused:
		action = audit.ActionCampaignPaused
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     action,
		EntityType: "campaign",
		EntityID:   id.String(),
		Summary:    "Кампания мәртебесі: " + string(status),
		New:        map[string]string{"status": string(status)},
	})

	httpx.JSON(w, http.StatusOK, campaign)
}

func (s *Server) handleDuplicateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	campaign, err := s.deps.Campaigns.Duplicate(r.Context(), id, adminID(r))
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "duplicate campaign")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionCampaignDuplicated,
		EntityType: "campaign",
		EntityID:   campaign.ID.String(),
		Summary:    "Кампания көшірілді: " + campaign.Name,
	})

	httpx.JSON(w, http.StatusCreated, campaign)
}

func (s *Server) handleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Campaigns.Delete(r.Context(), id); err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "delete campaign")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionCampaignDeleted,
		EntityType: "campaign",
		EntityID:   id.String(),
		Summary:    "Кампания жойылды",
	})

	httpx.NoContent(w)
}

func (s *Server) handleCampaignPreview(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	entries, err := s.deps.Campaigns.Preview(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "campaign preview")
		return
	}
	if entries == nil {
		entries = []campaigns.PreviewEntry{}
	}
	httpx.JSON(w, http.StatusOK, entries)
}

// handleCampaignValidate reports everything standing between a campaign and
// activation, so the panel can show the checklist while it is still a draft
// rather than only when the operator presses the button.
func (s *Server) handleCampaignValidate(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	campaign, problems, err := s.deps.Campaigns.Validate(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "validate campaign")
		return
	}
	if problems == nil {
		problems = []campaigns.Problem{}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"can_activate": len(campaigns.Blocking(problems)) == 0,
		"status":       campaign.Status,
		"problems":     problems,
	})
}

// handleReconcileCampaign runs reconciliation for one campaign on demand.
//
// The scheduler does this on a timer already, so the button is not what makes
// the system correct. It is here because an operator who has just fixed a
// campaign should not have to wait out an interval to see the queue agree with
// it, and because the counters it returns answer "did that actually change
// anything?" — which is otherwise only visible in the logs.
func (s *Server) handleReconcileCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	stats, err := s.deps.Campaigns.ReconcileCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "reconcile campaign")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionCampaignReconciled,
		EntityType: "campaign",
		EntityID:   id.String(),
		Summary: fmt.Sprintf("Кезек тексерілді: %d жазылым, %d жаңа тапсырма, %d жылжытылды",
			stats.EnrollmentsChecked, stats.JobsCreated, stats.JobsMoved),
	})

	httpx.JSON(w, http.StatusOK, stats)
}

// handleConsistency reports whether the queue still matches the campaigns that
// define it, without changing anything.
func (s *Server) handleConsistency(w http.ResponseWriter, r *http.Request) {
	report, err := s.deps.Campaigns.Consistency(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "run consistency checks")
		return
	}
	httpx.JSON(w, http.StatusOK, report)
}

// handleCampaignScheduled lists the real queued messages for one campaign, as
// opposed to the plan: what is actually waiting, for whom, and at what time.
func (s *Server) handleCampaignScheduled(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	filter := scheduler.ListFilter{
		CampaignID: &id,
		Status:     strings.ToUpper(httpx.QueryString(r, "status")),
		Limit:      httpx.QueryInt(r, "limit", 50, 1, 200),
		Offset:     httpx.QueryInt(r, "offset", 0, 0, 100000),
	}

	items, total, err := s.deps.Jobs.List(r.Context(), filter)
	if err != nil {
		httpx.Internal(w, s.log, err, "list campaign scheduled messages")
		return
	}
	if items == nil {
		items = []domain.ScheduledMessage{}
	}

	httpx.Paged(w, items, total, filter.Limit, filter.Offset)
}

// ----------------------------------------------------------------- steps --

func (s *Server) handleListSteps(w http.ResponseWriter, r *http.Request) {
	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	steps, err := s.deps.Campaigns.Repo().ListSteps(r.Context(), nil, id)
	if err != nil {
		httpx.Internal(w, s.log, err, "list steps")
		return
	}
	if steps == nil {
		steps = []domain.CampaignStep{}
	}
	httpx.JSON(w, http.StatusOK, steps)
}

// stepRequest accepts a step in either of the two ways the panel offers it.
//
// For an event-anchored step the operator picks a wall-clock date and time
// ("18:00 on 16.08.2026"); the panel may send that directly as scheduled_date
// and scheduled_time, and the server converts it into the signed offset from
// the campaign's event start that the queue actually stores. Sending
// offset_seconds instead is equally valid and is what the drag-free editor
// does. Storing the offset rather than the timestamp is what lets moving a
// webinar carry its whole queue with it.
//
// For a trigger-anchored step offset_seconds is the delay after the customer
// wrote in, and the date and time fields are ignored.
type stepRequest struct {
	Name          string `json:"name"`
	OffsetSeconds int    `json:"offset_seconds"`
	ScheduledDate string `json:"scheduled_date"`
	ScheduledTime string `json:"scheduled_time"`
	TemplateID    string `json:"message_template_id"`
	Enabled       bool   `json:"enabled"`
	ScheduleKind  string `json:"schedule_kind"`
}

func (req stepRequest) toInput(campaign *domain.Campaign) (campaigns.StepInput, error) {
	in := campaigns.StepInput{
		Name:          req.Name,
		OffsetSeconds: req.OffsetSeconds,
		Enabled:       req.Enabled,
		ScheduleKind:  req.ScheduleKind,
	}

	id, err := uuid.Parse(strings.TrimSpace(req.TemplateID))
	if err != nil {
		return in, errors.New("шаблон таңдалуы керек")
	}
	in.TemplateID = id

	kind := strings.TrimSpace(req.ScheduleKind)
	if kind == "" || kind == string(domain.ScheduleRelativeToEvent) {
		if date := strings.TrimSpace(req.ScheduledDate); date != "" {
			if campaign == nil {
				return in, errors.New("кампания табылмады")
			}
			if campaign.EventStartAt == nil {
				return in, errors.New("нақты уақыт қою үшін алдымен іс-шара күні мен уақытын белгілеңіз")
			}
			at, err := timex.ParseInLocation(date, req.ScheduledTime, campaign.Timezone)
			if err != nil {
				return in, err
			}
			in.OffsetSeconds = int(at.Sub(*campaign.EventStartAt).Round(time.Second) / time.Second)
		}
	}

	return in, nil
}

func (s *Server) handleCreateStep(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	campaignID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req stepRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	campaign, err := s.deps.Campaigns.Repo().GetByID(r.Context(), nil, campaignID)
	if err != nil {
		httpx.Internal(w, s.log, err, "load campaign")
		return
	}
	if campaign == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		return
	}

	in, err := req.toInput(campaign)
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	step, err := s.deps.Campaigns.AddStep(r.Context(), campaignID, in)
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionStepCreated,
		EntityType: "campaign_step",
		EntityID:   step.ID.String(),
		Summary: fmt.Sprintf("Қадам қосылды (%s) кампанияға %s",
			timex.HumanOffset(step.OffsetSeconds), campaignID),
		New: map[string]any{"offset_seconds": step.OffsetSeconds, "enabled": step.Enabled},
	})

	httpx.JSON(w, http.StatusCreated, step)
}

func (s *Server) handleUpdateStep(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	campaignID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	stepID, err := httpx.PathUUID(r, "stepId")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req stepRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	campaign, err := s.deps.Campaigns.Repo().GetByID(r.Context(), nil, campaignID)
	if err != nil {
		httpx.Internal(w, s.log, err, "load campaign")
		return
	}
	if campaign == nil {
		httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Кампания табылмады")
		return
	}

	in, err := req.toInput(campaign)
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	step, err := s.deps.Campaigns.UpdateStep(r.Context(), stepID, in)
	if err != nil {
		if errors.Is(err, campaigns.ErrStepNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Қадам табылмады")
			return
		}
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionStepUpdated,
		EntityType: "campaign_step",
		EntityID:   stepID.String(),
		Summary:    "Қадам жаңартылды: " + timex.HumanOffset(step.OffsetSeconds),
		New:        map[string]any{"offset_seconds": step.OffsetSeconds, "enabled": step.Enabled},
	})

	httpx.JSON(w, http.StatusOK, step)
}

func (s *Server) handleDeleteStep(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	stepID, err := httpx.PathUUID(r, "stepId")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Campaigns.DeleteStep(r.Context(), stepID); err != nil {
		if errors.Is(err, campaigns.ErrStepNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Қадам табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "delete step")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionStepDeleted,
		EntityType: "campaign_step",
		EntityID:   stepID.String(),
		Summary:    "Қадам жойылды",
	})

	httpx.NoContent(w)
}

type reorderRequest struct {
	StepIDs []string `json:"step_ids"`
}

func (s *Server) handleReorderSteps(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	campaignID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req reorderRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}
	if len(req.StepIDs) == 0 {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Қадамдар тізімі бос")
		return
	}

	ordered := make([]uuid.UUID, 0, len(req.StepIDs))
	for _, raw := range req.StepIDs {
		id, parseErr := uuid.Parse(strings.TrimSpace(raw))
		if parseErr != nil {
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, "Қадам идентификаторы жарамсыз")
			return
		}
		ordered = append(ordered, id)
	}

	if err := s.deps.Campaigns.ReorderSteps(r.Context(), campaignID, ordered); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionStepReordered,
		EntityType: "campaign",
		EntityID:   campaignID.String(),
		Summary:    fmt.Sprintf("Қадамдар реті өзгертілді (%d қадам)", len(ordered)),
	})

	steps, err := s.deps.Campaigns.Repo().ListSteps(r.Context(), nil, campaignID)
	if err != nil {
		httpx.Internal(w, s.log, err, "list steps")
		return
	}
	httpx.JSON(w, http.StatusOK, steps)
}

// -------------------------------------------------------------- triggers --

func (s *Server) handleListTriggers(w http.ResponseWriter, r *http.Request) {
	triggers, err := s.deps.Campaigns.Repo().AllTriggers(r.Context())
	if err != nil {
		httpx.Internal(w, s.log, err, "list triggers")
		return
	}
	if triggers == nil {
		triggers = []domain.CampaignTrigger{}
	}
	httpx.JSON(w, http.StatusOK, triggers)
}

type triggerRequest struct {
	Keyword   string `json:"keyword"`
	MatchMode string `json:"match_mode"`
	IsActive  bool   `json:"is_active"`
}

func (s *Server) handleCreateTrigger(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	campaignID, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req triggerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	trigger, err := s.deps.Campaigns.AddTrigger(r.Context(), campaignID, req.Keyword, req.MatchMode, req.IsActive)
	if err != nil {
		if errors.Is(err, campaigns.ErrTriggerTaken) {
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict,
				"Бұл триггер басқа белсенді кампанияда қолданылып жатыр")
			return
		}
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTriggerCreated,
		EntityType: "trigger",
		EntityID:   trigger.ID.String(),
		Summary:    "Триггер қосылды: " + trigger.Keyword,
		New:        map[string]any{"keyword": trigger.Keyword, "match_mode": trigger.MatchMode},
	})

	httpx.JSON(w, http.StatusCreated, trigger)
}

func (s *Server) handleUpdateTrigger(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	var req triggerRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	trigger, err := s.deps.Campaigns.UpdateTrigger(r.Context(), id, req.Keyword, req.MatchMode, req.IsActive)
	if err != nil {
		switch {
		case errors.Is(err, campaigns.ErrNotFound):
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Триггер табылмады")
		case errors.Is(err, campaigns.ErrTriggerTaken):
			httpx.Fail(w, http.StatusConflict, httpx.CodeConflict,
				"Бұл триггер басқа белсенді кампанияда қолданылып жатыр")
		default:
			httpx.Fail(w, http.StatusBadRequest, httpx.CodeValidation, err.Error())
		}
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTriggerUpdated,
		EntityType: "trigger",
		EntityID:   id.String(),
		Summary:    "Триггер жаңартылды: " + trigger.Keyword,
	})

	httpx.JSON(w, http.StatusOK, trigger)
}

func (s *Server) handleDeleteTrigger(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriter(w, r) {
		return
	}

	id, err := httpx.PathUUID(r, "id")
	if err != nil {
		httpx.Fail(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	}

	if err := s.deps.Campaigns.DeleteTrigger(r.Context(), id); err != nil {
		if errors.Is(err, campaigns.ErrNotFound) {
			httpx.Fail(w, http.StatusNotFound, httpx.CodeNotFound, "Триггер табылмады")
			return
		}
		httpx.Internal(w, s.log, err, "delete trigger")
		return
	}

	s.deps.Audit.Record(r.Context(), s.actorFrom(r), audit.Entry{
		Action:     audit.ActionTriggerDeleted,
		EntityType: "trigger",
		EntityID:   id.String(),
		Summary:    "Триггер жойылды",
	})

	httpx.NoContent(w)
}
