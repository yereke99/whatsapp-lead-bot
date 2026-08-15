package inbound

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/messaging"
	"github.com/ayran/whatsapp-automation/internal/templates"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
	"github.com/ayran/whatsapp-automation/pkg/phone"
	"github.com/ayran/whatsapp-automation/pkg/render"
	"github.com/ayran/whatsapp-automation/pkg/textnorm"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

// Processor drains the stored notification queue and applies each event to
// the domain.
type Processor struct {
	cfg       config.Scheduler
	repo      *Repository
	contacts  *contacts.Repository
	messages  *conversations.Repository
	campaigns *campaigns.Service
	templates *templates.Repository
	sender    *messaging.Sender
	media     *media.Store
	log       *slog.Logger

	wg   sync.WaitGroup
	once sync.Once
}

func NewProcessor(
	cfg config.Scheduler,
	repo *Repository,
	contactRepo *contacts.Repository,
	messageRepo *conversations.Repository,
	campaignSvc *campaigns.Service,
	templateRepo *templates.Repository,
	sender *messaging.Sender,
	mediaStore *media.Store,
	log *slog.Logger,
) *Processor {
	return &Processor{
		cfg:       cfg,
		repo:      repo,
		contacts:  contactRepo,
		messages:  messageRepo,
		campaigns: campaignSvc,
		templates: templateRepo,
		sender:    sender,
		media:     mediaStore,
		log:       log.With(slog.String("component", "notifications")),
	}
}

// Ingest stores a raw notification and returns whether it was new.
//
// It does nothing else: parsing, contact creation and campaign enrollment all
// happen on the background workers. Keeping this to a single insert is what
// lets the poller acknowledge a notification immediately and move on, instead
// of holding the provider's queue open for the length of a campaign enrollment.
func (p *Processor) Ingest(ctx context.Context, body []byte) (accepted bool, err error) {
	event, err := greenapi.ParseWebhook(body)
	if err != nil {
		return false, err
	}

	_, inserted, err := p.repo.Insert(ctx, "greenapi", event.RawType, event.DedupeKey,
		event.ExternalID, body, event.Timestamp)
	if err != nil {
		return false, err
	}

	if !inserted {
		p.log.Debug("duplicate notification ignored",
			slog.String("type", event.RawType),
			slog.String("dedupe_key", event.DedupeKey))
		return false, nil
	}

	p.log.Info("notification stored",
		slog.String("type", event.RawType),
		slog.String("external_id", event.ExternalID))
	return true, nil
}

// Start launches the processing loops.
func (p *Processor) Start(ctx context.Context) {
	p.once.Do(func() {
		if released, err := p.repo.ReleaseStale(ctx, 5*time.Minute); err != nil {
			p.log.Error("startup recovery failed", slog.String("error", err.Error()))
		} else if released > 0 {
			p.log.Warn("requeued notifications from a previous run", slog.Int64("count", released))
		}

		workers := p.cfg.NotificationWorkers
		if workers < 1 {
			workers = 1
		}

		for i := 0; i < workers; i++ {
			p.wg.Add(1)
			go p.loop(ctx)
		}

		p.wg.Add(1)
		go p.maintenanceLoop(ctx)

		p.log.Info("notification processor started", slog.Int("workers", workers))
	})
}

func (p *Processor) Wait() { p.wg.Wait() }

func (p *Processor) loop(ctx context.Context) {
	defer p.wg.Done()

	// Inbound events are latency-sensitive: a customer's trigger should start their
	// automation within a second or two, not on a five-second tick.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.drain(ctx)
		}
	}
}

func (p *Processor) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		events, err := p.repo.Claim(ctx, p.cfg.NotificationBatchSize)
		if err != nil {
			p.log.Error("claiming notifications failed", slog.String("error", err.Error()))
			return
		}
		if len(events) == 0 {
			return
		}

		for _, event := range events {
			if ctx.Err() != nil {
				return
			}
			p.handle(ctx, event)
		}
	}
}

