package greenapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

// ReplyHistory reads both the journal and its phone-message capture setting.
// It never changes provider settings or acknowledges notifications.
func (c *Client) ReplyHistory(ctx context.Context, chatID string) (*whatsapp.ReplyHistory, error) {
	if err := c.ready("getChatHistory"); err != nil {
		return nil, err
	}
	raw, err := c.do(ctx, http.MethodGet, c.endpoint(c.baseURL, "getSettings"), "", nil, "getSettings")
	if err != nil {
		return nil, err
	}
	var settings struct {
		OutgoingMessageWebhook string `json:"outgoingMessageWebhook"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, wrapLocal("getSettings", err)
	}
	raw, err = c.do(ctx, http.MethodPost, c.endpoint(c.baseURL, "getChatHistory"), "application/json",
		mustJSON(map[string]any{"chatId": chatID, "count": 1000}), "getChatHistory")
	if err != nil {
		return nil, err
	}
	history := &whatsapp.ReplyHistory{PhoneRepliesRecorded: settings.OutgoingMessageWebhook == "yes"}
	if err := json.Unmarshal(raw, &history.Messages); err != nil {
		return nil, wrapLocal("getChatHistory", err)
	}
	return history, nil
}
