// Package realtime broadcasts server-side events to connected admin browsers
// over Server-Sent Events.
//
// SSE is chosen over WebSockets deliberately: the traffic is one-directional
// (server to dashboard), it survives proxies that mangle upgrades, and the
// browser reconnects on its own. Nothing here is durable — a client that
// misses events while disconnected reconciles by re-fetching the affected
// conversation.
package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// EventType names the kinds of updates the dashboard reacts to.
type EventType string

const (
	EventMessageCreated EventType = "message.created"
	EventMessageStatus  EventType = "message.status"
	EventChatUpdated    EventType = "chat.updated"
	EventContactUpdated EventType = "contact.updated"
	EventJobUpdated     EventType = "job.updated"
	EventCampaign       EventType = "campaign.updated"
	EventProviderState  EventType = "provider.state"
)

// Event is one broadcast payload.
type Event struct {
	Type      EventType `json:"type"`
	ContactID string    `json:"contact_id,omitempty"`
	Data      any       `json:"data,omitempty"`
	At        time.Time `json:"at"`
}

type subscriber struct {
	id     uint64
	ch     chan Event
	closed atomic.Bool
}

// Hub fans events out to every connected client.
type Hub struct {
	mu     sync.RWMutex
	subs   map[uint64]*subscriber
	nextID atomic.Uint64
	log    *slog.Logger

	dropped atomic.Uint64
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{
		subs: make(map[uint64]*subscriber),
		log:  log.With(slog.String("component", "realtime")),
	}
}

// Subscribe registers a client. The returned channel is closed when ctx ends
// or the hub shuts down; the caller must drain it until then.
func (h *Hub) Subscribe(ctx context.Context) <-chan Event {
	sub := &subscriber{
		id: h.nextID.Add(1),
		// A small buffer absorbs bursts (a campaign step firing for many
		// contacts at once) without blocking the publisher.
		ch: make(chan Event, 64),
	}

	h.mu.Lock()
	h.subs[sub.id] = sub
	count := len(h.subs)
	h.mu.Unlock()

	h.log.Debug("client subscribed", slog.Uint64("id", sub.id), slog.Int("clients", count))

	go func() {
		<-ctx.Done()
		h.unsubscribe(sub)
	}()

	return sub.ch
}

func (h *Hub) unsubscribe(sub *subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[sub.id]; ok {
		delete(h.subs, sub.id)
		// Mark closed before closing so a concurrent Publish cannot send on a
		// closed channel.
		sub.closed.Store(true)
		close(sub.ch)
	}
	h.mu.Unlock()
}

// Publish delivers an event to every subscriber.
//
// A client that cannot keep up loses the event rather than stalling the
// caller: correctness never depends on the stream, only responsiveness does.
func (h *Hub) Publish(event Event) {
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for _, sub := range h.subs {
		if sub.closed.Load() {
			continue
		}
		select {
		case sub.ch <- event:
		default:
			h.dropped.Add(1)
		}
	}
}

// PublishMessage announces a new message in a conversation.
func (h *Hub) PublishMessage(contactID uuid.UUID, payload any) {
	h.Publish(Event{Type: EventMessageCreated, ContactID: contactID.String(), Data: payload})
}

// PublishStatus announces a delivery-state change.
func (h *Hub) PublishStatus(contactID uuid.UUID, payload any) {
	h.Publish(Event{Type: EventMessageStatus, ContactID: contactID.String(), Data: payload})
}

// PublishChat announces that a chat-list row changed.
func (h *Hub) PublishChat(contactID uuid.UUID, payload any) {
	h.Publish(Event{Type: EventChatUpdated, ContactID: contactID.String(), Data: payload})
}

// Clients reports the number of connected dashboards.
func (h *Hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}

// Dropped reports how many events were discarded for slow clients, exposed on
// the health endpoint as a backpressure signal.
func (h *Hub) Dropped() uint64 { return h.dropped.Load() }

// Close disconnects every client.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, sub := range h.subs {
		sub.closed.Store(true)
		close(sub.ch)
		delete(h.subs, id)
	}
}

// Encode renders an event as an SSE frame.
func Encode(event Event) ([]byte, error) {
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}

	frame := make([]byte, 0, len(payload)+32)
	frame = append(frame, "event: "...)
	frame = append(frame, event.Type...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return frame, nil
}