func (p *Processor) maintenanceLoop(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := p.repo.ReleaseStale(ctx, 5*time.Minute); err != nil {
				p.log.Error("releasing stale notifications failed", slog.String("error", err.Error()))
			}
			// Keep two weeks of processed events for debugging, then drop them.
			if pruned, err := p.repo.Prune(ctx, 14*24*time.Hour); err != nil {
				p.log.Error("pruning notifications failed", slog.String("error", err.Error()))
			} else if pruned > 0 {
				p.log.Info("pruned old notifications", slog.Int64("count", pruned))
			}
		}
	}
}

const maxWebhookAttempts = 5

func (p *Processor) handle(ctx context.Context, stored StoredEvent) {
	event, err := greenapi.ParseWebhook(stored.Payload)
	if err != nil {
		// A payload that cannot be parsed will never parse; do not retry it.
		p.finish(ctx, stored.ID, domain.WebhookIgnored, fmt.Sprintf("unparseable payload: %v", err))
		return
	}

	handlerErr := p.apply(ctx, event)
	if handlerErr == nil {
		p.finish(ctx, stored.ID, domain.WebhookProcessed, "")
		return
	}

	if stored.Attempts >= maxWebhookAttempts {
		p.log.Error("notification failed permanently",
			slog.String("type", event.RawType),
			slog.Int("attempts", stored.Attempts),
			slog.String("error", handlerErr.Error()))
		p.finish(ctx, stored.ID, domain.WebhookFailed, handlerErr.Error())
		return
	}

	p.log.Warn("notification will be retried",
		slog.String("type", event.RawType),
		slog.Int("attempts", stored.Attempts),
		slog.String("error", handlerErr.Error()))
	if err := p.repo.Requeue(ctx, stored.ID, handlerErr.Error()); err != nil {
		p.log.Error("requeueing notification failed", slog.String("error", err.Error()))
	}
}

func (p *Processor) finish(ctx context.Context, id uuid.UUID, status domain.WebhookEventStatus, errText string) {
	if err := p.repo.MarkProcessed(ctx, id, status, errText); err != nil {
		p.log.Error("recording notification outcome failed", slog.String("error", err.Error()))
	}
}

func (p *Processor) apply(ctx context.Context, event *whatsapp.Event) error {
	switch event.Kind {
	case whatsapp.EventIncomingMessage:
		return p.handleIncoming(ctx, event)
	case whatsapp.EventOutgoingMessage:
		return p.handleOutgoingEcho(ctx, event)
	case whatsapp.EventOutgoingStatus:
		return p.handleStatus(ctx, event)
	case whatsapp.EventStateChanged:
		p.log.Info("provider state changed", slog.String("state", event.State))
		return nil
	case whatsapp.EventIncomingCall:
		p.log.Info("incoming call ignored", slog.String("from", event.ChatID))
		return nil
	default:
		return nil
	}
}

