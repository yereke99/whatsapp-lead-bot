package greenapi

import (
	"testing"

	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

func TestParseIncomingTextMessage(t *testing.T) {
	body := []byte(`{
		"typeWebhook": "incomingMessageReceived",
		"instanceData": {"idInstance": 1101, "wid": "77010000000@c.us", "typeInstance": "whatsapp"},
		"timestamp": 1786000000,
		"idMessage": "BAE5F4886F6F2D05",
		"senderData": {
			"chatId": "77011234567@c.us",
			"sender": "77011234567@c.us",
			"senderName": "Alisher",
			"senderContactName": "Әлішер Сәрсенов"
		},
		"messageData": {
			"typeMessage": "textMessage",
			"textMessageData": {"textMessage": "Айран"}
		}
	}`)

	event, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}

	if event.Kind != whatsapp.EventIncomingMessage {
		t.Errorf("Kind = %v, want %v", event.Kind, whatsapp.EventIncomingMessage)
	}
	if event.ExternalID != "BAE5F4886F6F2D05" {
		t.Errorf("ExternalID = %q", event.ExternalID)
	}
	if event.ChatID != "77011234567@c.us" {
		t.Errorf("ChatID = %q", event.ChatID)
	}
	// The contact name is preferred over the push name.
	if event.SenderName != "Әлішер Сәрсенов" {
		t.Errorf("SenderName = %q", event.SenderName)
	}
	if event.Message.Type != whatsapp.TypeText || event.Message.Text != "Айран" {
		t.Errorf("Message = %+v", event.Message)
	}
	if event.Timestamp.Unix() != 1786000000 {
		t.Errorf("Timestamp = %v", event.Timestamp)
	}
}

func TestParseExtendedTextMessage(t *testing.T) {
	body := []byte(`{
		"typeWebhook": "incomingMessageReceived",
		"idMessage": "X1",
		"senderData": {"chatId": "77011234567@c.us"},
		"messageData": {
			"typeMessage": "extendedTextMessage",
			"extendedTextMessageData": {"text": "Сілтеме: https://example.com", "isForwarded": true}
		}
	}`)

	event, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if event.Message.Type != whatsapp.TypeText {
		t.Errorf("Type = %v, want TEXT", event.Message.Type)
	}
	if !event.Message.IsForwarded {
		t.Error("IsForwarded should be true")
	}
}

// TestParseAudioClassification checks the distinction that decides whether the
// admin UI shows a voice note or a music file.
func TestParseAudioClassification(t *testing.T) {
	cases := []struct {
		mime string
		want whatsapp.MessageType
	}{
		{"audio/ogg; codecs=opus", whatsapp.TypeVoice},
		{"audio/ogg", whatsapp.TypeVoice},
		{"audio/mpeg", whatsapp.TypeAudio},
		{"audio/mp4", whatsapp.TypeAudio},
	}

	for _, tc := range cases {
		body := []byte(`{
			"typeWebhook": "incomingMessageReceived",
			"idMessage": "A1",
			"senderData": {"chatId": "77011234567@c.us"},
			"messageData": {
				"typeMessage": "audioMessage",
				"fileMessageData": {"downloadUrl": "https://example.com/a", "mimeType": "` + tc.mime + `"}
			}
		}`)

		event, err := ParseWebhook(body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if event.Message.Type != tc.want {
			t.Errorf("mime %q classified as %v, want %v", tc.mime, event.Message.Type, tc.want)
		}
	}
}

func TestParseImageWithCaption(t *testing.T) {
	body := []byte(`{
		"typeWebhook": "incomingMessageReceived",
		"idMessage": "IMG1",
		"senderData": {"chatId": "77011234567@c.us"},
		"messageData": {
			"typeMessage": "imageMessage",
			"fileMessageData": {
				"downloadUrl": "https://media.green-api.com/x.jpg",
				"caption": "Мынау сурет",
				"fileName": "x.jpg",
				"mimeType": "image/jpeg"
			}
		}
	}`)

	event, err := ParseWebhook(body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}

	msg := event.Message
	if msg.Type != whatsapp.TypeImage {
		t.Errorf("Type = %v", msg.Type)
	}
	if msg.Text != "Мынау сурет" {
		t.Errorf("caption not carried into Text: %q", msg.Text)
	}
	if msg.DownloadURL == "" || msg.FileName != "x.jpg" {
		t.Errorf("file metadata missing: %+v", msg)
	}
	if !msg.Type.IsMedia() {
		t.Error("image should report as media")
	}
}

func TestParseInteractiveSelectionsAsText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "simple button",
			body: `{
				"typeWebhook": "incomingMessageReceived",
				"idMessage": "BTN1",
				"senderData": {"chatId": "77011234567@c.us"},
				"messageData": {
					"typeMessage": "buttonsResponseMessage",
					"buttonsResponseMessage": {
						"stanzaId": "ORIG1",
						"selectedButtonId": "1",
						"selectedButtonText": "Айран"
					}
				}
			}`,
			want: "Айран",
		},
		{
			name: "list item",
			body: `{
				"typeWebhook": "incomingMessageReceived",
				"idMessage": "LIST1",
				"senderData": {"chatId": "77011234567@c.us"},
				"messageData": {
					"typeMessage": "listResponseMessage",
					"listResponseMessage": {
						"stanzaId": "ORIG2",
						"title": "Қаймақ",
						"singleSelectReply": "cream"
					}
				}
			}`,
			want: "Қаймақ",
		},
		{
			name: "template button",
			body: `{
				"typeWebhook": "incomingMessageReceived",
				"idMessage": "TPLBTN1",
				"senderData": {"chatId": "77011234567@c.us"},
				"messageData": {
					"typeMessage": "templateButtonsReplyMessage",
					"templateButtonReplyMessage": {
						"stanzaId": "ORIG3",
						"selectedId": "free-lesson",
						"selectedDisplayText": "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"
					}
				}
			}`,
			want: "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, err := ParseWebhook([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseWebhook: %v", err)
			}
			if event.Message.Type != whatsapp.TypeText {
				t.Errorf("Type = %v, want TEXT", event.Message.Type)
			}
			if event.Message.Text != tc.want {
				t.Errorf("Text = %q, want %q", event.Message.Text, tc.want)
			}
			if event.Message.QuotedID == "" {
				t.Error("button selection should keep the source stanza id")
			}
		})
	}
}

