package textnorm

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"trims and lowercases", "  Айран  ", "айран"},
		{"collapses internal whitespace", "Айран\t\tҚаймақ", "айран қаймақ"},
		{"newlines become spaces", "Айран\nҚаймақ", "айран қаймақ"},
		{"case folds cyrillic", "ТОҚТАТУ", "тоқтату"},
		{"keeps punctuation", "Айран/Қаймақ", "айран/қаймақ"},
		{"strips zero width space", "Ай​ран", "айран"},
		{"strips bidi marks", "‪Айран‬", "айран"},
		{"folds fullwidth slash", "Айран／Қаймақ", "айран/қаймақ"},
		{"folds typographic quotes", "«Айран»", `"айран"`},
		{"folds en dash", "Айран–Қаймақ", "айран-қаймақ"},
		{"nbsp is whitespace", "Айран Қаймақ", "айран қаймақ"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.input); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestNormalizeComposesCombiningMarks covers the case that motivates NFKC:
// the same letter can arrive pre-composed or as a base letter followed by a
// combining mark, depending on the sender's keyboard.
func TestNormalizeComposesCombiningMarks(t *testing.T) {
	precomposed := "\u04D2"      // CYRILLIC CAPITAL LETTER A WITH DIAERESIS
	decomposed := "\u0410\u0308" // CYRILLIC CAPITAL LETTER A + COMBINING DIAERESIS

	if Normalize(precomposed) != Normalize(decomposed) {
		t.Errorf("decomposed form did not normalize to the precomposed one: %q vs %q",
			Normalize(precomposed), Normalize(decomposed))
	}
}

// TestNormalizeIsIdempotent guards the invariant that the stored trigger and
// the incoming message go through the same stable transformation.
func TestNormalizeIsIdempotent(t *testing.T) {
	inputs := []string{
		"Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді",
		"  МИКС  текст  ",
		"«Кавычки» и — тире",
	}

	for _, input := range inputs {
		once := Normalize(input)
		twice := Normalize(once)
		if once != twice {
			t.Errorf("Normalize is not idempotent for %q: %q then %q", input, once, twice)
		}
	}
}

func TestMatchesExact(t *testing.T) {
	const trigger = "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді"
	keyword := Normalize(trigger)

	shouldMatch := []string{
		trigger,
		"  " + trigger + "  ",
		"АЙРАН/ҚАЙМАҚ КӘСІБІ БОЙЫНША ТЕГІН САБАҚҚА ҚАТЫСҚЫМ КЕЛЕДІ",
		"Айран/Қаймақ   кәсібі бойынша тегін сабаққа қатысқым келеді",
	}
	for _, message := range shouldMatch {
		if !Matches(Normalize(message), keyword, MatchExact) {
			t.Errorf("expected exact match for %q", message)
		}
	}

	shouldNotMatch := []string{
		"",
		"Айран",
		"Сәлеметсіз бе",
		trigger + " рахмет",
		"Мен " + trigger,
	}
	for _, message := range shouldNotMatch {
		if Matches(Normalize(message), keyword, MatchExact) {
			t.Errorf("did not expect exact match for %q", message)
		}
	}
}

func TestMatchesStartsWith(t *testing.T) {
	keyword := Normalize("айран")

	cases := map[string]bool{
		"Айран":              true,
		"Айран қаймақ керек": true,
		"Айран, сәлем":       true,
		"Айрандар":           false, // continues the same word
		"Маған айран керек":  false,
		"":                   false,
	}

	for message, want := range cases {
		if got := Matches(Normalize(message), keyword, MatchStartsWith); got != want {
			t.Errorf("STARTS_WITH %q = %v, want %v", message, got, want)
		}
	}
}

// TestMatchesContainsRespectsWordBoundaries is the guard against the platform
// firing on an unrelated message that merely embeds the letters.
func TestMatchesContainsRespectsWordBoundaries(t *testing.T) {
	keyword := Normalize("айран")

	cases := map[string]bool{
		"маған айран керек":   true,
		"айран":               true,
		"сәлем, айран бар ма": true,
		"айрандарды":          false,
		"байрандық":           false,
		"қайран қалдым":       false,
	}

	for message, want := range cases {
		if got := Matches(Normalize(message), keyword, MatchContains); got != want {
			t.Errorf("CONTAINS %q = %v, want %v", message, got, want)
		}
	}
}

func TestMatchesEmptyInputs(t *testing.T) {
	for _, mode := range []MatchMode{MatchExact, MatchContains, MatchStartsWith} {
		if Matches("", "айран", mode) {
			t.Errorf("%s matched an empty message", mode)
		}
		if Matches("айран", "", mode) {
			t.Errorf("%s matched an empty keyword", mode)
		}
	}
}

func TestValidMatchMode(t *testing.T) {
	for _, mode := range []string{"EXACT", "CONTAINS", "STARTS_WITH"} {
		if !ValidMatchMode(mode) {
			t.Errorf("%s should be valid", mode)
		}
	}
	for _, mode := range []string{"", "exact", "REGEX", "FUZZY"} {
		if ValidMatchMode(mode) {
			t.Errorf("%q should be rejected", mode)
		}
	}
}

func TestFirstName(t *testing.T) {
	cases := map[string]string{
		"Әлішер Сәрсенов": "Әлішер",
		"  Айгүл  ":       "Айгүл",
		"":                "",
		"   ":             "",
	}
	for input, want := range cases {
		if got := FirstName(input); got != want {
			t.Errorf("FirstName(%q) = %q, want %q", input, got, want)
		}
	}
}

func BenchmarkNormalize(b *testing.B) {
	input := "  АЙРАН/ҚАЙМАҚ КӘСІБІ бойынша тегін сабаққа қатысқым келеді  "
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		Normalize(input)
	}
}
