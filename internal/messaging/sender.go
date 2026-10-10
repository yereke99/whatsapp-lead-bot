// Package messaging turns a rendered template or an operator's input into a
// delivered WhatsApp message, and records it in the conversation history.
//
// Every outbound path in the platform goes through Sender.Send. That is where
// the consent rule lives: the platform never initiates a conversation, so a
// contact who has not written to us first can never be messaged, by automation
// or by an administrator.
package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

var (
	// ErrNoConsent is returned when the target has never written to us. This
	// is a permanent refusal, never retried.
	ErrNoConsent = errors.New("contact has not opted in: the platform only replies to contacts who wrote first")
	// ErrOptedOut is returned for an unsubscribed or blocked contact.
	ErrOptedOut = errors.New("contact has unsubscribed or is blocked")
	// ErrAlreadyDelivered reports that this job's message already reached the
	// provider, so the retry must not send a second copy.
	ErrAlreadyDelivered = errors.New("message was already delivered")
)

// Outbound describes one message to deliver.
type Outbound struct {
	// WhatsAppInstanceID names the account this message leaves through, and it
	// is required. There is no default: a zero here resolves to Primary only
	// through domain.InstanceIDOrPrimary, and every caller inside the platform
	// sets it explicitly from the campaign that owns the message.
	WhatsAppInstanceID int64

	Type        domain.TemplateType
	Text        string
	LinkPreview bool

	MediaFileID *uuid.UUID
	MediaPath   string
	MediaMIME   string
	FileName    string

	CampaignID         *uuid.UUID
	EnrollmentID       *uuid.UUID
	CampaignStepID     *uuid.UUID
	ScheduledMessageID *uuid.UUID
	IncomingEventID    *uuid.UUID
	TemplateID         *uuid.UUID
	TemplateVersion    *int

	IsManual bool
	AdminID  *uuid.UUID
}

// ProviderRegistry resolves an account id to the transport bound to that
// account's credentials.
//
// Sender depends on this rather than on a provider so that no send path can
// hold a client for one number and use it for another: the client is looked up
// per message, from the id the message itself carries. An unknown, disabled or
// unconfigured account returns an error, never a different account's client.
type ProviderRegistry interface {
	ProviderFor(instanceID int64) (whatsapp.Provider, error)
}

type Sender struct {
	providers  ProviderRegistry
	messages   *conversations.Repository
	contacts   *contacts.Repository
	mediaStore *media.Store
	log        *slog.Logger
}

func NewSender(
	providers ProviderRegistry,
	messages *conversations.Repository,
	contactRepo *contacts.Repository,
	mediaStore *media.Store,
	log *slog.Logger,
) *Sender {
	return &Sender{
		providers:  providers,
		messages:   messages,
		contacts:   contactRepo,
		mediaStore: mediaStore,
		log:        log.With(slog.String("component", "messaging")),
	}
}

