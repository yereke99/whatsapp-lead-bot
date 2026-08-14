// Package render substitutes {{variable}} placeholders in message bodies.
//
// Rendering never fails: an unknown or empty variable collapses to its
// configured fallback rather than leaking "{{first_name}}" into a customer's
// WhatsApp chat.
package render

import (
	"sort"
	"strings"
	"unicode"
)

// Variable documents one placeholder for the template editor.
type Variable struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

// Catalog is the full set of placeholders the engine understands. The admin
// UI renders this list beside the template editor.
var Catalog = []Variable{
	{"contact_name", "Клиент аты", "Байланыстың толық аты, белгісіз болса — әмбебап сәлемдесу", "Әлішер Сәрсенов"},
	{"first_name", "Аты", "Байланыстың бірінші аты", "Әлішер"},
	{"phone", "Телефон", "Байланыс нөмірі", "+7 700 123 45 67"},
	{"campaign_name", "Кампания", "Кампания атауы", "Түрік айраны вебинары"},
	{"webinar_date", "Сабақ күні", "Іс-шара күні кампания уақыт белдеуінде", "15.08.2026"},
	{"webinar_time", "Сабақ уақыты", "Іс-шара басталу уақыты", "21:00"},
	{"webinar_datetime", "Күні және уақыты", "Күні мен уақыты бірге", "15.08.2026 21:00"},
	{"webinar_link", "Сілтеме", "Сабаққа қосылу сілтемесі", "https://example.com/live"},
	{"remaining_time", "Қалған уақыт", "Сабақтың басталуына қалған уақыт", "2 сағат 30 минут"},
	{"timezone", "Уақыт белдеуі", "Кампания уақыт белдеуі", "Asia/Almaty"},
}

// CatalogKeys returns the recognised variable names.
func CatalogKeys() []string {
	keys := make([]string, 0, len(Catalog))
	for _, v := range Catalog {
		keys = append(keys, v.Key)
	}
	sort.Strings(keys)
	return keys
}

// Context carries the values available to a single render.
type Context struct {
	Values map[string]string
	// Fallback replaces variables with no value. Empty by default, which
	// simply removes the placeholder.
	Fallback map[string]string
}

// NewContext builds a render context from key/value pairs.
func NewContext(values map[string]string) Context {
	if values == nil {
		values = map[string]string{}
	}
	return Context{
		Values: values,
		Fallback: map[string]string{
			"contact_name": "құрметті клиент",
			"first_name":   "құрметті клиент",
		},
	}
}

// Render replaces every {{key}} occurrence in body.
//
// Placeholders tolerate surrounding whitespace ("{{ first_name }}") and are
// matched case-insensitively. Unknown placeholders are removed, and any
// whitespace left dangling by the removal is tidied so the message never shows
// a double space or a stray blank line.
func Render(body string, ctx Context) string {
	if body == "" || !strings.Contains(body, "{{") {
		return body
	}

	var out strings.Builder
	out.Grow(len(body))

	rest := body
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			out.WriteString(rest)
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			// Unterminated placeholder: emit the remainder verbatim.
			out.WriteString(rest)
			break
		}
		end += start

		out.WriteString(rest[:start])
		key := normalizeKey(rest[start+2 : end])
		out.WriteString(ctx.lookup(key))
		rest = rest[end+2:]
	}

	return tidy(out.String())
}

func (c Context) lookup(key string) string {
	if key == "" {
		return ""
	}
	if v, ok := c.Values[key]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	if v, ok := c.Fallback[key]; ok {
		return v
	}
	return ""
}

func normalizeKey(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// tidy removes artefacts created by substituting an empty value: repeated
// spaces on a line and runs of more than two blank lines.
func tidy(s string) string {
	lines := strings.Split(s, "\n")
	cleaned := make([]string, 0, len(lines))

	blankRun := 0
	for _, line := range lines {
		line = collapseSpaces(line)
		if strings.TrimSpace(line) == "" {
			blankRun++
			// Preserve paragraph breaks, drop anything beyond one blank line.
			if blankRun > 1 {
				continue
			}
			cleaned = append(cleaned, "")
			continue
		}
		blankRun = 0
		cleaned = append(cleaned, line)
	}

	return strings.TrimRight(strings.Join(cleaned, "\n"), " \n\t")
}

func collapseSpaces(line string) string {
	var b strings.Builder
	b.Grow(len(line))

	prevSpace := false
	for _, r := range line {
		isSpace := r == ' ' || r == '\t'
		if isSpace {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteRune(' ')
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimRightFunc(b.String(), unicode.IsSpace)
}

// UnknownVariables lists placeholders in body that the catalog does not
// define. The template editor surfaces these as a warning before saving.
func UnknownVariables(body string) []string {
	known := map[string]bool{}
	for _, v := range Catalog {
		known[v.Key] = true
	}

	seen := map[string]bool{}
	var unknown []string

	rest := body
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			break
		}
		end := strings.Index(rest[start:], "}}")
		if end < 0 {
			break
		}
		end += start

		key := normalizeKey(rest[start+2 : end])
		if key != "" && !known[key] && !seen[key] {
			seen[key] = true
			unknown = append(unknown, key)
		}
		rest = rest[end+2:]
	}

	sort.Strings(unknown)
	return unknown
}
