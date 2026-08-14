// Package whatsapp defines the transport-agnostic contract the automation
// engine uses to reach WhatsApp, plus the shared value types.
//
// Business logic depends on Provider only. Green API specifics live in the
// greenapi sub-package, so a second provider can be added without touching the
// scheduler, campaigns or webhook pipeline.
package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// MediaKind classifies an outbound attachment.
type MediaKind string

const (
	MediaImage    MediaKind = "IMAGE"
	MediaVideo    MediaKind = "VIDEO"
	MediaAudio    MediaKind = "AUDIO"
	MediaVoice    MediaKind = "VOICE"
	MediaDocument MediaKind = "DOCUMENT"
)

// TextMessage is a plain text send.
type TextMessage struct {
	ChatID      string
	Text        string
	LinkPreview bool
	QuotedID    string
}

// MediaMessage is an attachment send. Caption, when set, is delivered as part
// of the same WhatsApp message rather than as a follow-up text.
//
// Exactly one source must be provided: FilePath for a local file that gets
// uploaded, or URL for a file the provider fetches itself.
type MediaMessage struct {
	ChatID   string
	Kind     MediaKind
	Caption  string
	FileName string
	MimeType string
	FilePath string
	URL      string
	QuotedID string
}

func (m MediaMessage) Validate() error {
	if strings.TrimSpace(m.ChatID) == "" {
		return errors.New("chat id is required")
	}
	if m.FilePath == "" && m.URL == "" {
		return errors.New("either a file path or a url is required")
	}
	if m.FilePath != "" && m.URL != "" {
		return errors.New("file path and url are mutually exclusive")
	}
	if strings.TrimSpace(m.FileName) == "" {
		return errors.New("file name is required")
	}
	// Captions are only meaningful on visual media; WhatsApp silently drops
	// them elsewhere, and a silently dropped caption is a lost message.
	if m.Caption != "" && m.Kind != MediaImage && m.Kind != MediaVideo && m.Kind != MediaDocument {
		return fmt.Errorf("%s messages cannot carry a caption", m.Kind)
	}
	return nil
}

// SendResult carries the provider's acknowledgement.
type SendResult struct {
	// ExternalID is the provider message id, used to correlate later status
	// webhooks and to deduplicate.
	ExternalID string
	Raw        map[string]any
}

// InstanceState describes provider connectivity.
type InstanceState struct {
	State        string `json:"stateInstance"`
	Authorized   bool   `json:"-"`
	PhoneNumber  string `json:"-"`
	DeviceID     string `json:"-"`
	RawStateJSON string `json:"-"`
}

// Provider is the WhatsApp transport contract.
type Provider interface {
	Name() string

	SendText(ctx context.Context, msg TextMessage) (*SendResult, error)
	SendImage(ctx context.Context, msg MediaMessage) (*SendResult, error)
	SendVideo(ctx context.Context, msg MediaMessage) (*SendResult, error)
	SendAudio(ctx context.Context, msg MediaMessage) (*SendResult, error)
	SendVoice(ctx context.Context, msg MediaMessage) (*SendResult, error)
	SendDocument(ctx context.Context, msg MediaMessage) (*SendResult, error)

	// SendMediaWithCaption dispatches on Kind and delivers the caption inside
	// the same WhatsApp message.
	SendMediaWithCaption(ctx context.Context, msg MediaMessage) (*SendResult, error)

	// CheckNumber reports whether a phone number has WhatsApp.
	CheckNumber(ctx context.Context, phone string) (bool, error)

	// State reports the provider instance's connectivity.
	State(ctx context.Context) (*InstanceState, error)
}

// Error is a provider failure classified for the retry engine.
type Error struct {
	Provider   string
	Op         string
	StatusCode int
	Message    string
	Body       string
	// Retryable distinguishes transient faults (timeouts, 5xx, rate limits)
	// from permanent ones (invalid recipient, bad credentials). Permanent
	// failures must not consume the retry budget.
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", e.Provider, e.Op)
	if e.StatusCode > 0 {
		fmt.Fprintf(&b, " (http %d)", e.StatusCode)
	}
	if e.Message != "" {
		fmt.Fprintf(&b, ": %s", e.Message)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether the automation engine should schedule a retry.
// Unclassified errors are treated as retryable: a message delayed by a bug is
// recoverable, a message dropped by one is not.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var provErr *Error
	if errors.As(err, &provErr) {
		return provErr.Retryable
	}
	return true
}

// RetryableStatus classifies an HTTP status code from any provider.
func RetryableStatus(code int) bool {
	switch {
	case code == http.StatusTooManyRequests:
		return true
	case code == http.StatusRequestTimeout:
		return true
	case code >= 500:
		return true
	default:
		return false
	}
}

// ErrNotConfigured is returned when provider credentials are absent. The
// platform boots without them so the admin panel remains usable, but any send
// attempt fails loudly instead of silently succeeding.
var ErrNotConfigured = errors.New("whatsapp provider is not configured")