// handleIncoming is the entry point of the whole funnel.
func (p *Processor) handleIncoming(ctx context.Context, event *whatsapp.Event) error {
	if event.Message == nil {
		return nil
	}
	// Group chats are out of scope: the platform addresses individuals.
	if phone.IsGroupChatID(event.ChatID) {
		p.log.Debug("group message ignored", slog.String("chat_id", event.ChatID))
		return nil
	}

	digits, err := phone.FromChatID(event.ChatID)
	if err != nil {
		p.log.Warn("unusable chat id",
			slog.String("chat_id", event.ChatID), slog.String("error", err.Error()))
		return nil
	}

	contact, created, err := p.contacts.UpsertFromInbound(ctx, nil, event.ChatID, digits, event.SenderName, event.Timestamp)
	if err != nil {
		return fmt.Errorf("upsert contact: %w", err)
	}
	if created {
		p.log.Info("contact created from inbound message",
			slog.String("contact_id", contact.ID.String()),
			slog.String("phone", digits))
	}

	message := &domain.Message{
		ContactID: contact.ID,
		Direction: domain.DirectionIncoming,
		Type:      string(event.Message.Type),
		Text:      event.Message.Text,
		MediaURL:  event.Message.DownloadURL,
		FileName:  event.Message.FileName,
		MimeType:  event.Message.MimeType,
		Status:    domain.MessageStatusReceived,
		CreatedAt: event.Timestamp,
	}
	if event.ExternalID != "" {
		id := event.ExternalID
		message.ExternalID = &id
	}
	if event.Message.Type.IsMedia() && event.Message.DownloadURL != "" {
		// The provider's download url is short-lived, so the attachment is
		// pulled into local storage by a background worker.
		message.MediaDownloadState = "PENDING"
	}

	if err := p.messages.Create(ctx, nil, message); err != nil {
		if errors.Is(err, conversations.ErrDuplicate) {
			// A replay that slipped past the event-level dedupe.
			p.log.Debug("duplicate inbound message ignored",
				slog.String("external_id", event.ExternalID))
			return nil
		}
		return fmt.Errorf("record inbound message: %w", err)
	}

	preview := messaging.Preview(event.Message.Type, event.Message.Text, event.Message.FileName)
	if err := p.contacts.RecordIncoming(ctx, nil, contact.ID, preview, string(event.Message.Type), event.Timestamp); err != nil {
		p.log.Warn("updating contact activity failed", slog.String("error", err.Error()))
	}

	// Only text carries intent the automation can act on.
	if event.Message.Type != whatsapp.TypeText || strings.TrimSpace(event.Message.Text) == "" {
		return nil
	}

	// Unsubscribe wins over everything else, including a trigger match.
	stopped, err := p.handleUnsubscribe(ctx, contact, event.Message.Text)
	if err != nil {
		return err
	}
	if stopped {
		return nil
	}

	return p.handleTrigger(ctx, contact, event.Message.Text, event.Timestamp)
}

func (p *Processor) handleUnsubscribe(ctx context.Context, contact *domain.Contact, text string) (bool, error) {
	isStop, err := p.campaigns.IsUnsubscribe(ctx, nil, contact.ID, text)
	if err != nil {
		return false, fmt.Errorf("check unsubscribe keywords: %w", err)
	}
	if !isStop {
		return false, nil
	}

	if err := p.contacts.SetOptOut(ctx, nil, contact.ID, true); err != nil {
		return false, fmt.Errorf("record opt out: %w", err)
	}
	if err := p.campaigns.StopForContact(ctx, contact.ID,
		domain.EnrollmentUnsubscribed, "contact sent an unsubscribe keyword"); err != nil {
		return false, fmt.Errorf("stop enrollments: %w", err)
	}

	p.log.Info("contact unsubscribed",
		slog.String("contact_id", contact.ID.String()),
		slog.String("phone", contact.Phone))

	return true, nil
}

func (p *Processor) handleTrigger(ctx context.Context, contact *domain.Contact, text string, at time.Time) error {
	match, err := p.campaigns.MatchTrigger(ctx, nil, text)
	if err != nil {
		return fmt.Errorf("match trigger: %w", err)
	}
	if match == nil {
		// No campaign claims this message. The platform stays silent: it never
		// replies to a message it was not configured to answer.
		return nil
	}

	result, err := p.campaigns.HandleTrigger(ctx, contact, match, at)
	if err != nil {
		return fmt.Errorf("handle trigger: %w", err)
	}

	if result.Action == campaigns.ActionSpecialMessage && result.SpecialTemplateID != nil {
		if err := p.sendSpecialReply(ctx, contact, result); err != nil {
			// The enrollment decision already stands; a failed courtesy reply
			// must not cause the whole event to be retried.
			p.log.Error("repeat-trigger reply failed", slog.String("error", err.Error()))
		}
	}

	return nil
}

