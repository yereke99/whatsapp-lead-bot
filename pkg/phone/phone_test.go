package phone

import (
	"errors"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"77011234567":        "77011234567",
		"+7 701 123 45 67":   "77011234567",
		"+7 (701) 123-45-67": "77011234567",
		"77011234567@c.us":   "77011234567",
		"  77011234567  ":    "77011234567",
		"+77011234567":       "77011234567",
	}

	for input, want := range cases {
		got, err := Normalize(input)
		if err != nil {
			t.Errorf("Normalize(%q) returned error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNormalizeRejectsInvalid(t *testing.T) {
	cases := map[string]error{
		"":                      ErrEmpty,
		"   ":                   ErrEmpty,
		"12345":                 ErrInvalid,
		"07011234567":           ErrInvalid, // national trunk prefix
		"abcdefgh":              ErrInvalid,
		"123456789012345678901": ErrInvalid,
	}

	for input, wantErr := range cases {
		_, err := Normalize(input)
		if err == nil {
			t.Errorf("Normalize(%q) should have failed", input)
			continue
		}
		if !errors.Is(err, wantErr) {
			t.Errorf("Normalize(%q) error = %v, want %v", input, err, wantErr)
		}
	}
}

func TestGroupChatsAreRejected(t *testing.T) {
	if !IsGroupChatID("77011234567-1234567890@g.us") {
		t.Error("group chat id was not recognised")
	}
	if IsGroupChatID("77011234567@c.us") {
		t.Error("individual chat misidentified as a group")
	}

	if _, err := Normalize("77011234567-1590000000@g.us"); !errors.Is(err, ErrGroup) {
		t.Errorf("group chat should return ErrGroup, got %v", err)
	}
	if _, err := FromChatID("77011234567-1590000000@g.us"); !errors.Is(err, ErrGroup) {
		t.Errorf("FromChatID should reject groups, got %v", err)
	}
}

func TestChatID(t *testing.T) {
	got, err := ChatID("+7 701 123 45 67")
	if err != nil {
		t.Fatalf("ChatID: %v", err)
	}
	if got != "77011234567@c.us" {
		t.Errorf("ChatID = %q, want %q", got, "77011234567@c.us")
	}
}

func TestFromChatID(t *testing.T) {
	got, err := FromChatID("77011234567@c.us")
	if err != nil {
		t.Fatalf("FromChatID: %v", err)
	}
	if got != "77011234567" {
		t.Errorf("FromChatID = %q, want %q", got, "77011234567")
	}
}

func TestRoundTrip(t *testing.T) {
	const original = "77011234567"

	chatID, err := ChatID(original)
	if err != nil {
		t.Fatal(err)
	}
	back, err := FromChatID(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if back != original {
		t.Errorf("round trip produced %q, want %q", back, original)
	}
}

func TestDisplay(t *testing.T) {
	cases := map[string]string{
		"77011234567":  "+7 701 123 45 67",
		"":             "",
		"441234567890": "+441234567890",
	}

	for input, want := range cases {
		if got := Display(input); got != want {
			t.Errorf("Display(%q) = %q, want %q", input, got, want)
		}
	}
}
