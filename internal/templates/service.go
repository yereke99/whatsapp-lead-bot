package templates

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/pkg/render"
)

// TemplateVersion is one entry of a template's revision history.
type TemplateVersion struct {
	Version   int       `json:"version"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Body      string    `json:"body"`
	FileName  string    `json:"file_name"`
	CreatedAt time.Time `json:"created_at"`
	Author    string    `json:"author"`
}

// Input is the validated payload for creating or updating a template.
type Input struct {
	Name        string
	Description string
	Type        string
	Body        string
	MediaFileID *uuid.UUID
	FileName    string
	LinkPreview bool
}

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (v ValidationError) Error() string { return v.Field + ": " + v.Message }

// ValidationErrors carries every problem found, so the form can highlight all
// of them at once instead of one per round trip.
type ValidationErrors []ValidationError

func (v ValidationErrors) Error() string {
	parts := make([]string, 0, len(v))
	for _, e := range v {
		parts = append(parts, e.Error())
	}
	return strings.Join(parts, "; ")
}

type Service struct {
	repo  *Repository
	media *media.Service
}

func NewService(repo *Repository, mediaSvc *media.Service) *Service {
	return &Service{repo: repo, media: mediaSvc}
}

func (s *Service) List(ctx context.Context, search, typeFilter string, includeArchived bool) ([]domain.MessageTemplate, error) {
	items, err := s.repo.List(ctx, search, typeFilter, includeArchived)
	if err != nil {
		return nil, err
	}
	s.decorate(items)
	return items, nil
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*domain.MessageTemplate, error) {
	tpl, err := s.repo.GetByID(ctx, nil, id)
	if err != nil || tpl == nil {
		return nil, err
	}
	one := []domain.MessageTemplate{*tpl}
	s.decorate(one)
	return &one[0], nil
}

func (s *Service) decorate(items []domain.MessageTemplate) {
	for i := range items {
		if items[i].Media != nil {
			items[i].Media.URL = s.media.Store().PublicURL(items[i].Media.ID)
		}
	}
}

// Validate checks an input against the template type's requirements.
//
// The rules mirror the database CHECK constraints so the operator gets a
// readable message instead of a constraint violation.
func (s *Service) Validate(ctx context.Context, in Input) error {
	var problems ValidationErrors

	if strings.TrimSpace(in.Name) == "" {
		problems = append(problems, ValidationError{"name", "Атауы міндетті"})
	}
	if len(in.Name) > 200 {
		problems = append(problems, ValidationError{"name", "Атауы 200 таңбадан аспауы керек"})
	}
	if !domain.ValidTemplateType(in.Type) {
		problems = append(problems, ValidationError{"type", "Хабарлама түрі жарамсыз"})
		return problems
	}

	tplType := domain.TemplateType(in.Type)
	body := strings.TrimSpace(in.Body)

	if tplType.RequiresMedia() && in.MediaFileID == nil {
		problems = append(problems, ValidationError{"media_file_id", "Бұл түр үшін файл жүктеу қажет"})
	}
	if !tplType.RequiresMedia() && in.MediaFileID != nil {
		problems = append(problems, ValidationError{"media_file_id", "Мәтіндік хабарламаға файл тіркелмейді"})
	}
	if tplType == domain.TemplateText && body == "" {
		problems = append(problems, ValidationError{"body", "Мәтін бос болмауы керек"})
	}
	if !tplType.AllowsCaption() && body != "" {
		problems = append(problems, ValidationError{"body", "Бұл түрде мәтін қолданылмайды"})
	}
	// WhatsApp truncates captions well before this, but the limit keeps a
	// runaway paste out of the provider request.
	if len([]rune(body)) > 4000 {
		problems = append(problems, ValidationError{"body", "Мәтін 4000 таңбадан аспауы керек"})
	}

	if in.MediaFileID != nil {
		file, err := s.media.Get(ctx, *in.MediaFileID)
		if err != nil {
			return err
		}
		if file == nil {
			problems = append(problems, ValidationError{"media_file_id", "Файл табылмады"})
		} else if want := tplType.MediaKind(); want != "" && file.Kind != want {
			problems = append(problems, ValidationError{
				"media_file_id",
				fmt.Sprintf("Файл түрі сәйкес емес: %s қажет, %s жүктелген", want, file.Kind),
			})
		}
	}

	if len(problems) > 0 {
		return problems
	}
	return nil
}

func (s *Service) Create(ctx context.Context, in Input, adminID *uuid.UUID) (*domain.MessageTemplate, error) {
	if err := s.Validate(ctx, in); err != nil {
		return nil, err
	}

	tpl := &domain.MessageTemplate{
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		Type:        domain.TemplateType(in.Type),
		Body:        strings.TrimSpace(in.Body),
		MediaFileID: in.MediaFileID,
		FileName:    strings.TrimSpace(in.FileName),
		LinkPreview: in.LinkPreview,
		CreatedBy:   adminID,
	}

	if err := s.repo.Create(ctx, tpl); err != nil {
		return nil, err
	}
	return s.Get(ctx, tpl.ID)
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input, adminID *uuid.UUID) (*domain.MessageTemplate, error) {
	existing, err := s.repo.GetByID(ctx, nil, id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, ErrNotFound
	}
	if err := s.Validate(ctx, in); err != nil {
		return nil, err
	}

	tpl := &domain.MessageTemplate{
		ID:          id,
		Name:        strings.TrimSpace(in.Name),
		Description: strings.TrimSpace(in.Description),
		Type:        domain.TemplateType(in.Type),
		Body:        strings.TrimSpace(in.Body),
		MediaFileID: in.MediaFileID,
		FileName:    strings.TrimSpace(in.FileName),
		LinkPreview: in.LinkPreview,
		CreatedBy:   adminID,
	}

	if err := s.repo.Update(ctx, tpl); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Duplicate copies a template under a new, unique name.
func (s *Service) Duplicate(ctx context.Context, id uuid.UUID, adminID *uuid.UUID) (*domain.MessageTemplate, error) {
	src, err := s.repo.GetByID(ctx, nil, id)
	if err != nil {
		return nil, err
	}
	if src == nil {
		return nil, ErrNotFound
	}

	base := src.Name + " (көшірме)"
	name := base
	for attempt := 2; attempt <= 50; attempt++ {
		tpl, err := s.Create(ctx, Input{
			Name:        name,
			Description: src.Description,
			Type:        string(src.Type),
			Body:        src.Body,
			MediaFileID: src.MediaFileID,
			FileName:    src.FileName,
			LinkPreview: src.LinkPreview,
		}, adminID)
		if err == nil {
			return tpl, nil
		}
		if !errors.Is(err, ErrNameTaken) {
			return nil, err
		}
		name = fmt.Sprintf("%s %d", base, attempt)
	}
	return nil, ErrNameTaken
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.repo.Delete(ctx, id)
}

func (s *Service) Versions(ctx context.Context, id uuid.UUID, limit int) ([]TemplateVersion, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	return s.repo.Versions(ctx, id, limit)
}

// Preview renders a template body with sample values so the operator sees the
// message roughly as the recipient will.
type Preview struct {
	Type             domain.TemplateType `json:"type"`
	RenderedText     string              `json:"rendered_text"`
	MediaURL         string              `json:"media_url,omitempty"`
	MediaKind        domain.MediaKind    `json:"media_kind,omitempty"`
	MediaName        string              `json:"media_name,omitempty"`
	UnknownVariables []string            `json:"unknown_variables,omitempty"`
}

func (s *Service) Preview(ctx context.Context, id uuid.UUID, values map[string]string) (*Preview, error) {
	tpl, err := s.Get(ctx, id)
	if err != nil || tpl == nil {
		return nil, err
	}
	return s.PreviewContent(tpl, values), nil
}

func (s *Service) PreviewContent(tpl *domain.MessageTemplate, values map[string]string) *Preview {
	merged := sampleValues()
	for k, v := range values {
		if strings.TrimSpace(v) != "" {
			merged[k] = v
		}
	}

	preview := &Preview{
		Type:             tpl.Type,
		RenderedText:     render.Render(tpl.Body, render.NewContext(merged)),
		UnknownVariables: render.UnknownVariables(tpl.Body),
	}
	if tpl.Media != nil {
		preview.MediaURL = tpl.Media.URL
		preview.MediaKind = tpl.Media.Kind
		preview.MediaName = tpl.Media.OriginalName
	}
	return preview
}

func sampleValues() map[string]string {
	return map[string]string{
		"contact_name":     "Әлішер Сәрсенов",
		"first_name":       "Әлішер",
		"phone":            "+7 700 123 45 67",
		"campaign_name":    "Түрік айраны вебинары",
		"webinar_date":     "15.08.2026",
		"webinar_time":     "21:00",
		"webinar_datetime": "15.08.2026 21:00",
		"webinar_link":     "https://example.com/webinar",
		"remaining_time":   "2 сағат 30 минут",
		"timezone":         "Asia/Almaty",
	}
}

// Variables exposes the placeholder catalog to the template editor.
func Variables() []render.Variable { return render.Catalog }