// sendSpecialReply answers a repeat trigger with the configured template.
func (p *Processor) sendSpecialReply(ctx context.Context, contact *domain.Contact, result *campaigns.EnrollResult) error {
	spec, err := p.templates.ResolveForSend(ctx, nil, *result.SpecialTemplateID)
	if err != nil {
		return err
	}

	values := map[string]string{
		"contact_name":  contact.DisplayName(),
		"first_name":    textnorm.FirstName(contact.DisplayName()),
		"phone":         phone.Display(contact.Phone),
		"campaign_name": result.Campaign.Name,
		"webinar_link":  result.Campaign.WebinarLink,
		"timezone":      result.Campaign.Timezone,
	}
	if result.Campaign.EventStartAt != nil {
		tz := result.Campaign.Timezone
		values["webinar_date"] = timex.FormatIn(*result.Campaign.EventStartAt, tz, "02.01.2006")
		values["webinar_time"] = timex.FormatIn(*result.Campaign.EventStartAt, tz, "15:04")
		values["webinar_datetime"] = timex.FormatIn(*result.Campaign.EventStartAt, tz, "02.01.2006 15:04")
		values["remaining_time"] = timex.RemainingLabel(time.Now().UTC(), *result.Campaign.EventStartAt)
	}

	out := messaging.Outbound{
		Type:            spec.Type,
		Text:            render.Render(spec.Body, render.NewContext(values)),
		LinkPreview:     spec.LinkPreview,
		MediaFileID:     spec.MediaID,
		MediaMIME:       spec.MediaMIME,
		FileName:        spec.FileName,
		CampaignID:      &result.Campaign.ID,
		TemplateID:      &spec.TemplateID,
		TemplateVersion: &spec.Version,
	}
	if spec.Type.RequiresMedia() {
		abs, err := p.media.AbsPath(spec.MediaPath)
		if err != nil {
			return err
		}
		out.MediaPath = abs
	}

	_, err = p.sender.Send(ctx, contact, out)
	return err
}

// handleOutgoingEcho records messages the operator sent from the linked phone
// so the admin conversation view matches what the customer sees.
func (p *Processor) handleOutgoingEcho(ctx context.Context, event *whatsapp.Event) error {
	if event.Message == nil || event.ExternalID == "" {
		return nil
	}
	if phone.IsGroupChatID(event.ChatID) {
		return nil
	}

	// Messages this platform sent are already recorded; only echoes of
	// messages composed elsewhere need to be added.
	exists, err := p.messages.ExistsByExternalID(ctx, nil, event.ExternalID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	digits, err := phone.FromChatID(event.ChatID)
	if err != nil {
		return nil
	}

	contact, err := p.contacts.GetByPhone(ctx, nil, digits)
	if err != nil {
		return err
	}
	if contact == nil {
		// An outbound message to someone who never wrote to us: record nothing
		// rather than inventing a contact with no consent anchor.
		return nil
	}

	id := event.ExternalID
	sentAt := event.Timestamp
	message := &domain.Message{
		ContactID:  contact.ID,
		Direction:  domain.DirectionOutgoing,
		Type:       string(event.Message.Type),
		Text:       event.Message.Text,
		MediaURL:   event.Message.DownloadURL,
		FileName:   event.Message.FileName,
		MimeType:   event.Message.MimeType,
		ExternalID: &id,
		Status:     domain.MessageStatusSent,
		SentAt:     &sentAt,
	}
	if event.Message.Type.IsMedia() && event.Message.DownloadURL != "" {
		message.MediaDownloadState = "PENDING"
	}

	if err := p.messages.Create(ctx, nil, message); err != nil {
		if errors.Is(err, conversations.ErrDuplicate) {
			return nil
		}
		return err
	}

	preview := messaging.Preview(event.Message.Type, event.Message.Text, event.Message.FileName)
	if err := p.contacts.RecordOutgoing(ctx, nil, contact.ID, preview, string(event.Message.Type), sentAt); err != nil {
		p.log.Warn("updating contact activity failed", slog.String("error", err.Error()))
	}

	return nil
}

func (p *Processor) handleStatus(ctx context.Context, event *whatsapp.Event) error {
	if event.Status == nil || event.Status.ExternalID == "" || event.Status.Status == "" {
		return nil
	}

	if _, err := p.messages.ApplyStatus(ctx, nil,
		event.Status.ExternalID, event.Status.Status, event.Timestamp, event.Status.Description); err != nil {
		return fmt.Errorf("apply delivery status: %w", err)
	}
	return nil
}
