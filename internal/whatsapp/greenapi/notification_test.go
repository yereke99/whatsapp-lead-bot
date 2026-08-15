package greenapi

import (
	"encoding/json"
	"testing"
)

// An empty queue is the steady state while nobody is messaging the bot. It must
// read as "nothing to do", never as a failure, or the receiver would back off
// and treat an idle instance as an outage.
func TestDecodeNotificationEmptyQueue(t *testing.T) {
	for _, raw := range []string{"null", "  null  ", "\n", ""} {
		got, err := decodeNotification([]byte(raw), "receiveNotification")
		if err != nil {
			t.Errorf("decodeNotification(%q): unexpected error %v", raw, err)
		}
		if got != nil {
			t.Errorf("decodeNotification(%q): expected no notification, got %+v", raw, got)
		}
	}
}

func TestDecodeNotification(t *testing.T) {
	raw := []byte(`{
		"receiptId": 4217,
		"body": {
			"typeWebhook": "incomingMessageReceived",
			"idMessage": "ABC123",
			"senderData": {"chatId": "77011234567@c.us"},
			"messageData": {"typeMessage": "textMessage",
			                "textMessageData": {"textMessage": "Айран"}}
		}
	}`)

	got, err := decodeNotification(raw, "receiveNotification")
	if err != nil {
		t.Fatalf("decodeNotification: %v", err)
	}
	if got == nil {
		t.Fatal("expected a notification")
	}
	if got.ReceiptID != 4217 {
		t.Errorf("ReceiptID: got %d, want 4217", got.ReceiptID)
	}

	// The body must survive intact: it is handed straight to ParseWebhook.
	event, err := ParseWebhook(got.Body)
	if err != nil {
		t.Fatalf("ParseWebhook on the notification body: %v", err)
	}
	if event.Message == nil || event.Message.Text != "Айран" {
		t.Errorf("message text did not survive decoding: %+v", event.Message)
	}
	if event.ChatID != "77011234567@c.us" {
		t.Errorf("ChatID: got %q", event.ChatID)
	}
}

// Without a receipt id an entry can never be acknowledged, so it would block
// the queue forever. Rejecting it lets the receiver drop it deliberately.
func TestDecodeNotificationRejectsMissingReceiptID(t *testing.T) {
	if _, err := decodeNotification([]byte(`{"body":{"typeWebhook":"x"}}`), "receiveNotification"); err == nil {
		t.Error("expected an error when receiptId is absent")
	}
}

func TestDecodeNotificationRejectsMalformedJSON(t *testing.T) {
	if _, err := decodeNotification([]byte(`{"receiptId":`), "receiveNotification"); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

// ParseWebhook must mark unparseable payloads distinctly so a queue consumer
// can drop them instead of retrying forever.
func TestParseWebhookReportsParseErrors(t *testing.T) {
	for _, raw := range []string{`{`, `{"idMessage":"1"}`} {
		_, err := ParseWebhook([]byte(raw))
		if err == nil {
			t.Fatalf("ParseWebhook(%q): expected an error", raw)
		}
		var parseErr *ParseError
		if !asParseError(err, &parseErr) {
			t.Errorf("ParseWebhook(%q): error is not a *ParseError: %v", raw, err)
		}
	}
}

func asParseError(err error, target **ParseError) bool {
	pe, ok := err.(*ParseError)
	if ok {
		*target = pe
	}
	return ok
}

// The notification body is passed through verbatim, so a payload type the
// platform does not act on must still decode without error.
func TestDecodeNotificationKeepsUnknownTypes(t *testing.T) {
	raw := []byte(`{"receiptId": 9, "body": {"typeWebhook": "stateInstanceChanged", "stateInstance": "authorized"}}`)

	got, err := decodeNotification(raw, "receiveNotification")
	if err != nil {
		t.Fatalf("decodeNotification: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal(got.Body, &body); err != nil {
		t.Fatalf("body is not valid json: %v", err)
	}
	if body["typeWebhook"] != "stateInstanceChanged" {
		t.Errorf("body was altered: %v", body)
	}
}
