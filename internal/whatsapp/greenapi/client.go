// Package greenapi implements whatsapp.Provider against the Green API
// WhatsApp gateway.
//
// Endpoint shapes follow the Green API HTTP API:
//
//	POST {base}/waInstance{id}/sendMessage/{token}
//	POST {media}/waInstance{id}/sendFileByUpload/{token}   (multipart)
//	POST {base}/waInstance{id}/sendFileByUrl/{token}
//	POST {base}/waInstance{id}/getContactInfo/{token}
//	POST {base}/waInstance{id}/getAvatar/{token}
//	POST {base}/waInstance{id}/checkWhatsapp/{token}
//	GET  {base}/waInstance{id}/getStateInstance/{token}
package greenapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

const providerName = "greenapi"

// Client talks to a single Green API instance.
type Client struct {
	instanceID string
	token      string
	baseURL    string
	mediaURL   string
	http       *http.Client
	log        *slog.Logger
	limiter    *rateLimiter
	configured bool
}

// New builds a client from configuration. It never returns an error: when
// credentials are missing the client is constructed in an unconfigured state
// and every send fails with whatsapp.ErrNotConfigured, which keeps the admin
// panel usable on a fresh install.
func New(cfg config.GreenAPI, log *slog.Logger) *Client {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
	}

	return &Client{
		instanceID: cfg.InstanceID,
		token:      cfg.Token,
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		mediaURL:   strings.TrimRight(cfg.MediaURL, "/"),
		http:       &http.Client{Timeout: cfg.Timeout, Transport: transport},
		log:        log.With(slog.String("component", "greenapi")),
		limiter:    newRateLimiter(cfg.RateInterval),
		configured: cfg.InstanceID != "" && cfg.Token != "",
	}
}

func (c *Client) Name() string { return providerName }

func (c *Client) Configured() bool { return c.configured }

// endpoint builds an API url. The token appears in the path, so it must never
// be written to logs; logRequest below only records the method name.
func (c *Client) endpoint(host, method string) string {
	return fmt.Sprintf("%s/waInstance%s/%s/%s", host, c.instanceID, method, c.token)
}

// ---------------------------------------------------------------- sending --