// Send delivers a message and records it.
//
// Order of operations matters for recoverability:
//
//  1. the consent guard runs before anything is written or sent;
//  2. a PENDING message row is created, so a crash mid-flight leaves a
//     visible record rather than a silent gap;
//  3. the provider call happens;
//  4. the row is marked SENT with the provider id, which is also the
//     deduplication key for the delivery-status webhooks that follow.
func (s *Sender) Send(ctx context.Context, contact *domain.Contact, out Outbound) (*domain.Message, error) {
	if contact == nil {
		return nil, errors.New("contact is required")
	}
	if out.ScheduledMessageID != nil || out.IncomingEventID != nil {
		sent, err := s.messages.SuccessfulOutbound(ctx, out.ScheduledMessageID, out.IncomingEventID)
		if err != nil {
			return nil, fmt.Errorf("check previous response: %w", err)
		}
		if sent != nil {
			s.log.Info("trigger_reply_already_sent", slog.String("message_id", sent.ID.String()))
			return sent, nil
		}
	}

	instanceID := domain.InstanceIDOrPrimary(&out.WhatsAppInstanceID)

	// Resolve the transport before anything is written. A message that cannot
	// leave through the account it belongs to must not be recorded as pending
	// and must never be retried through another account.
	provider, err := s.providers.ProviderFor(instanceID)
	if err != nil {
		return nil, err
	}

	if err := s.checkConsent(ctx, contact, instanceID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(contact.ChatID) == "" {
		return nil, fmt.Errorf("contact %s has no chat id", contact.ID)
	}

	msgType := messageTypeFor(out.Type)
	if err := validateOutbound(out, msgType); err != nil {
		return nil, err
	}

	record := &domain.Message{
		ContactID:          contact.ID,
		WhatsAppInstanceID: &instanceID,
		CampaignID:         out.CampaignID,
		EnrollmentID:       out.EnrollmentID,
		CampaignStepID:     out.CampaignStepID,
		ScheduledMessageID: out.ScheduledMessageID,
		IncomingEventID:    out.IncomingEventID,
		Direction:          domain.DirectionOutgoing,
		Type:               string(msgType),
		Text:               out.Text,
		MediaFileID:        out.MediaFileID,
		FileName:           out.FileName,
		MimeType:           out.MediaMIME,
		Status:             domain.MessageStatusPending,
		IsManual:           out.IsManual,
		SentByAdminID:      out.AdminID,
		TemplateID:         out.TemplateID,
		TemplateVersion:    out.TemplateVersion,
	}

	if err := s.messages.Create(ctx, nil, record); err != nil {
		return nil, fmt.Errorf("record outgoing message: %w", err)
	}

	result, sendErr := s.deliver(ctx, provider, contact.ChatID, out)
	if sendErr != nil {
		if markErr := s.messages.MarkFailed(ctx, nil, record.ID, sendErr.Error()); markErr != nil {
			s.log.Error("could not mark message failed",
				slog.String("message_id", record.ID.String()),
				slog.String("error", markErr.Error()))
		}
		record.Status = domain.MessageStatusFailed
		record.Error = sendErr.Error()
		return record, sendErr
	}

	sentAt := time.Now().UTC()
	// Shutdown must not cancel recording an acknowledgement already received.
	receiptCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.messages.MarkSent(receiptCtx, nil, record.ID, result.ExternalID, sentAt); err != nil {
		s.log.Error("message sent but not recorded as sent",
			slog.String("message_id", record.ID.String()),
			slog.String("external_id", result.ExternalID),
			slog.String("error", err.Error()))
		return record, &whatsapp.Error{Provider: provider.Name(), Op: "persistSendReceipt",
			Message: "provider accepted message but receipt could not be saved; inspect before retry", Err: err}
	}

	record.Status = domain.MessageStatusSent
	record.ExternalID = &result.ExternalID
	record.SentAt = &sentAt

	preview := previewFor(msgType, out.Text, out.FileName)
	if err := s.contacts.RecordOutgoing(ctx, nil, contact.ID, preview, string(msgType), sentAt); err != nil {
		s.log.Warn("could not update contact activity",
			slog.String("contact_id", contact.ID.String()),
			slog.String("error", err.Error()))
	}

	s.log.Info("message sent",
		slog.Int64("whatsapp_instance_id", instanceID),
		slog.String("contact_id", contact.ID.String()),
		slog.String("type", string(msgType)),
		slog.String("external_id", result.ExternalID),
		slog.Bool("manual", out.IsManual))

	return record, nil
}

// checkConsent is the single gate protecting the "never message first" rule.
//
// Two halves, and they have different scopes on purpose:
//
//   - having written to us first, and not being blocked, are facts about the
//     person. They hold on every number the installation owns.
//   - having unsubscribed is an instruction given to one number. Somebody who
//     sent STOP to the second number is still waiting for the first number's
//     messages, and stopping those would be the platform deciding something
//     the customer did not ask for.
//
// The per-account lookup is a primary-key read on a table with one row per
// unsubscribe, and sends are paced seconds apart, so asking every time costs
// nothing and removes any chance of a stale copy being carried into a retry.
func (s *Sender) checkConsent(ctx context.Context, contact *domain.Contact, instanceID int64) error {
	if !contact.CanBeContacted() {
		if contact.FirstContactAt == nil {
			return ErrNoConsent
		}
		return ErrOptedOut
	}

	optedOut, err := s.contacts.IsOptedOutOn(ctx, nil, instanceID, contact.ID)
	if err != nil {
		return fmt.Errorf("check unsubscribe state: %w", err)
	}
	if optedOut {
		return ErrOptedOut
	}
	return nil
}

// deliver hands the message to the account's own client. The provider is
// passed in rather than read from the Sender, which is what makes it
// impossible for two accounts sending concurrently to reach each other's
// credentials.
func (s *Sender) deliver(ctx context.Context, provider whatsapp.Provider, chatID string, out Outbound) (*whatsapp.SendResult, error) {
	switch out.Type {
	case domain.TemplateText:
		return provider.SendText(ctx, whatsapp.TextMessage{
			ChatID:      chatID,
			Text:        out.Text,
			LinkPreview: out.LinkPreview,
		})

	case domain.TemplateImage, domain.TemplateImageWithCaption:
		return provider.SendImage(ctx, s.mediaMessage(chatID, whatsapp.MediaImage, out))

	case domain.TemplateVideo, domain.TemplateVideoWithCaption:
		return provider.SendVideo(ctx, s.mediaMessage(chatID, whatsapp.MediaVideo, out))

	case domain.TemplateAudio:
		return provider.SendAudio(ctx, s.mediaMessage(chatID, whatsapp.MediaAudio, out))

	case domain.TemplateVoice:
		return provider.SendVoice(ctx, s.mediaMessage(chatID, whatsapp.MediaVoice, out))

	case domain.TemplateDocument:
		return provider.SendDocument(ctx, s.mediaMessage(chatID, whatsapp.MediaDocument, out))

	default:
		return nil, fmt.Errorf("unsupported message type %q", out.Type)
	}
}

// mediaMessage builds the provider payload. The caption travels with the file
// so the recipient sees a single WhatsApp message rather than a picture
// followed by a stray block of text.
func (s *Sender) mediaMessage(chatID string, kind whatsapp.MediaKind, out Outbound) whatsapp.MediaMessage {
	caption := ""
	if out.Type.AllowsCaption() {
		caption = out.Text
	}

	fileName := out.FileName
	if strings.TrimSpace(fileName) == "" {
		fileName = defaultFileName(kind)
	}

	return whatsapp.MediaMessage{
		ChatID:   chatID,
		Kind:     kind,
		Caption:  caption,
		FileName: fileName,
		MimeType: out.MediaMIME,
		FilePath: out.MediaPath,
	}
}

func validateOutbound(out Outbound, msgType whatsapp.MessageType) error {
	if out.Type == domain.TemplateText {
		if strings.TrimSpace(out.Text) == "" {
			return errors.New("message text is empty")
		}
		return nil
	}
	if strings.TrimSpace(out.MediaPath) == "" {
		return fmt.Errorf("%s message has no media file", msgType)
	}
	return nil
}

func messageTypeFor(t domain.TemplateType) whatsapp.MessageType {
	switch t {
	case domain.TemplateImage, domain.TemplateImageWithCaption:
		return whatsapp.TypeImage
	case domain.TemplateVideo, domain.TemplateVideoWithCaption:
		return whatsapp.TypeVideo
	case domain.TemplateAudio:
		return whatsapp.TypeAudio
	case domain.TemplateVoice:
		return whatsapp.TypeVoice
	case domain.TemplateDocument:
		return whatsapp.TypeDocument
	default:
		return whatsapp.TypeText
	}
}

func defaultFileName(kind whatsapp.MediaKind) string {
	switch kind {
	case whatsapp.MediaImage:
		return "image.jpg"
	case whatsapp.MediaVideo:
		return "video.mp4"
	case whatsapp.MediaVoice:
		return "voice.ogg"
	case whatsapp.MediaAudio:
		return "audio.mp3"
	default:
		return "document"
	}
}

// previewFor builds the short chat-list summary for a message.
func previewFor(msgType whatsapp.MessageType, text, fileName string) string {
	text = strings.TrimSpace(text)
	if text != "" {
		return text
	}

	switch msgType {
	case whatsapp.TypeImage:
		return "📷 Сурет"
	case whatsapp.TypeVideo:
		return "🎬 Бейне"
	case whatsapp.TypeVoice:
		return "🎤 Дауыстық хабарлама"
	case whatsapp.TypeAudio:
		return "🎵 Аудио"
	case whatsapp.TypeDocument:
		if fileName != "" {
			return "📄 " + fileName
		}
		return "📄 Құжат"
	case whatsapp.TypeSticker:
		return "Стикер"
	case whatsapp.TypeLocation:
		return "📍 Геолокация"
	case whatsapp.TypeContact:
		return "👤 Контакт"
	case whatsapp.TypePoll:
		return "📊 Сауалнама"
	default:
		return ""
	}
}

// Preview is exported for the inbound pipeline, which needs the same summary
// shape for incoming messages.
func Preview(msgType whatsapp.MessageType, text, fileName string) string {
	return previewFor(msgType, text, fileName)
}
