package greenapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

// webhookPayload covers every Green API notification shape the platform
// handles. Fields absent from a given notification simply stay zero.
type webhookPayload struct {
	TypeWebhook  string `json:"typeWebhook"`
	Timestamp    int64  `json:"timestamp"`
	IDMessage    string `json:"idMessage"`
	InstanceData struct {
		IDInstance   json.Number `json:"idInstance"`
		Wid          string      `json:"wid"`
		TypeInstance string      `json:"typeInstance"`
	} `json:"instanceData"`

	SenderData struct {
		ChatID            string `json:"chatId"`
		ChatName          string `json:"chatName"`
		Sender            string `json:"sender"`
		SenderName        string `json:"senderName"`
		SenderContactName string `json:"senderContactName"`
	} `json:"senderData"`

	MessageData *messageData `json:"messageData"`

	// outgoingMessageStatus
	Status      string `json:"status"`
	Description string `json:"description"`
	ChatID      string `json:"chatId"`
	SendByAPI   bool   `json:"sendByApi"`

	// stateInstanceChanged
	StateInstance string `json:"stateInstance"`

	// incomingCall
	From string `json:"from"`
}

type messageData struct {
	TypeMessage     string `json:"typeMessage"`
	TextMessageData *struct {
		TextMessage string `json:"textMessage"`
	} `json:"textMessageData"`
	ExtendedTextMessageData *struct {
		Text        string `json:"text"`
		Description string `json:"description"`
		Title       string `json:"title"`
		IsForwarded bool   `json:"isForwarded"`
	} `json:"extendedTextMessageData"`
	FileMessageData *struct {
		DownloadURL string `json:"downloadUrl"`
		Caption     string `json:"caption"`
		FileName    string `json:"fileName"`
		MimeType    string `json:"mimeType"`
		IsAnimated  bool   `json:"isAnimated"`
		IsForwarded bool   `json:"isForwarded"`
	} `json:"fileMessageData"`
	LocationMessageData *struct {
		NameLocation string  `json:"nameLocation"`
		Address      string  `json:"address"`
		Latitude     float64 `json:"latitude"`
		Longitude    float64 `json:"longitude"`
	} `json:"locationMessageData"`
	ContactMessageData *struct {
		DisplayName string `json:"displayName"`
		VCard       string `json:"vcard"`
	} `json:"contactMessageData"`
	PollMessageData *struct {
		Name    string `json:"name"`
		Options []struct {
			OptionName string `json:"optionName"`
		} `json:"options"`
	} `json:"pollMessageData"`
	ButtonsResponseMessage *struct {
		SelectedButtonID   string `json:"selectedButtonId"`
		SelectedButtonText string `json:"selectedButtonText"`
		StanzaID           string `json:"stanzaId"`
	} `json:"buttonsResponseMessage"`
	ListResponseMessage *struct {
		Title             string `json:"title"`
		SingleSelectReply string `json:"singleSelectReply"`
		StanzaID          string `json:"stanzaId"`
	} `json:"listResponseMessage"`
	TemplateButtonReplyMessage *struct {
		SelectedID          string `json:"selectedId"`
		SelectedDisplayText string `json:"selectedDisplayText"`
		StanzaID            string `json:"stanzaId"`
	} `json:"templateButtonReplyMessage"`
	QuotedMessage *struct {
		StanzaID string `json:"stanzaId"`
	} `json:"quotedMessage"`
}