func (c *Client) SendText(ctx context.Context, msg whatsapp.TextMessage) (*whatsapp.SendResult, error) {
	if err := c.ready("sendMessage"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(msg.Text) == "" {
		return nil, &whatsapp.Error{Provider: providerName, Op: "sendMessage", Message: "message text is empty"}
	}

	payload := map[string]any{
		"chatId":      msg.ChatID,
		"message":     msg.Text,
		"linkPreview": msg.LinkPreview,
	}
	if msg.QuotedID != "" {
		payload["quotedMessageId"] = msg.QuotedID
	}

	return c.postJSONForID(ctx, c.baseURL, "sendMessage", payload)
}

func (c *Client) SendImage(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	msg.Kind = whatsapp.MediaImage
	return c.sendFile(ctx, msg)
}

func (c *Client) SendVideo(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	msg.Kind = whatsapp.MediaVideo
	return c.sendFile(ctx, msg)
}

func (c *Client) SendAudio(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	msg.Kind = whatsapp.MediaAudio
	return c.sendFile(ctx, msg)
}

func (c *Client) SendVoice(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	msg.Kind = whatsapp.MediaVoice
	// Green API renders an attachment as a push-to-talk voice note only when
	// the uploaded file is OGG/Opus. Anything else arrives as a music file,
	// which is a different product experience, so refuse rather than degrade.
	if !strings.EqualFold(filepath.Ext(msg.FileName), ".ogg") {
		return nil, &whatsapp.Error{
			Provider: providerName,
			Op:       "sendFileByUpload",
			Message:  fmt.Sprintf("voice messages must be OGG/Opus, got %q", filepath.Ext(msg.FileName)),
		}
	}
	return c.sendFile(ctx, msg)
}

func (c *Client) SendDocument(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	msg.Kind = whatsapp.MediaDocument
	return c.sendFile(ctx, msg)
}

// SendMediaWithCaption dispatches on the media kind. The caption travels with
// the file so the recipient receives one WhatsApp message, not two.
func (c *Client) SendMediaWithCaption(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	switch msg.Kind {
	case whatsapp.MediaImage:
		return c.SendImage(ctx, msg)
	case whatsapp.MediaVideo:
		return c.SendVideo(ctx, msg)
	case whatsapp.MediaAudio:
		return c.SendAudio(ctx, msg)
	case whatsapp.MediaVoice:
		return c.SendVoice(ctx, msg)
	case whatsapp.MediaDocument:
		return c.SendDocument(ctx, msg)
	default:
		return nil, &whatsapp.Error{
			Provider: providerName,
			Op:       "sendFileByUpload",
			Message:  fmt.Sprintf("unsupported media kind %q", msg.Kind),
		}
	}
}

func (c *Client) sendFile(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	if err := c.ready("sendFile"); err != nil {
		return nil, err
	}
	if err := msg.Validate(); err != nil {
		return nil, &whatsapp.Error{Provider: providerName, Op: "sendFile", Message: err.Error()}
	}

	if msg.URL != "" {
		payload := map[string]any{
			"chatId":   msg.ChatID,
			"urlFile":  msg.URL,
			"fileName": msg.FileName,
		}
		if msg.Caption != "" {
			payload["caption"] = msg.Caption
		}
		if msg.QuotedID != "" {
			payload["quotedMessageId"] = msg.QuotedID
		}
		return c.postJSONForID(ctx, c.baseURL, "sendFileByUrl", payload)
	}

	return c.uploadFile(ctx, msg)
}

func (c *Client) uploadFile(ctx context.Context, msg whatsapp.MediaMessage) (*whatsapp.SendResult, error) {
	file, err := os.Open(msg.FilePath)
	if err != nil {
		// A missing file on disk is a deployment fault, not a transient one.
		return nil, &whatsapp.Error{
			Provider: providerName,
			Op:       "sendFileByUpload",
			Message:  "cannot open media file",
			Err:      err,
		}
	}
	defer file.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	if err := writer.WriteField("chatId", msg.ChatID); err != nil {
		return nil, wrapLocal("sendFileByUpload", err)
	}
	if msg.Caption != "" {
		if err := writer.WriteField("caption", msg.Caption); err != nil {
			return nil, wrapLocal("sendFileByUpload", err)
		}
	}
	if msg.QuotedID != "" {
		if err := writer.WriteField("quotedMessageId", msg.QuotedID); err != nil {
			return nil, wrapLocal("sendFileByUpload", err)
		}
	}
	if err := writer.WriteField("fileName", msg.FileName); err != nil {
		return nil, wrapLocal("sendFileByUpload", err)
	}

	// The part's Content-Type decides how WhatsApp renders the attachment, so
	// it is set explicitly instead of relying on the default octet-stream.
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeFilename(msg.FileName)))
	header.Set("Content-Type", contentTypeFor(msg))

	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, wrapLocal("sendFileByUpload", err)
	}
	if _, err := io.Copy(part, file); err != nil {
		return nil, wrapLocal("sendFileByUpload", err)
	}
	if err := writer.Close(); err != nil {
		return nil, wrapLocal("sendFileByUpload", err)
	}

	raw, err := c.do(ctx, http.MethodPost,
		c.endpoint(c.mediaURL, "sendFileByUpload"),
		writer.FormDataContentType(), body.Bytes(), "sendFileByUpload")
	if err != nil {
		return nil, err
	}
	return decodeSendResult("sendFileByUpload", raw)
}

// ------------------------------------------------------------- account ops --

type contactInfoResponse struct {
	Avatar      string `json:"avatar"`
	Name        string `json:"name"`
	ContactName string `json:"contactName"`
	ChatID      string `json:"chatId"`
	IsBusiness  bool   `json:"isBusiness"`
}

// ContactInfo is the subset of Green API's getContactInfo the platform uses to
// populate the chat list.
type ContactInfo struct {
	Name      string
	AvatarURL string
}

func (c *Client) GetContactInfo(ctx context.Context, chatID string) (*ContactInfo, error) {
	if err := c.ready("getContactInfo"); err != nil {
		return nil, err
	}

	raw, err := c.do(ctx, http.MethodPost,
		c.endpoint(c.baseURL, "getContactInfo"), "application/json",
		mustJSON(map[string]any{"chatId": chatID}), "getContactInfo")
	if err != nil {
		return nil, err
	}

	var resp contactInfoResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, wrapLocal("getContactInfo", err)
	}

	name := strings.TrimSpace(resp.ContactName)
	if name == "" {
		name = strings.TrimSpace(resp.Name)
	}
	return &ContactInfo{Name: name, AvatarURL: strings.TrimSpace(resp.Avatar)}, nil
}

