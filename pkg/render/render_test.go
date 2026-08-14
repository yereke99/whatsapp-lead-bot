package render

import (
	"strings"
	"testing"
)

func TestRenderSubstitutesVariables(t *testing.T) {
	ctx := NewContext(map[string]string{
		"first_name":   "Әлішер",
		"webinar_time": "21:00",
		"webinar_link": "https://example.com/live",
	})

	got := Render("Сәлеметсіз бе, {{first_name}}! Сабақ {{webinar_time}}-де: {{webinar_link}}", ctx)
	want := "Сәлеметсіз бе, Әлішер! Сабақ 21:00-де: https://example.com/live"

	if got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestRenderIsWhitespaceAndCaseTolerant(t *testing.T) {
	ctx := NewContext(map[string]string{"first_name": "Айгүл"})

	for _, template := range []string{
		"{{first_name}}",
		"{{ first_name }}",
		"{{FIRST_NAME}}",
		"{{  First_Name  }}",
	} {
		if got := Render(template, ctx); got != "Айгүл" {
			t.Errorf("Render(%q) = %q, want %q", template, got, "Айгүл")
		}
	}
}

// TestRenderUsesFallbackForMissingName is the guard against a customer
// receiving a literal "{{first_name}}" or an awkward empty greeting.
func TestRenderUsesFallbackForMissingName(t *testing.T) {
	ctx := NewContext(map[string]string{})

	got := Render("Сәлеметсіз бе, {{first_name}}!", ctx)
	if strings.Contains(got, "{{") {
		t.Errorf("placeholder leaked into the output: %q", got)
	}
	if got != "Сәлеметсіз бе, құрметті клиент!" {
		t.Errorf("Render() = %q, want the configured fallback", got)
	}
}

func TestRenderTreatsBlankValueAsMissing(t *testing.T) {
	ctx := NewContext(map[string]string{"first_name": "   "})

	if got := Render("{{first_name}}", ctx); got != "құрметті клиент" {
		t.Errorf("blank value should use the fallback, got %q", got)
	}
}

func TestRenderRemovesUnknownVariables(t *testing.T) {
	ctx := NewContext(map[string]string{})

	got := Render("Бастау {{unknown_thing}} соңы", ctx)
	if strings.Contains(got, "{{") || strings.Contains(got, "unknown_thing") {
		t.Errorf("unknown placeholder survived: %q", got)
	}
	if got != "Бастау соңы" {
		t.Errorf("Render() = %q, want the double space collapsed", got)
	}
}

func TestRenderTidiesBlankLines(t *testing.T) {
	ctx := NewContext(map[string]string{})

	got := Render("Бірінші\n\n{{webinar_link}}\n\nСоңғы", ctx)
	if strings.Contains(got, "\n\n\n") {
		t.Errorf("more than one blank line survived: %q", got)
	}
}

func TestRenderLeavesUnterminatedPlaceholderAlone(t *testing.T) {
	ctx := NewContext(map[string]string{"first_name": "Аян"})

	input := "Сәлем {{first_name"
	if got := Render(input, ctx); got != input {
		t.Errorf("Render(%q) = %q, want it unchanged", input, got)
	}
}

func TestRenderEmptyBody(t *testing.T) {
	if got := Render("", NewContext(nil)); got != "" {
		t.Errorf("Render(\"\") = %q, want empty", got)
	}
}

func TestRenderWithoutPlaceholdersIsUnchanged(t *testing.T) {
	const body = "Қарапайым мәтін, айнымалысыз."
	if got := Render(body, NewContext(nil)); got != body {
		t.Errorf("Render() = %q, want %q", got, body)
	}
}

func TestRenderRepeatedPlaceholder(t *testing.T) {
	ctx := NewContext(map[string]string{"webinar_time": "21:00"})

	got := Render("{{webinar_time}} және тағы {{webinar_time}}", ctx)
	if got != "21:00 және тағы 21:00" {
		t.Errorf("Render() = %q", got)
	}
}

func TestUnknownVariables(t *testing.T) {
	body := "{{first_name}} {{bogus_one}} {{webinar_link}} {{bogus_two}} {{bogus_one}}"

	got := UnknownVariables(body)
	want := []string{"bogus_one", "bogus_two"}

	if len(got) != len(want) {
		t.Fatalf("UnknownVariables() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("UnknownVariables()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestUnknownVariablesEmptyForValidBody(t *testing.T) {
	body := "{{first_name}}, {{campaign_name}}, {{webinar_link}}"
	if got := UnknownVariables(body); len(got) != 0 {
		t.Errorf("UnknownVariables() = %v, want none", got)
	}
}

func TestCatalogKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, variable := range Catalog {
		if seen[variable.Key] {
			t.Errorf("duplicate catalog key %q", variable.Key)
		}
		seen[variable.Key] = true

		if variable.Label == "" || variable.Description == "" {
			t.Errorf("catalog entry %q is missing its label or description", variable.Key)
		}
	}
}
