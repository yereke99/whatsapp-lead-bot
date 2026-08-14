// Package textnorm normalizes user-supplied text so that trigger keywords
// match reliably across keyboards, platforms and casing.
//
// Normalization must be deterministic and identical on both sides of a
// comparison: the value stored in campaign_triggers.normalized_keyword is
// produced by the same function used on an incoming WhatsApp message.
package textnorm

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

var caseFolder = cases.Fold()

// Normalize prepares text for trigger comparison:
//
//   - Unicode NFKC composition, so decomposed Kazakh glyphs such as
//     "ә" written as "а"+U+0308 equal their precomposed form;
//   - lookalike separators, dashes and quotes folded to ASCII equivalents;
//   - zero-width and formatting characters removed;
//   - Unicode case folding, which lowercases Cyrillic correctly;
//   - leading/trailing whitespace trimmed and internal runs collapsed
//     to a single space.
//
// Punctuation is deliberately preserved: a trigger phrase such as
// "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді" depends on it.
func Normalize(s string) string {
	if s == "" {
		return ""
	}

	s = norm.NFKC.String(s)

	var b strings.Builder
	b.Grow(len(s))

	pendingSpace := false
	wroteAny := false

	for _, r := range s {
		if isIgnorable(r) {
			continue
		}
		if unicode.IsSpace(r) {
			// Collapse any run of whitespace into one space, and never emit a
			// leading one.
			if wroteAny {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			b.WriteRune(' ')
			pendingSpace = false
		}
		b.WriteRune(foldLookalike(r))
		wroteAny = true
	}

	return caseFolder.String(b.String())
}

// NormalizeName tidies a display name without case folding it.
func NormalizeName(s string) string {
	s = norm.NFKC.String(s)
	return strings.Join(strings.Fields(s), " ")
}

// Title upper-cases the first letter of a normalized name, leaving the rest
// untouched so acronyms survive.
func Title(s string) string {
	s = NormalizeName(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}

// FirstName returns the first whitespace-separated word of a name.
func FirstName(s string) string {
	fields := strings.Fields(NormalizeName(s))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// isIgnorable reports characters that carry no meaning for matching but are
// routinely pasted in from other apps.
func isIgnorable(r rune) bool {
	switch r {
	case '\u200B', // zero width space
		'\u200C', // zero width non-joiner
		'\u200D', // zero width joiner
		'\u2060', // word joiner
		'\uFEFF', // byte order mark
		'\u00AD': // soft hyphen
		return true
	}
	if r >= '\uFE00' && r <= '\uFE0F' { // variation selectors
		return true
	}
	// Remaining Unicode format characters (bidi marks and friends).
	return unicode.Is(unicode.Cf, r)
}

// foldLookalike maps typographic variants onto their plain ASCII counterpart
// so a phrase copied out of a document still matches one typed by hand.
func foldLookalike(r rune) rune {
	switch r {
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	case '‘', '’', '‚', '‛', '′', '´', '`':
		return '\''
	case '“', '”', '„', '‟', '″', '«', '»':
		return '"'
	case '⁄', '∕', '／':
		return '/'
	case '…':
		// Ellipsis expands to a single dot; the length difference is
		// irrelevant because trigger phrases rarely end in one.
		return '.'
	}
	return r
}

// MatchMode enumerates how a trigger keyword is compared against a message.
type MatchMode string

const (
	MatchExact      MatchMode = "EXACT"
	MatchContains   MatchMode = "CONTAINS"
	MatchStartsWith MatchMode = "STARTS_WITH"
)

// ValidMatchMode reports whether mode is one the platform understands.
func ValidMatchMode(mode string) bool {
	switch MatchMode(mode) {
	case MatchExact, MatchContains, MatchStartsWith:
		return true
	}
	return false
}

// Matches compares an already-normalized message against an already-normalized
// keyword using the given mode. Both arguments must come from Normalize.
//
// CONTAINS is intentionally word-boundary aware so that a short keyword does
// not fire on an unrelated message that merely embeds those letters.
func Matches(normalizedMessage, normalizedKeyword string, mode MatchMode) bool {
	if normalizedMessage == "" || normalizedKeyword == "" {
		return false
	}

	switch mode {
	case MatchExact:
		return normalizedMessage == normalizedKeyword

	case MatchStartsWith:
		if !strings.HasPrefix(normalizedMessage, normalizedKeyword) {
			return false
		}
		return endsOnBoundary(normalizedMessage, len(normalizedKeyword))

	case MatchContains:
		offset := 0
		for {
			idx := strings.Index(normalizedMessage[offset:], normalizedKeyword)
			if idx < 0 {
				return false
			}
			start := offset + idx
			end := start + len(normalizedKeyword)
			if startsOnBoundary(normalizedMessage, start) && endsOnBoundary(normalizedMessage, end) {
				return true
			}
			offset = start + 1
			if offset >= len(normalizedMessage) {
				return false
			}
		}
	}

	return false
}

func startsOnBoundary(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	prev := []rune(s[:idx])
	return !isWordRune(prev[len(prev)-1])
}

func endsOnBoundary(s string, idx int) bool {
	if idx >= len(s) {
		return true
	}
	for _, r := range s[idx:] {
		return !isWordRune(r)
	}
	return true
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
