package inbound

import (
	"context"
	"encoding/json"
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
	"github.com/ayran/whatsapp-automation/pkg/backoff"
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
	sender    replyDispatcher
	media     *media.Store
	log       *slog.Logger
	history   *messaging.ReplyHistory

	wg   sync.WaitGroup
	once sync.Once
}

type replyDispatcher interface {
	Send(context.Context, *domain.Contact, messaging.Outbound) (*domain.Message, error)
}

func NewProcessor(
	cfg config.Scheduler,
	repo *Repository,
	contactRepo *contacts.Repository,
	messageRepo *conversations.Repository,
	campaignSvc *campaigns.Service,
	templateRepo *templates.Repository,
	sender replyDispatcher,
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
		history:   messaging.NewReplyHistory(nil, messageRepo),
	}
}

func (p *Processor) WithReplyHistory(history *messaging.ReplyHistory) *Processor {
	p.history = history
	return p
}

// Ingest stores a raw notification and returns whether it was new.
//
// It does nothing else: parsing, contact creation and campaign enrollment all
// happen on the background workers. Keeping this to a single insert is what
// lets the poller acknowledge a notification immediately and move on, instead
// of holding the provider's queue open for the length of a campaign enrollment.
func (p *Processor) Ingest(ctx context.Context, instanceID int64, instanceKey string, body []byte) (accepted bool, err error) {
	event, err := greenapi.ParseWebhook(body)
	if err != nil {
		return false, err
	}

	_, inserted, err := p.repo.Insert(ctx, instanceID, "greenapi", event.RawType,
		scopedDedupeKey(instanceKey, event.DedupeKey), event.ExternalID, body, event.Timestamp)
	if err != nil {
		return false, err
	}

	if !inserted {
		p.log.Debug("duplicate notification ignored",
			slog.Int64("whatsapp_instance_id", instanceID),
			slog.String("type", event.RawType),
			slog.String("dedupe_key", event.DedupeKey))
		return false, nil
	}

	p.log.Info("incoming_event_persisted",
		slog.Int64("whatsapp_instance_id", instanceID),
		slog.String("instance", instanceKey),
		slog.String("type", event.RawType),
		slog.String("external_message_id", event.ExternalID))
	return true, nil
}

// scopedDedupeKey namespaces a provider's deduplication key with the account
// that produced it.
//
// Green API's own identifiers are unique inside one instance, not across
// instances, and the fallback key for an event carrying no message id is a
// hash of the payload -- two accounts reporting the same state change produce
// the same bytes. Prefixing with the account key makes the existing
// UNIQUE (provider, dedupe_key) index per-account without rebuilding the
// table, so a notification handled on one number can no longer silently
// suppress an identical-looking one on the other.
//
// Legacy keys are also recognized by instance + external incoming message ID
// in Repository.Insert. Existing rows do not need to be rewritten.
func scopedDedupeKey(instanceKey, dedupeKey string) string {
	instanceKey = strings.TrimSpace(instanceKey)
	if instanceKey == "" {
		instanceKey = domain.InstanceKeyPrimary
	}
	return instanceKey + ":" + dedupeKey
}

