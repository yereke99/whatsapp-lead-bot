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
	TemplateID         *uuid.UUID
	TemplateVersion    *int

	IsManual bool
	AdminID  *uuid.UUID
}

type Sender struct {
	provider   whatsapp.Provider
	messages   *conversations.Repository
	contacts   *contacts.Repository
	mediaStore *media.Store
	log        *slog.Logger
}

func NewSender(
	provider whatsapp.Provider,
	messages *conversations.Repository,
	contactRepo *contacts.Repository,
	mediaStore *media.Store,
	log *slog.Logger,
) *Sender {
	return &Sender{
		provider:   provider,
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
	if err := s.checkConsent(contact); err != nil {
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
		CampaignID:         out.CampaignID,
		EnrollmentID:       out.EnrollmentID,
		CampaignStepID:     out.CampaignStepID,
		ScheduledMessageID: out.ScheduledMessageID,
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

	result, sendErr := s.deliver(ctx, contact.ChatID, out)
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
	if err := s.messages.MarkSent(ctx, nil, record.ID, result.ExternalID, sentAt); err != nil {
		// The provider accepted the message; failing to persist the id costs
		// us later status correlation but the send itself stands.
		s.log.Error("message sent but not recorded as sent",
			slog.String("message_id", record.ID.String()),
			slog.String("external_id", result.ExternalID),
			slog.String("error", err.Error()))
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
		slog.String("contact_id", contact.ID.String()),
		slog.String("type", string(msgType)),
		slog.String("external_id", result.ExternalID),
		slog.Bool("manual", out.IsManual))

	return record, nil
}

// checkConsent is the single gate protecting the "never message first" rule.
func (s *Sender) checkConsent(contact *domain.Contact) error {
	if contact.FirstContactAt == nil {
		return ErrNoConsent
	}
	if contact.OptedOut || contact.BlockedAt != nil ||
		contact.Status == domain.ContactUnsubscribed || contact.Status == domain.ContactBlocked {
		return ErrOptedOut
	}
	return nil
}

func (s *Sender) deliver(ctx context.Context, chatID string, out Outbound) (*whatsapp.SendResult, error) {
	switch out.Type {
	case domain.TemplateText:
		return s.provider.SendText(ctx, whatsapp.TextMessage{
			ChatID:      chatID,
			Text:        out.Text,
			LinkPreview: out.LinkPreview,
		})

	case domain.TemplateImage, domain.TemplateImageWithCaption:
		return s.provider.SendImage(ctx, s.mediaMessage(chatID, whatsapp.MediaImage, out))

	case domain.TemplateVideo, domain.TemplateVideoWithCaption:
		return s.provider.SendVideo(ctx, s.mediaMessage(chatID, whatsapp.MediaVideo, out))

	case domain.TemplateAudio:
		return s.provider.SendAudio(ctx, s.mediaMessage(chatID, whatsapp.MediaAudio, out))

	case domain.TemplateVoice:
		return s.provider.SendVoice(ctx, s.mediaMessage(chatID, whatsapp.MediaVoice, out))

	case domain.TemplateDocument:
		return s.provider.SendDocument(ctx, s.mediaMessage(chatID, whatsapp.MediaDocument, out))

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
