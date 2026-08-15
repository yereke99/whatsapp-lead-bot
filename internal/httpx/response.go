// Package httpx holds the shared HTTP plumbing: consistent JSON envelopes,
// request parsing and client-address resolution.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Envelope is the shape of every JSON response the API returns.
type Envelope struct {
	Data  any        `json:"data,omitempty"`
	Meta  *Meta      `json:"meta,omitempty"`
	Error *ErrorBody `json:"error,omitempty"`
}

// Meta carries pagination details.
type Meta struct {
	Total   int  `json:"total"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"has_more"`
}

// ErrorBody is the machine-readable error payload.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// Error codes are stable strings the frontend can branch on.
const (
	CodeBadRequest    = "bad_request"
	CodeValidation    = "validation_failed"
	CodeUnauthorized  = "unauthorized"
	CodeForbidden     = "forbidden"
	CodeNotFound      = "not_found"
	CodeConflict      = "conflict"
	CodeTooLarge      = "payload_too_large"
	CodeRateLimited   = "rate_limited"
	CodeInternal      = "internal_error"
	CodeUnavailable   = "service_unavailable"
	CodeCSRF          = "csrf_failed"
	CodeNotConfigured = "not_configured"
)

// JSON writes a success response.
func JSON(w http.ResponseWriter, status int, data any) {
	write(w, status, Envelope{Data: emptyNotNull(data)})
}

// emptyNotNull turns a nil slice or map into an empty one.
//
// Go encodes a nil slice as JSON null, so an endpoint with nothing to return
// answers "null" rather than "[]". That pushes a guard onto every consumer of
// every list endpoint, and a single missing one is a page that breaks the
// moment the table happens to be empty. Returning the empty collection makes
// "no rows" the same shape as "some rows".
//
// Pointers are left alone: a nil pointer means absent, and null is the right
// encoding for that.
func emptyNotNull(data any) any {
	if data == nil {
		return nil
	}

	value := reflect.ValueOf(data)
	switch value.Kind() {
	case reflect.Slice:
		if value.IsNil() {
			return reflect.MakeSlice(value.Type(), 0, 0).Interface()
		}
	case reflect.Map:
		if value.IsNil() {
			return reflect.MakeMap(value.Type()).Interface()
		}
	}
	return data
}

// Paged writes a success response with pagination metadata.
func Paged(w http.ResponseWriter, data any, total, limit, offset int) {
	write(w, http.StatusOK, Envelope{
		Data: emptyNotNull(data),
		Meta: &Meta{
			Total:   total,
			Limit:   limit,
			Offset:  offset,
			HasMore: offset+limit < total,
		},
	})
}

// NoContent acknowledges a request with no body.
func NoContent(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) }

// Fail writes an error response.
func Fail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, Envelope{Error: &ErrorBody{Code: code, Message: message}})
}

// FailWithDetails writes an error response carrying structured details, used
// for field-level validation feedback.
func FailWithDetails(w http.ResponseWriter, status int, code, message string, details any) {
	write(w, status, Envelope{Error: &ErrorBody{Code: code, Message: message, Details: details}})
}

// Internal logs the underlying cause and returns a generic message.
//
// The client never sees the internal error text: it can contain query
// fragments, file paths or provider responses.
func Internal(w http.ResponseWriter, log *slog.Logger, err error, context string) {
	if log != nil {
		log.Error("request failed",
			slog.String("context", context),
			slog.String("error", err.Error()))
	}
	Fail(w, http.StatusInternalServerError, CodeInternal, "Ішкі қате орын алды. Кейінірек қайталап көріңіз.")
}

func write(w http.ResponseWriter, status int, body Envelope) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)

	if status == http.StatusNoContent {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line is already on the wire; nothing useful left to do.
		slog.Default().Debug("encoding response failed", slog.String("error", err.Error()))
	}
}

// DecodeJSON reads and validates a JSON request body.
//
// The body is size-limited and unknown fields are rejected, so a typo in a
// field name surfaces as an error instead of being silently ignored.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	const maxBody = 2 << 20

	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(strings.ToLower(ct), "application/json") {
		return fmt.Errorf("Content-Type application/json болуы керек")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		var syntaxErr *json.SyntaxError
		var typeErr *json.UnmarshalTypeError
		var maxErr *http.MaxBytesError

		switch {
		case errors.As(err, &syntaxErr):
			return fmt.Errorf("JSON синтаксис қатесі (позиция %d)", syntaxErr.Offset)
		case errors.As(err, &typeErr):
			return fmt.Errorf("«%s» өрісінің түрі дұрыс емес", typeErr.Field)
		case errors.As(err, &maxErr):
			return fmt.Errorf("сұраныс денесі тым үлкен")
		case errors.Is(err, io.EOF):
			return fmt.Errorf("сұраныс денесі бос")
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			field := strings.TrimPrefix(err.Error(), "json: unknown field ")
			return fmt.Errorf("белгісіз өріс: %s", field)
		default:
			return fmt.Errorf("сұранысты оқу мүмкін болмады")
		}
	}

	// A second value means the client sent more than one JSON document.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("сұраныс денесінде бір ғана JSON нысаны болуы керек")
	}
	return nil
}

// ---------------------------------------------------------- query parsing --

// QueryInt reads an integer query parameter with a default and bounds.
func QueryInt(r *http.Request, key string, def, min, max int) int {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// QueryBool reads a boolean query parameter.
func QueryBool(r *http.Request, key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(r.URL.Query().Get(key)))
	return err == nil && v
}

// QueryBoolPtr distinguishes "absent" from "false".
func QueryBoolPtr(r *http.Request, key string) *bool {
	raw := strings.TrimSpace(r.URL.Query().Get(key))
	if raw == "" {
		return nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return nil
	}
	return &v
}

// QueryString reads a trimmed string parameter.
func QueryString(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

// QueryUUID parses an optional UUID parameter.
func QueryUUID(r *http.Request, key string) *uuid.UUID {
	raw := QueryString(r, key)
	if raw == "" {
		return nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return &id
}

// QueryDate parses a YYYY-MM-DD parameter as midnight in tz.
func QueryDate(r *http.Request, key, tz string) *time.Time {
	raw := QueryString(r, key)
	if raw == "" {
		return nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		loc = time.UTC
	}
	t, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}

// PathUUID reads a UUID from a routing wildcard.
func PathUUID(r *http.Request, key string) (uuid.UUID, error) {
	raw := strings.TrimSpace(r.PathValue(key))
	if raw == "" {
		return uuid.Nil, fmt.Errorf("«%s» параметрі жоқ", key)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("«%s» параметрі жарамсыз", key)
	}
	return id, nil
}

// ------------------------------------------------------------- client IP --

// ClientIP resolves the caller's address.
//
// Proxy headers are only honoured when the immediate peer is a configured
// trusted proxy; otherwise any client could spoof its own address and defeat
// per-IP rate limiting.
func ClientIP(r *http.Request, trustedProxies []string) string {
	remote := stripPort(r.RemoteAddr)

	if len(trustedProxies) == 0 || !isTrusted(remote, trustedProxies) {
		return remote
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		// The left-most entry is the original client.
		parts := strings.Split(forwarded, ",")
		if candidate := strings.TrimSpace(parts[0]); candidate != "" {
			if net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		if net.ParseIP(realIP) != nil {
			return realIP
		}
	}
	return remote
}

func isTrusted(ip string, trusted []string) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}

	for _, entry := range trusted {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(parsed) {
				return true
			}
			continue
		}
		if entry == ip {
			return true
		}
	}
	return false
}

func stripPort(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