// Start launches the processing loops.
func (p *Processor) Start(ctx context.Context) {
	p.once.Do(func() {
		if released, err := p.repo.ReleaseStale(ctx, 5*time.Minute); err != nil {
			p.log.Error("startup recovery failed", slog.String("error", err.Error()))
		} else if released > 0 {
			p.log.Warn("requeued notifications from a previous run", slog.Int64("count", released))
		}
		p.recoverRetryable(ctx)

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
	limit := p.cfg.NotificationBatchSize
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, p.recoveryBatchSize())
	for i := 0; i < limit; i++ {
		if ctx.Err() != nil {
			return
		}

		// Claim only work about to run: queued claims must not lose their
		// leases while another reply waits for outbound pacing. Reserve slots
		// for live traffic, oldest work, and triggers not waiting for a queued
		// reply. Rechecking a large outbound backlog must not starve new work.
		lane := claimReady
		switch i % 4 {
		case 0:
			lane = claimLive
		case 1:
			lane = claimOldest
		}
		events, err := p.repo.claim(ctx, 1, lane)
		if err != nil {
			p.log.Error("claiming notifications failed", slog.String("error", err.Error()))
			return
		}
		if len(events) == 0 && lane == claimLive {
			events, err = p.repo.claim(ctx, 1, claimReady)
			if err != nil {
				p.log.Error("claiming notifications failed", slog.String("error", err.Error()))
				return
			}
		}
		if len(events) == 0 {
			events, err = p.repo.Claim(ctx, 1)
			if err != nil || len(events) == 0 {
				return
			}
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

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	lastPrune := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.recoverRetryable(ctx)
			if _, err := p.repo.ReleaseStale(ctx, 5*time.Minute); err != nil {
				p.log.Error("releasing stale notifications failed", slog.String("error", err.Error()))
			}
			if time.Since(lastPrune) < 10*time.Minute {
				continue
			}
			lastPrune = time.Now()
			// Retain incoming identities/checkpoints. Other processed provider
			// notifications keep the existing two-week diagnostics retention.
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
	ctx, release := p.keepLease(ctx, stored)
	defer release()
	event, err := greenapi.ParseWebhook(stored.Payload)
	if err != nil {
		// A payload that cannot be parsed will never parse; do not retry it.
		p.finish(ctx, stored.ID, domain.WebhookIgnored, fmt.Sprintf("unparseable payload: %v", err))
		return
	}

	// The account is read from the stored row, not from whichever poller
	// happens to be running. A notification that outlives a restart is applied
	// to the same account it arrived on.
	instanceID := domain.InstanceIDOrPrimary(&stored.WhatsAppInstanceID)

	handlerErr := p.apply(ctx, instanceID, stored, event)
	if handlerErr == nil {
		p.finish(ctx, stored.ID, domain.WebhookProcessed, "")
		return
	}
	if errors.Is(handlerErr, errAwaitingReply) {
		if err := p.repo.Defer(ctx, stored.ID, time.Now().UTC().Add(5*time.Second), handlerErr.Error(), true); err != nil {
			p.log.Error("deferring trigger event failed", slog.String("error", err.Error()))
		}
		return
	}

	retryable := whatsapp.IsRetryable(handlerErr) || whatsapp.IsAvailabilityFailure(handlerErr)
	if !retryable || stored.Attempts >= maxWebhookAttempts {
		p.log.Error("notification failed permanently",
			slog.Int64("instance_id", stored.WhatsAppInstanceID),
			slog.String("external_message_id", stored.ExternalID),
			slog.String("type", event.RawType),
			slog.Int("attempts", stored.Attempts),
			slog.String("error", handlerErr.Error()))
		reason := handlerErr.Error()
		if !retryable {
			reason = "permanent failure: " + reason
		}
		p.finish(ctx, stored.ID, domain.WebhookFailed, reason)
		return
	}

	p.log.Warn("notification will be retried",
		slog.Int64("instance_id", stored.WhatsAppInstanceID),
		slog.String("external_message_id", stored.ExternalID),
		slog.String("type", event.RawType),
		slog.Int("attempts", stored.Attempts),
		slog.String("error", handlerErr.Error()))
	next := backoff.Default(p.cfg.RetryBaseDelay, p.cfg.RetryMaxDelay).NextAttemptAt(time.Now().UTC(), stored.Attempts)
	if err := p.repo.Defer(ctx, stored.ID, next, handlerErr.Error(), false); err != nil {
		p.log.Error("requeueing notification failed", slog.String("error", err.Error()))
	}
}

func (p *Processor) finish(ctx context.Context, id uuid.UUID, status domain.WebhookEventStatus, errText string) {
	if err := p.repo.MarkProcessed(ctx, id, status, errText); err != nil {
		p.log.Error("recording notification outcome failed", slog.String("error", err.Error()))
	}
}

func (p *Processor) apply(ctx context.Context, instanceID int64, stored StoredEvent, event *whatsapp.Event) error {
	switch event.Kind {
	case whatsapp.EventIncomingMessage:
		return p.handleIncoming(ctx, instanceID, stored, event)
	case whatsapp.EventOutgoingMessage:
		return p.handleOutgoingEcho(ctx, instanceID, event)
	case whatsapp.EventOutgoingStatus:
		return p.handleStatus(ctx, instanceID, event)
	case whatsapp.EventStateChanged:
		p.log.Info("[WHATSAPP] provider state changed",
			slog.Int64("whatsapp_instance_id", instanceID),
			slog.String("state", event.State))
		return nil
	case whatsapp.EventIncomingCall:
		p.log.Info("incoming call ignored",
			slog.Int64("whatsapp_instance_id", instanceID),
			slog.String("from", event.ChatID))
		return nil
	default:
		return nil
	}
}

// handleIncoming is the entry point of the whole funnel.
//
// instanceID is the number the customer wrote to, and it is carried through
// every decision below: which campaigns may claim the message, which stop
// words count, and which enrolments an unsubscribe ends. Nothing here ever
// looks at the installation as a whole.
func (p *Processor) handleIncoming(ctx context.Context, instanceID int64, stored StoredEvent, event *whatsapp.Event) error {
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
		IncomingEventID:    &stored.ID,
		ContactID:          contact.ID,
		WhatsAppInstanceID: &instanceID,
		Direction:          domain.DirectionIncoming,
		Type:               string(event.Message.Type),
		Text:               event.Message.Text,
		MediaURL:           event.Message.DownloadURL,
		FileName:           event.Message.FileName,
		MimeType:           event.Message.MimeType,
		Status:             domain.MessageStatusReceived,
		CreatedAt:          event.Timestamp,
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

	duplicate := false
	if err := p.messages.Create(ctx, nil, message); err != nil {
		if errors.Is(err, conversations.ErrDuplicate) {
			// messages still has the original global external-id index. Keep
			// it intact on production databases, but do not let another
			// account's identical provider ID suppress this message's history.
			var sameAccount bool
			if err := p.repo.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages
			  WHERE external_id = $1 AND direction = 'INCOMING'
			    AND COALESCE(whatsapp_instance_id, 1) = $2)`, event.ExternalID, instanceID).Scan(&sameAccount); err != nil {
				return err
			}
			if !sameAccount {
				scopedID := fmt.Sprintf("incoming:%d:%s", instanceID, event.ExternalID)
				message.ExternalID = &scopedID
				if err := p.messages.Create(ctx, nil, message); err != nil && !errors.Is(err, conversations.ErrDuplicate) {
					return err
				} else if err == nil {
					duplicate = false
				} else {
					duplicate = true
				}
			} else {
				// Recording history is not proof that the trigger ran. Resume
				// from the transactional business checkpoint instead of returning.
				duplicate = true
			}
		} else {
			return fmt.Errorf("record inbound message: %w", err)
		}
	}

	if !duplicate {
		preview := messaging.Preview(event.Message.Type, event.Message.Text, event.Message.FileName)
		if err := p.contacts.RecordIncoming(ctx, nil, contact.ID, preview, string(event.Message.Type), event.Timestamp); err != nil {
			p.log.Warn("updating contact activity failed", slog.String("error", err.Error()))
		}
	}

	// Only text carries intent the automation can act on.
	if event.Message.Type != whatsapp.TypeText || strings.TrimSpace(event.Message.Text) == "" {
		return nil
	}

	// Unsubscribe wins over everything else, including a trigger match.
	stopped, err := p.handleUnsubscribe(ctx, instanceID, contact, event.Message.Text)
	if err != nil {
		return err
	}
	if stopped {
		return nil
	}

	return p.handleTrigger(ctx, instanceID, stored, contact, event.Message.Text, event.Timestamp,
		duplicate || stored.Error == "manual trigger recovery")
}

// handleUnsubscribe applies a stop word to the account that received it.
//
// The scope is the point. "STOP" written to one of the installation's numbers
// is an instruction about that number's messages: it ends those enrolments and
// cancels those queued jobs, and it leaves the other number's alone. Somebody
// who signed up for two different webinars through two different numbers can
// leave one without losing the other.
//
// The whole-person columns on contacts -- opted_out, opted_out_at, status --
// are still written exactly as before, so the panel, its filters, the exports
// and the analytics see no change. They are the "asked us to stop somewhere"
// view; the per-account row written here is what the send path actually asks
// before delivering anything.
func (p *Processor) handleUnsubscribe(ctx context.Context, instanceID int64, contact *domain.Contact, text string) (bool, error) {
	isStop, err := p.campaigns.IsUnsubscribe(ctx, nil, contact.ID, instanceID, text)
	if err != nil {
		return false, fmt.Errorf("check unsubscribe keywords: %w", err)
	}
	if !isStop {
		return false, nil
	}

	if err := p.contacts.OptOutOn(ctx, nil, instanceID, contact.ID,
		"contact sent an unsubscribe keyword"); err != nil {
		return false, fmt.Errorf("record opt out: %w", err)
	}
	if err := p.contacts.SetOptOut(ctx, nil, contact.ID, true); err != nil {
		return false, fmt.Errorf("record opt out: %w", err)
	}
	if err := p.campaigns.StopForContactOnInstance(ctx, contact.ID, instanceID,
		domain.EnrollmentUnsubscribed, "contact sent an unsubscribe keyword"); err != nil {
		return false, fmt.Errorf("stop enrollments: %w", err)
	}

	p.log.Info("[WHATSAPP] contact unsubscribed",
		slog.Int64("whatsapp_instance_id", instanceID),
		slog.String("contact_id", contact.ID.String()),
		slog.String("phone", contact.Phone))

	return true, nil
}

var errAwaitingReply = errors.New("trigger reply is waiting in the outbound queue")

func (p *Processor) handleTrigger(ctx context.Context, instanceID int64, stored StoredEvent, contact *domain.Contact, text string, at time.Time, recovery bool) error {
	if stored.TriggerResult != nil {
		var result campaigns.EnrollResult
		if err := json.Unmarshal([]byte(*stored.TriggerResult), &result); err != nil {
			return fmt.Errorf("read trigger checkpoint: %w", err)
		}
		return p.completeTriggerReply(ctx, instanceID, stored.ID, contact, &result)
	}
	// Only this account's campaigns are candidates. A word configured on the
	// other number is not a trigger here, however exactly it matches.
	match, err := p.campaigns.MatchTrigger(ctx, nil, instanceID, text)
	if err != nil {
		return fmt.Errorf("match trigger: %w", err)
	}
	if match == nil {
		// No campaign claims this message. The platform stays silent: it never
		// replies to a message it was not configured to answer.
		return nil
	}
	if time.Since(at) > messaging.HistoricalReplyAge || stored.Error == "manual trigger recovery" {
		state, err := p.history.Check(ctx, instanceID, contact.ID, contact.ChatID, stored.ExternalID, at)
		if err != nil {
			return err
		}
		if state == messaging.ReplyAnswered {
			p.log.Info("trigger_reply_already_sent", slog.String("event_id", stored.ID.String()))
			return nil
		}
		if state != messaging.ReplyUnanswered {
			return messaging.ManualReviewError()
		}
		recovery = true
	}

	result, err := p.campaigns.HandleTriggerEvent(ctx, contact, match, at, stored.ID, recovery)
	if err != nil {
		return fmt.Errorf("handle trigger: %w", err)
	}

	p.log.Info("trigger_matched",
		slog.Int64("whatsapp_instance_id", instanceID),
		slog.String("campaign", match.CampaignName),
		slog.String("campaign_id", match.CampaignID.String()),
		slog.String("trigger_id", match.TriggerID.String()),
		slog.String("external_message_id", stored.ExternalID),
		slog.String("contact_id", contact.ID.String()),
		slog.String("action", string(result.Action)))

	return p.completeTriggerReply(ctx, instanceID, stored.ID, contact, result)
}

func (p *Processor) completeTriggerReply(ctx context.Context, instanceID int64, eventID uuid.UUID, contact *domain.Contact, result *campaigns.EnrollResult) error {
	if result.InitialJobID != nil {
		return p.repo.InitialReplyOutcome(ctx, *result.InitialJobID)
	}
	if result.Action != campaigns.ActionSpecialMessage || result.SpecialTemplateID == nil {
		return nil
	}
	sent, err := p.messages.SuccessfulOutbound(ctx, nil, &eventID)
	if err != nil {
		return err
	}
	if sent != nil {
		p.log.Info("trigger_reply_already_sent", slog.String("event_id", eventID.String()), slog.String("contact_id", contact.ID.String()))
		return nil
	}
	if err := p.sendSpecialReply(ctx, instanceID, eventID, contact, result); err != nil {
		if errors.Is(err, messaging.ErrAlreadyAnswered) {
			return nil
		}
		p.log.Warn("trigger_reply_retry", slog.String("event_id", eventID.String()), slog.String("error", err.Error()))
		return err
	}
	p.log.Info("trigger_reply_sent", slog.String("event_id", eventID.String()), slog.String("contact_id", contact.ID.String()))
	return nil
}

// sendSpecialReply answers a repeat trigger with the configured template,
// through the account the trigger arrived on.
func (p *Processor) sendSpecialReply(ctx context.Context, instanceID int64, eventID uuid.UUID, contact *domain.Contact, result *campaigns.EnrollResult) error {
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
	// The webinar this contact is waiting for: their pinned occurrence when the
	// campaign runs daily, the campaign's own event start otherwise.
	if webinar := campaigns.EventAnchor(result.Campaign, result.Enrollment); webinar != nil {
		tz := result.Campaign.Timezone
		values["webinar_date"] = timex.FormatIn(*webinar, tz, "02.01.2006")
		values["webinar_time"] = timex.FormatIn(*webinar, tz, "15:04")
		values["webinar_datetime"] = timex.FormatIn(*webinar, tz, "02.01.2006 15:04")
		values["remaining_time"] = timex.RemainingLabel(time.Now().UTC(), *webinar)
	}

	out := messaging.Outbound{
		IncomingEventID:    &eventID,
		WhatsAppInstanceID: instanceID,

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
func (p *Processor) handleOutgoingEcho(ctx context.Context, instanceID int64, event *whatsapp.Event) error {
	if event.Message == nil || event.ExternalID == "" {
		return nil
	}
	if phone.IsGroupChatID(event.ChatID) {
		return nil
	}

	// Messages this platform sent are already recorded; only echoes of
	// messages composed elsewhere need to be added.
	exists, err := p.messages.ExistsByExternalIDOn(ctx, nil, instanceID, event.ExternalID)
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
		ContactID:          contact.ID,
		WhatsAppInstanceID: &instanceID,
		Direction:          domain.DirectionOutgoing,
		Type:               string(event.Message.Type),
		Text:               event.Message.Text,
		MediaURL:           event.Message.DownloadURL,
		FileName:           event.Message.FileName,
		MimeType:           event.Message.MimeType,
		ExternalID:         &id,
		Status:             domain.MessageStatusSent,
		SentAt:             &sentAt,
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

func (p *Processor) handleStatus(ctx context.Context, instanceID int64, event *whatsapp.Event) error {
	if event.Status == nil || event.Status.ExternalID == "" || event.Status.Status == "" {
		return nil
	}

	if _, err := p.messages.ApplyStatus(ctx, nil, instanceID,
		event.Status.ExternalID, event.Status.Status, event.Timestamp, event.Status.Description); err != nil {
		return fmt.Errorf("apply delivery status: %w", err)
	}
	return nil
}
