package greenapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/internal/config"
	"github.com/ayran/whatsapp-automation/internal/whatsapp"
)

func TestClassifyHTTPRetriesTemporaryInstanceStates(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		retryable bool
	}{
		{"starting", http.StatusBadRequest, `{"message":"instance in starting process try later"}`, true},
		{"disconnected", http.StatusBadRequest, `{"message":"instance is starting or not authorized"}`, true},
		{"disconnected alternate wording", http.StatusBadRequest, `{"stateInstance":"notAuthorized"}`, true},
		{"client closed request", 499, ``, true},
		{"bad payload", http.StatusBadRequest, `{"message":"Validation failed: chatId is required"}`, false},
		{"expired account", http.StatusBadRequest, `{"message":"Instance account is expired"}`, false},
		{"bad token", http.StatusUnauthorized, `{"message":"Unauthorized"}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyHTTP("sendMessage", tc.status, tc.body)
			if err.Retryable != tc.retryable {
				t.Fatalf("Retryable = %v, want %v (%v)", err.Retryable, tc.retryable, err)
			}
			if whatsapp.IsRetryable(err) != tc.retryable {
				t.Fatalf("IsRetryable = %v, want %v", whatsapp.IsRetryable(err), tc.retryable)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestTransportErrorsNeverExposeToken(t *testing.T) {
	const token = "production-secret-token"
	transportErr := errors.New("dial failed")

	var logs bytes.Buffer
	client := New(config.GreenAPI{
		InstanceID: "12345",
		Token:      token,
		BaseURL:    "https://example.invalid",
		MediaURL:   "https://example.invalid",
		Timeout:    time.Second,
	}, slog.New(slog.NewTextHandler(&logs, nil)))
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.Join(transportErr, errors.New(req.URL.String()))
	})

	_, err := client.doRequest(context.Background(), http.MethodGet,
		client.endpoint(client.baseURL, "receiveNotification"), "", nil, "receiveNotification")
	if err == nil {
		t.Fatal("doRequest returned nil error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("returned error leaked token: %s", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("log leaked token: %s", logs.String())
	}
	if !errors.Is(err, transportErr) {
		t.Fatal("redaction broke error unwrapping")
	}
}

func TestHTTPErrorBodyIsRedacted(t *testing.T) {
	const token = "production-secret-token"
	client := New(config.GreenAPI{
		InstanceID: "12345",
		Token:      token,
		BaseURL:    "https://example.invalid",
		MediaURL:   "https://example.invalid",
		Timeout:    time.Second,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"message":"bad ` + token + `"}`)),
			Header:     make(http.Header),
		}, nil
	})

	_, err := client.doRequest(context.Background(), http.MethodGet,
		client.endpoint(client.baseURL, "getStateInstance"), "", nil, "getStateInstance")
	if err == nil {
		t.Fatal("doRequest returned nil error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("returned error leaked token: %s", err)
	}
	var providerErr *whatsapp.Error
	if !errors.As(err, &providerErr) {
		t.Fatalf("error type = %T, want *whatsapp.Error", err)
	}
	if strings.Contains(providerErr.Body, token) {
		t.Fatalf("stored response body leaked token: %s", providerErr.Body)
	}
}
