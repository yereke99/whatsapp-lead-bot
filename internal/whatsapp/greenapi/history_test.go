package greenapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
)

func TestReplyHistoryReadsPhoneCaptureAndChatJournal(t *testing.T) {
	client := New(config.GreenAPI{InstanceID: "123", Token: "secret", BaseURL: "https://example.invalid", Timeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	calls := 0
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := `{"outgoingMessageWebhook":"yes"}`
		if strings.Contains(req.URL.Path, "getChatHistory") {
			if req.Method != http.MethodPost {
				t.Fatal("history must use POST")
			}
			payload, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(payload), `"chatId":"77011234567@c.us"`) {
				t.Fatal("incorrect history account/chat")
			}
			body = `[{"type":"incoming","idMessage":"trigger","timestamp":123456,"chatId":"77011234567@c.us"},{"type":"outgoing","idMessage":"phone","timestamp":123457,"chatId":"77011234567@c.us","statusMessage":"delivered","sendByApi":false}]`
		} else if req.Method != http.MethodGet {
			t.Fatal("settings must use GET")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	history, err := client.ReplyHistory(context.Background(), "77011234567@c.us")
	if err != nil || !history.PhoneRepliesRecorded || len(history.Messages) != 2 || calls != 2 {
		t.Fatalf("history=%+v calls=%d err=%v", history, calls, err)
	}
}

func TestDecodeSendResultRequiresProviderAcknowledgement(t *testing.T) {
	for _, body := range []string{`{}`, `{"idMessage":" "}`, `null`, `broken`} {
		result, err := decodeSendResult("sendMessage", []byte(body))
		if err == nil || result != nil {
			t.Fatalf("unconfirmed result=%+v err=%v", result, err)
		}
	}
	result, err := decodeSendResult("sendMessage", []byte(`{"idMessage":"accepted"}`))
	if err != nil || result.ExternalID != "accepted" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