func TestParseStatusWebhook(t *testing.T) {
	cases := map[string]whatsapp.DeliveryStatus{
		"sent":       whatsapp.StatusSent,
		"delivered":  whatsapp.StatusDelivered,
		"read":       whatsapp.StatusRead,
		"failed":     whatsapp.StatusFailed,
		"noAccount":  whatsapp.StatusFailed,
		"notInGroup": whatsapp.StatusFailed,
	}

	for raw, want := range cases {
		body := []byte(`{
			"typeWebhook": "outgoingMessageStatus",
			"idMessage": "OUT1",
			"chatId": "77011234567@c.us",
			"status": "` + raw + `"
		}`)

		event, err := ParseWebhook(body)
		if err != nil {
			t.Fatalf("ParseWebhook: %v", err)
		}
		if event.Kind != whatsapp.EventOutgoingStatus {
			t.Errorf("Kind = %v", event.Kind)
		}
		if event.Status.Status != want {
			t.Errorf("status %q mapped to %v, want %v", raw, event.Status.Status, want)
		}
	}
}

// TestDedupeKeyDistinguishesStatuses is what allows sent, delivered and read
// notifications for one message to all be processed while genuine replays are
// dropped.
func TestDedupeKeyDistinguishesStatuses(t *testing.T) {
	makeStatus := func(status string) string {
		return `{"typeWebhook":"outgoingMessageStatus","idMessage":"SAME","status":"` + status + `"}`
	}

	sent, _ := ParseWebhook([]byte(makeStatus("sent")))
	delivered, _ := ParseWebhook([]byte(makeStatus("delivered")))
	sentAgain, _ := ParseWebhook([]byte(makeStatus("sent")))

	if sent.DedupeKey == delivered.DedupeKey {
		t.Error("different statuses must produce different dedupe keys")
	}
	if sent.DedupeKey != sentAgain.DedupeKey {
		t.Error("a replayed identical status must produce the same dedupe key")
	}
}

func TestDedupeKeyForMessages(t *testing.T) {
	body := `{"typeWebhook":"incomingMessageReceived","idMessage":"M1","senderData":{"chatId":"7@c.us"},"messageData":{"typeMessage":"textMessage","textMessageData":{"textMessage":"hi"}}}`

	first, _ := ParseWebhook([]byte(body))
	second, _ := ParseWebhook([]byte(body))

	if first.DedupeKey != second.DedupeKey {
		t.Error("identical deliveries must share a dedupe key")
	}
	if first.DedupeKey != "incomingMessageReceived:M1" {
		t.Errorf("DedupeKey = %q", first.DedupeKey)
	}
}

// TestDedupeKeyWithoutMessageID falls back to hashing the body so an event
// with no id still cannot be processed twice.
func TestDedupeKeyWithoutMessageID(t *testing.T) {
	body := `{"typeWebhook":"stateInstanceChanged","stateInstance":"authorized"}`

	first, err := ParseWebhook([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := ParseWebhook([]byte(body))

	if first.DedupeKey != second.DedupeKey {
		t.Error("identical bodies must hash to the same key")
	}

	other, _ := ParseWebhook([]byte(`{"typeWebhook":"stateInstanceChanged","stateInstance":"notAuthorized"}`))
	if first.DedupeKey == other.DedupeKey {
		t.Error("different bodies must hash differently")
	}
}

func TestParseStateChange(t *testing.T) {
	event, err := ParseWebhook([]byte(`{"typeWebhook":"stateInstanceChanged","stateInstance":"authorized"}`))
	if err != nil {
		t.Fatal(err)
	}
	if event.Kind != whatsapp.EventStateChanged || event.State != "authorized" {
		t.Errorf("event = %+v", event)
	}
}

func TestParseUnknownWebhookType(t *testing.T) {
	event, err := ParseWebhook([]byte(`{"typeWebhook":"somethingNew","idMessage":"Z1"}`))
	if err != nil {
		t.Fatalf("an unknown type should parse, not error: %v", err)
	}
	if event.Kind != whatsapp.EventUnknown {
		t.Errorf("Kind = %v, want UNKNOWN", event.Kind)
	}
}

func TestParseRejectsMalformedPayloads(t *testing.T) {
	cases := [][]byte{
		[]byte(``),
		[]byte(`not json`),
		[]byte(`{}`),
		[]byte(`{"foo":"bar"}`),
	}

	for _, body := range cases {
		if _, err := ParseWebhook(body); err == nil {
			t.Errorf("ParseWebhook(%q) should have failed", body)
		}
	}
}

func TestDeliveryStatusRank(t *testing.T) {
	// Ranking is what stops a late SENT webhook from undoing a READ.
	if whatsapp.StatusRead.Rank() <= whatsapp.StatusDelivered.Rank() {
		t.Error("READ must outrank DELIVERED")
	}
	if whatsapp.StatusDelivered.Rank() <= whatsapp.StatusSent.Rank() {
		t.Error("DELIVERED must outrank SENT")
	}
	if whatsapp.DeliveryStatus("").Rank() != 0 {
		t.Error("an unknown status must rank lowest")
	}
}
