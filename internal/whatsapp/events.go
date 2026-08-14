package whatsapp

import (
	"encoding/json"
	"time"
)

// EventKind is the provider-neutral classification of an inbound webhook.
type EventKind string

const (
	EventIncomingMessage EventKind = "INCOMING_MESSAGE"
	// EventOutgoingMessage is an echo of a message the operator sent from the
	// linked phone rather than through the API. Recording it keeps the admin
	// conversation view faithful to what the customer actually sees.
	EventOutgoingMessage EventKind = "OUTGOING_MESSAGE"
	EventOutgoingStatus  EventKind = "OUTGOING_STATUS"
	EventStateChanged    EventKind = "STATE_CHANGED"
	EventIncomingCall    EventKind = "INCOMING_CALL"
	EventUnknown         EventKind = "UNKNOWN"
)

// MessageType mirrors the message type vocabulary stored in the database.
type MessageType string

const (
	TypeText     MessageType = "TEXT"
	TypeImage    MessageType = "IMAGE"
	TypeVideo    MessageType = "VIDEO"
	TypeAudio    MessageType = "AUDIO"
	TypeVoice    MessageType = "VOICE"
	TypeDocument MessageType = "DOCUMENT"
	TypeSticker  MessageType = "STICKER"
	TypeLocation MessageType = "LOCATION"
	TypeContact  MessageType = "CONTACT"
	TypePoll     MessageType = "POLL"
	TypeReaction MessageType = "REACTION"
	TypeUnknown  MessageType = "UNKNOWN"
)

// IsMedia reports whether the type carries a file.
func (t MessageType) IsMedia() bool {
	switch t {
	case TypeImage, TypeVideo, TypeAudio, TypeVoice, TypeDocument, TypeSticker:
		return true
	}
	return false
}

// DeliveryStatus is the normalized lifecycle state of an outbound message.
type DeliveryStatus string

const (
	StatusSent      DeliveryStatus = "SENT"
	StatusDelivered DeliveryStatus = "DELIVERED"
	StatusRead      DeliveryStatus = "READ"
	StatusFailed    DeliveryStatus = "FAILED"
)

// Rank orders statuses so a late-arriving webhook cannot move a message
// backwards from READ to SENT.
func (s DeliveryStatus) Rank() int {
	switch s {
	case StatusSent:
		return 1
	case StatusDelivered:
		return 2
	case StatusRead:
		return 3
	case StatusFailed:
		return 4
	default:
		return 0
	}
}

// InboundMessage is the content of a received or echoed message.
type InboundMessage struct {
	Type        MessageType
	Text        string
	FileName    string
	MimeType    string
	DownloadURL string
	IsForwarded bool
	QuotedID    string
}

// StatusUpdate reports a delivery state transition for a message the platform
// previously sent.
type StatusUpdate struct {
	ExternalID  string
	Status      DeliveryStatus
	Description string
}

// Event is the provider-neutral shape the webhook pipeline consumes.
type Event struct {
	Kind EventKind
	// DedupeKey uniquely identifies this delivery. Replays of the same event
	// carry the same key and are dropped by a unique index.
	DedupeKey  string
	ExternalID string
	RawType    string
	Timestamp  time.Time
	ChatID     string
	SenderID   string
	SenderName string
	Message    *InboundMessage
	Status     *StatusUpdate
	State      string
	Raw        json.RawMessage
}
