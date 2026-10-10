package messaging

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

const (
	ReplyAnswered   = "replied"
	ReplyUnanswered = "eligible"
	ReplyUnknown    = "ambiguous"
	// Allow for notification transit and the provider journal's publication lag.
	HistoricalReplyAge = 5 * time.Minute
)

var ErrAlreadyAnswered = errors.New("trigger already answered")
var ErrSendWindowExpired = errors.New("send window expired")

type ReplyHistory struct {
	providers ProviderRegistry
	messages  *conversations.Repository
}

func NewReplyHistory(providers ProviderRegistry, messages *conversations.Repository) *ReplyHistory {
	return &ReplyHistory{providers: providers, messages: messages}
}

// Check uses positive local receipts first. A negative local lookup is not
// proof: the operator may have answered from the phone while we were offline.
// The journal must include this exact incoming message and capture phone
// replies; missing coverage, pending sends and provider errors require review.
func (h *ReplyHistory) Check(ctx context.Context, instanceID int64, contactID uuid.UUID, chatID, externalID string, at time.Time) (string, error) {
	answered, err := h.messages.AnsweredSince(ctx, nil, instanceID, contactID, at)
	if err != nil {
		return "", err
	}
	if answered {
		return ReplyAnswered, nil
	}
	if h.providers == nil || externalID == "" {
		return ReplyUnknown, nil
	}
	provider, err := h.providers.ProviderFor(instanceID)
	if err != nil {
		return ReplyUnknown, nil
	}
	reader, ok := provider.(whatsapp.HistoryProvider)
	if !ok {
		return ReplyUnknown, nil
	}
	history, err := reader.ReplyHistory(ctx, chatID)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil || history == nil {
		return ReplyUnknown, nil
	}
	anchor, uncertain := false, false
	for _, m := range history.Messages {
		if m.ChatID != chatID {
			continue
		}
		if m.Type == "incoming" && m.ExternalID == externalID && m.Timestamp == at.Unix() {
			anchor = true
		}
		if m.Type != "outgoing" || m.Timestamp < at.Unix() {
			continue
		}
		switch m.Status {
		case "sent", "delivered", "read":
			return ReplyAnswered, nil
		case "failed", "noAccount":
		default:
			uncertain = true
		}
	}
	if !anchor || !history.PhoneRepliesRecorded || uncertain || time.Since(at) < HistoricalReplyAge {
		return ReplyUnknown, nil
	}
	return ReplyUnanswered, nil
}

func ManualReviewError() error {
	return &whatsapp.Error{Op: "manualReview", Message: "historical trigger reply cannot be verified; manual review required"}
}