// ParseWebhook converts a raw Green API notification body into the neutral
// event shape. It never returns a partially populated event: either the
// payload is understood, or an error explains why it was rejected.
func ParseWebhook(body []byte) (*whatsapp.Event, error) {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}
	if strings.TrimSpace(payload.TypeWebhook) == "" {
		return nil, fmt.Errorf("webhook payload has no typeWebhook field")
	}

	event := &whatsapp.Event{
		RawType:    payload.TypeWebhook,
		ExternalID: strings.TrimSpace(payload.IDMessage),
		Timestamp:  parseTimestamp(payload.Timestamp),
		Raw:        json.RawMessage(body),
	}

	switch payload.TypeWebhook {
	case "incomingMessageReceived":
		event.Kind = whatsapp.EventIncomingMessage
		event.ChatID = strings.TrimSpace(payload.SenderData.ChatID)
		event.SenderID = strings.TrimSpace(payload.SenderData.Sender)
		event.SenderName = pickName(payload)
		event.Message = parseMessageData(payload.MessageData)

	case "outgoingMessageReceived", "outgoingAPIMessageReceived":
		event.Kind = whatsapp.EventOutgoingMessage
		event.ChatID = strings.TrimSpace(payload.SenderData.ChatID)
		event.SenderID = strings.TrimSpace(payload.SenderData.Sender)
		event.SenderName = pickName(payload)
		event.Message = parseMessageData(payload.MessageData)

	case "outgoingMessageStatus":
		event.Kind = whatsapp.EventOutgoingStatus
		event.ChatID = strings.TrimSpace(payload.ChatID)
		event.Status = &whatsapp.StatusUpdate{
			ExternalID:  event.ExternalID,
			Status:      mapStatus(payload.Status),
			Description: strings.TrimSpace(payload.Description),
		}

	case "stateInstanceChanged":
		event.Kind = whatsapp.EventStateChanged
		event.State = payload.StateInstance

	case "incomingCall":
		event.Kind = whatsapp.EventIncomingCall
		event.ChatID = strings.TrimSpace(payload.From)
		event.SenderID = event.ChatID
		event.State = payload.Status

	default:
		event.Kind = whatsapp.EventUnknown
	}

	event.DedupeKey = dedupeKey(payload, body)
	return event, nil
}

// dedupeKey identifies one logical delivery.
//
// Message notifications are keyed by their provider message id. Status
// notifications reuse that id across the sent/delivered/read progression, so
// the status value joins the key. Anything without an id falls back to a hash
// of the body, which still collapses byte-identical retries.
func dedupeKey(p webhookPayload, body []byte) string {
	id := strings.TrimSpace(p.IDMessage)

	switch p.TypeWebhook {
	case "outgoingMessageStatus":
		if id != "" {
			return fmt.Sprintf("%s:%s:%s", p.TypeWebhook, id, strings.ToLower(strings.TrimSpace(p.Status)))
		}
	default:
		if id != "" {
			return fmt.Sprintf("%s:%s", p.TypeWebhook, id)
		}
	}

	sum := sha256.Sum256(body)
	return fmt.Sprintf("%s:sha256:%s", p.TypeWebhook, hex.EncodeToString(sum[:]))
}

