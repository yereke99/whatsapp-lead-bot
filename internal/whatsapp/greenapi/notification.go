package greenapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

// Notification is one entry from the Green API queue.
//
// Green API holds inbound events in a per-instance FIFO queue and hands them
// out one at a time. The same entry keeps being returned until it is deleted by
// its receipt id, which is what makes the queue reliable: nothing is lost if the
// process dies mid-processing.
type Notification struct {
	ReceiptID int64
	// Body carries the event itself, in exactly the shape the webhook used to
	// POST, so ParseWebhook consumes it unchanged.
	Body json.RawMessage
}

// notificationEnvelope is the wire format of receiveNotification.
type notificationEnvelope struct {
	ReceiptID int64           `json:"receiptId"`
	Body      json.RawMessage `json:"body"`
}

// ReceiveNotification returns the oldest queued notification, or nil when the
// queue is empty.
//
// The call is a long poll: Green API holds the connection for up to
// receiveTimeout seconds waiting for something to arrive, then answers with a
// literal JSON null. An empty queue is the normal idle state, not an error.
func (c *Client) ReceiveNotification(ctx context.Context, receiveTimeout time.Duration) (*Notification, error) {
	const op = "receiveNotification"
	if err := c.ready(op); err != nil {
		return nil, err
	}

	seconds := int(receiveTimeout.Seconds())
	switch {
	case seconds < 5:
		// Green API's own floor; anything lower is rejected.
		seconds = 5
	case seconds > 60:
		seconds = 60
	}

	url := c.endpoint(c.baseURL, op) + "?receiveTimeout=" + strconv.Itoa(seconds)

	raw, err := c.doRequest(ctx, http.MethodGet, url, "", nil, op)
	if err != nil {
		return nil, err
	}
	return decodeNotification(raw, op)
}

func decodeNotification(raw []byte, op string) (*Notification, error) {
	if isJSONNull(raw) {
		return nil, nil
	}

	var envelope notificationEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &whatsapp.Error{
			Provider: providerName, Op: op, Retryable: false,
			Err: fmt.Errorf("decode notification: %w", err),
		}
	}

	// A receipt id is the only way to acknowledge an entry. Without one the
	// notification could never be deleted, so treat it as malformed.
	if envelope.ReceiptID == 0 {
		return nil, &whatsapp.Error{
			Provider: providerName, Op: op, Retryable: false,
			Err: fmt.Errorf("notification has no receiptId"),
		}
	}

	return &Notification{ReceiptID: envelope.ReceiptID, Body: envelope.Body}, nil
}

// isJSONNull reports whether the payload is an empty queue response.
func isJSONNull(raw []byte) bool {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case 'n':
			return string(trimJSONSpace(raw)) == "null"
		default:
			return false
		}
	}
	return true // empty body
}

func trimJSONSpace(raw []byte) []byte {
	start, end := 0, len(raw)
	for start < end && isSpaceByte(raw[start]) {
		start++
	}
	for end > start && isSpaceByte(raw[end-1]) {
		end--
	}
	return raw[start:end]
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\r' || b == '\n'
}

// DeleteNotification acknowledges a notification so the queue moves on.
//
// Until this succeeds Green API keeps returning the same entry, so a failure
// here means the event is seen again rather than lost. The processor's
// deduplication is what makes that replay harmless.
func (c *Client) DeleteNotification(ctx context.Context, receiptID int64) error {
	const op = "deleteNotification"
	if err := c.ready(op); err != nil {
		return err
	}
	if receiptID <= 0 {
		return fmt.Errorf("%s: invalid receipt id %d", op, receiptID)
	}

	url := c.endpoint(c.baseURL, op) + "/" + strconv.FormatInt(receiptID, 10)

	if _, err := c.doRequest(ctx, http.MethodDelete, url, "", nil, op); err != nil {
		return err
	}

	c.log.Debug("notification deleted", slog.Int64("receipt_id", receiptID))
	return nil
}
