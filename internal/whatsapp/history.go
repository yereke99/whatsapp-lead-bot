package whatsapp

import "context"

// HistoryProvider is optional: an unavailable or incomplete journal never
// authorizes replaying an old trigger.
type HistoryProvider interface {
	ReplyHistory(context.Context, string) (*ReplyHistory, error)
}

type HistoryMessage struct {
	Type       string `json:"type"`
	ExternalID string `json:"idMessage"`
	Timestamp  int64  `json:"timestamp"`
	ChatID     string `json:"chatId"`
	Status     string `json:"statusMessage"`
}

type ReplyHistory struct {
	PhoneRepliesRecorded bool
	Messages             []HistoryMessage
}