func parseMessageData(md *messageData) *whatsapp.InboundMessage {
	if md == nil {
		return &whatsapp.InboundMessage{Type: whatsapp.TypeUnknown}
	}

	msg := &whatsapp.InboundMessage{Type: whatsapp.TypeUnknown}
	if md.QuotedMessage != nil {
		msg.QuotedID = md.QuotedMessage.StanzaID
	}

	switch md.TypeMessage {
	case "textMessage":
		msg.Type = whatsapp.TypeText
		if md.TextMessageData != nil {
			msg.Text = md.TextMessageData.TextMessage
		}
		// Some clients deliver plain text through the extended shape.
		if msg.Text == "" && md.ExtendedTextMessageData != nil {
			msg.Text = md.ExtendedTextMessageData.Text
		}

	case "extendedTextMessage", "quotedMessage":
		msg.Type = whatsapp.TypeText
		if md.ExtendedTextMessageData != nil {
			msg.Text = md.ExtendedTextMessageData.Text
			msg.IsForwarded = md.ExtendedTextMessageData.IsForwarded
		}
		if msg.Text == "" && md.TextMessageData != nil {
			msg.Text = md.TextMessageData.TextMessage
		}

	case "imageMessage", "videoMessage", "documentMessage", "audioMessage", "stickerMessage":
		applyFileData(msg, md)

	case "locationMessage":
		msg.Type = whatsapp.TypeLocation
		if md.LocationMessageData != nil {
			parts := []string{md.LocationMessageData.NameLocation, md.LocationMessageData.Address}
			label := strings.TrimSpace(strings.Join(trimEmpty(parts), ", "))
			msg.Text = strings.TrimSpace(fmt.Sprintf("%s (%.6f, %.6f)",
				label, md.LocationMessageData.Latitude, md.LocationMessageData.Longitude))
		}

	case "contactMessage", "contactsArrayMessage":
		msg.Type = whatsapp.TypeContact
		if md.ContactMessageData != nil {
			msg.Text = md.ContactMessageData.DisplayName
		}

	case "pollMessage", "pollUpdateMessage":
		msg.Type = whatsapp.TypePoll
		if md.PollMessageData != nil {
			options := make([]string, 0, len(md.PollMessageData.Options))
			for _, o := range md.PollMessageData.Options {
				options = append(options, o.OptionName)
			}
			msg.Text = strings.TrimSpace(md.PollMessageData.Name + ": " + strings.Join(options, " / "))
		}

	case "reactionMessage":
		msg.Type = whatsapp.TypeReaction
		if md.ExtendedTextMessageData != nil {
			msg.Text = md.ExtendedTextMessageData.Text
		}

	case "buttonsResponseMessage":
		msg.Type = whatsapp.TypeText
		if md.ButtonsResponseMessage != nil {
			msg.Text = firstNonBlank(md.ButtonsResponseMessage.SelectedButtonText, md.ButtonsResponseMessage.SelectedButtonID)
			if msg.QuotedID == "" {
				msg.QuotedID = md.ButtonsResponseMessage.StanzaID
			}
		}

	case "listResponseMessage":
		msg.Type = whatsapp.TypeText
		if md.ListResponseMessage != nil {
			msg.Text = firstNonBlank(md.ListResponseMessage.Title, md.ListResponseMessage.SingleSelectReply)
			if msg.QuotedID == "" {
				msg.QuotedID = md.ListResponseMessage.StanzaID
			}
		}

	case "templateButtonsReplyMessage":
		msg.Type = whatsapp.TypeText
		if md.TemplateButtonReplyMessage != nil {
			msg.Text = firstNonBlank(md.TemplateButtonReplyMessage.SelectedDisplayText, md.TemplateButtonReplyMessage.SelectedID)
			if msg.QuotedID == "" {
				msg.QuotedID = md.TemplateButtonReplyMessage.StanzaID
			}
		}
	}

	msg.Text = strings.TrimSpace(msg.Text)
	return msg
}

func applyFileData(msg *whatsapp.InboundMessage, md *messageData) {
	if md.FileMessageData != nil {
		msg.Text = md.FileMessageData.Caption
		msg.FileName = md.FileMessageData.FileName
		msg.MimeType = md.FileMessageData.MimeType
		msg.DownloadURL = md.FileMessageData.DownloadURL
		msg.IsForwarded = md.FileMessageData.IsForwarded
	}

	switch md.TypeMessage {
	case "imageMessage":
		msg.Type = whatsapp.TypeImage
	case "videoMessage":
		msg.Type = whatsapp.TypeVideo
	case "stickerMessage":
		msg.Type = whatsapp.TypeSticker
	case "documentMessage":
		msg.Type = whatsapp.TypeDocument
	case "audioMessage":
		// WhatsApp voice notes are OGG/Opus; anything else the user attached
		// from their music library is an ordinary audio file.
		if strings.Contains(strings.ToLower(msg.MimeType), "ogg") {
			msg.Type = whatsapp.TypeVoice
		} else {
			msg.Type = whatsapp.TypeAudio
		}
	}
}

func mapStatus(raw string) whatsapp.DeliveryStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "sent":
		return whatsapp.StatusSent
	case "delivered":
		return whatsapp.StatusDelivered
	case "read":
		return whatsapp.StatusRead
	case "failed", "noaccount", "notingroup", "yellowcard":
		return whatsapp.StatusFailed
	default:
		return ""
	}
}

func pickName(p webhookPayload) string {
	for _, candidate := range []string{
		p.SenderData.SenderContactName,
		p.SenderData.SenderName,
		p.SenderData.ChatName,
	} {
		if v := strings.TrimSpace(candidate); v != "" {
			return v
		}
	}
	return ""
}

func parseTimestamp(unix int64) time.Time {
	if unix <= 0 {
		return time.Now().UTC()
	}
	return time.Unix(unix, 0).UTC()
}

func trimEmpty(in []string) []string {
	out := in[:0]
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			out = append(out, strings.TrimSpace(v))
		}
	}
	return out
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