type avatarResponse struct {
	URLAvatar string `json:"urlAvatar"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// GetAvatar returns the contact's profile picture url, or an empty string when
// the contact has none or hides it.
func (c *Client) GetAvatar(ctx context.Context, chatID string) (string, error) {
	if err := c.ready("getAvatar"); err != nil {
		return "", err
	}

	raw, err := c.do(ctx, http.MethodPost,
		c.endpoint(c.baseURL, "getAvatar"), "application/json",
		mustJSON(map[string]any{"chatId": chatID}), "getAvatar")
	if err != nil {
		return "", err
	}

	var resp avatarResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", wrapLocal("getAvatar", err)
	}
	if !resp.Available {
		return "", nil
	}
	return strings.TrimSpace(resp.URLAvatar), nil
}

// Download fetches a provider-hosted file, such as the downloadUrl carried by
// an incoming media webhook. limit caps how many bytes are read.
func (c *Client) Download(ctx context.Context, url string, limit int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", wrapLocal("download", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", &whatsapp.Error{
			Provider: providerName, Op: "download", Retryable: true, Err: err,
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", &whatsapp.Error{
			Provider:   providerName,
			Op:         "download",
			StatusCode: resp.StatusCode,
			Message:    "unexpected status while downloading media",
			Retryable:  whatsapp.RetryableStatus(resp.StatusCode),
		}
	}

	// LimitReader guards against a hostile or misreported Content-Length.
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", &whatsapp.Error{Provider: providerName, Op: "download", Retryable: true, Err: err}
	}
	if int64(len(data)) > limit {
		return nil, "", &whatsapp.Error{
			Provider: providerName, Op: "download",
			Message: fmt.Sprintf("file exceeds the %d byte limit", limit),
		}
	}

	return data, resp.Header.Get("Content-Type"), nil
}

type checkWhatsappResponse struct {
	ExistsWhatsapp bool `json:"existsWhatsapp"`
}

func (c *Client) CheckNumber(ctx context.Context, phone string) (bool, error) {
	if err := c.ready("checkWhatsapp"); err != nil {
		return false, err
	}

	number, err := strconv.ParseInt(strings.TrimPrefix(phone, "+"), 10, 64)
	if err != nil {
		return false, &whatsapp.Error{
			Provider: providerName, Op: "checkWhatsapp",
			Message: fmt.Sprintf("invalid phone number %q", phone),
		}
	}

	raw, err := c.do(ctx, http.MethodPost,
		c.endpoint(c.baseURL, "checkWhatsapp"), "application/json",
		mustJSON(map[string]any{"phoneNumber": number}), "checkWhatsapp")
	if err != nil {
		return false, err
	}

	var resp checkWhatsappResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return false, wrapLocal("checkWhatsapp", err)
	}
	return resp.ExistsWhatsapp, nil
}

func (c *Client) State(ctx context.Context) (*whatsapp.InstanceState, error) {
	if err := c.ready("getStateInstance"); err != nil {
		return nil, err
	}

	raw, err := c.do(ctx, http.MethodGet,
		c.endpoint(c.baseURL, "getStateInstance"), "", nil, "getStateInstance")
	if err != nil {
		return nil, err
	}

	var resp whatsapp.InstanceState
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, wrapLocal("getStateInstance", err)
	}
	resp.Authorized = strings.EqualFold(resp.State, "authorized")
	resp.RawStateJSON = string(raw)
	return &resp, nil
}

// ------------------------------------------------------------- transport --

func (c *Client) ready(op string) error {
	if !c.configured {
		return &whatsapp.Error{Provider: providerName, Op: op, Message: "instance id or token is not set", Err: whatsapp.ErrNotConfigured}
	}
	return nil
}

func (c *Client) postJSONForID(ctx context.Context, host, method string, payload map[string]any) (*whatsapp.SendResult, error) {
	raw, err := c.do(ctx, http.MethodPost, c.endpoint(host, method), "application/json", mustJSON(payload), method)
	if err != nil {
		return nil, err
	}
	return decodeSendResult(method, raw)
}

func decodeSendResult(op string, raw []byte) (*whatsapp.SendResult, error) {
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, wrapLocal(op, fmt.Errorf("decode response: %w", err))
	}

	id, _ := parsed["idMessage"].(string)
	if strings.TrimSpace(id) == "" {
		// Without a provider id the message cannot be correlated to its later
		// status webhooks, so treat the ack as a failure worth retrying.
		return nil, &whatsapp.Error{
			Provider:  providerName,
			Op:        op,
			Message:   "provider response did not contain idMessage",
			Body:      truncate(string(raw), 500),
			Retryable: true,
		}
	}

	return &whatsapp.SendResult{ExternalID: id, Raw: parsed}, nil
}

func (c *Client) do(ctx context.Context, method, url, contentType string, body []byte, op string) ([]byte, error) {
	// Serialise outbound calls: Green API rejects bursts, and a 429 storm is
	// far more expensive than a few hundred milliseconds of pacing.
	if err := c.limiter.wait(ctx); err != nil {
		return nil, &whatsapp.Error{Provider: providerName, Op: op, Retryable: true, Err: err}
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, wrapLocal(op, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")

	started := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		retryable := true
		if errors.Is(err, context.Canceled) {
			retryable = false
		}
		c.log.Warn("provider request failed",
			slog.String("op", op),
			slog.Duration("took", time.Since(started)),
			slog.String("error", err.Error()))
		return nil, &whatsapp.Error{Provider: providerName, Op: op, Retryable: retryable, Err: err}
	}
	defer resp.Body.Close()

	// 5 MiB is far beyond any legitimate Green API response.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, &whatsapp.Error{Provider: providerName, Op: op, StatusCode: resp.StatusCode, Retryable: true, Err: err}
	}

	c.log.Debug("provider request",
		slog.String("op", op),
		slog.Int("status", resp.StatusCode),
		slog.Duration("took", time.Since(started)))

	if resp.StatusCode != http.StatusOK {
		return nil, classifyHTTP(op, resp.StatusCode, raw)
	}
	return raw, nil
}

func classifyHTTP(op string, status int, body []byte) *whatsapp.Error {
	err := &whatsapp.Error{
		Provider:   providerName,
		Op:         op,
		StatusCode: status,
		Body:       truncate(string(body), 1000),
		Retryable:  whatsapp.RetryableStatus(status),
	}

	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		err.Message = "instance credentials rejected by provider"
	case http.StatusBadRequest:
		err.Message = "provider rejected the request payload"
	case 466:
		// Green API's quota/subscription signal. Retrying cannot help until a
		// human tops the account up, but the message should not be discarded
		// either, so it is left permanent and surfaced in the admin panel.
		err.Message = "instance quota exhausted or subscription inactive"
	case http.StatusTooManyRequests:
		err.Message = "provider rate limit reached"
	default:
		if status >= 500 {
			err.Message = "provider is temporarily unavailable"
		} else {
			err.Message = "unexpected provider response"
		}
	}

	// Green API answers with a JSON body on most failures; surface it.
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) == nil {
		for _, key := range []string{"message", "error", "reason", "description"} {
			if v, ok := parsed[key].(string); ok && v != "" {
				err.Message = err.Message + ": " + v
				break
			}
		}
	}

	return err
}

func wrapLocal(op string, err error) *whatsapp.Error {
	return &whatsapp.Error{Provider: providerName, Op: op, Err: err}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// The payloads here are plain maps of strings and numbers; a failure
		// would mean programmer error, not runtime input.
		panic(fmt.Sprintf("greenapi: marshal payload: %v", err))
	}
	return b
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func sanitizeFilename(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, `"`, "")
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "file"
	}
	return name
}

func contentTypeFor(msg whatsapp.MediaMessage) string {
	if msg.MimeType != "" {
		return msg.MimeType
	}
	switch msg.Kind {
	case whatsapp.MediaImage:
		return "image/jpeg"
	case whatsapp.MediaVideo:
		return "video/mp4"
	case whatsapp.MediaVoice:
		return "audio/ogg; codecs=opus"
	case whatsapp.MediaAudio:
		return "audio/mpeg"
	default:
		return "application/octet-stream"
	}
}

// rateLimiter paces outbound calls to at most one per interval.
type rateLimiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	if interval < 0 {
		interval = 0
	}
	return &rateLimiter{interval: interval}
}

func (r *rateLimiter) wait(ctx context.Context) error {
	if r.interval == 0 {
		return nil
	}

	r.mu.Lock()
	now := time.Now()
	if r.next.Before(now) {
		r.next = now
	}
	slot := r.next
	r.next = slot.Add(r.interval)
	r.mu.Unlock()

	delay := time.Until(slot)
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Compile-time assertion that the client satisfies the provider contract.
var _ whatsapp.Provider = (*Client)(nil)
