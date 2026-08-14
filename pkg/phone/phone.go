// Package phone normalizes WhatsApp identifiers.
//
// Green API addresses individual chats as "<digits>@c.us" and groups as
// "<digits>-<digits>@g.us". The platform stores the bare digit string as the
// canonical phone number and keeps the full chat id alongside it.
package phone

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrEmpty   = errors.New("phone number is empty")
	ErrInvalid = errors.New("phone number is not a valid WhatsApp number")
	ErrGroup   = errors.New("group chats are not supported")
)

const (
	individualSuffix = "@c.us"
	groupSuffix      = "@g.us"
)

// Normalize reduces free-form input to digits only and validates the length.
// Accepts "+7 700 123 45 67", "77001234567", "77001234567@c.us".
func Normalize(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ErrEmpty
	}

	if IsGroupChatID(input) {
		return "", ErrGroup
	}

	if idx := strings.IndexByte(input, '@'); idx >= 0 {
		input = input[:idx]
	}

	var b strings.Builder
	b.Grow(len(input))
	for _, r := range input {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}

	digits := b.String()
	// Shortest international numbers are 7 digits plus country code; 20 is the
	// E.164 ceiling with room to spare.
	if len(digits) < 6 || len(digits) > 20 {
		return "", fmt.Errorf("%w: %q", ErrInvalid, input)
	}
	// A leading zero is a national trunk prefix, never valid in E.164.
	if digits[0] == '0' {
		return "", fmt.Errorf("%w: number must be in international format", ErrInvalid)
	}

	return digits, nil
}

// ChatID builds the Green API individual chat identifier for a phone number.
func ChatID(phone string) (string, error) {
	digits, err := Normalize(phone)
	if err != nil {
		return "", err
	}
	return digits + individualSuffix, nil
}

// FromChatID extracts the canonical phone number from a chat id.
func FromChatID(chatID string) (string, error) {
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return "", ErrEmpty
	}
	if IsGroupChatID(chatID) {
		return "", ErrGroup
	}
	return Normalize(chatID)
}

// IsGroupChatID reports whether the identifier addresses a group.
func IsGroupChatID(id string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(id)), groupSuffix)
}

// Display formats a stored number for the admin UI, e.g. "+7 700 123 45 67"
// for Kazakh and Russian numbers, "+<digits>" otherwise.
func Display(digits string) string {
	if digits == "" {
		return ""
	}
	if len(digits) == 11 && (digits[0] == '7' || digits[0] == '8') {
		return fmt.Sprintf("+%s %s %s %s %s",
			digits[0:1], digits[1:4], digits[4:7], digits[7:9], digits[9:11])
	}
	return "+" + digits
}
