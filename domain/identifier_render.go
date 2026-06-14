package domain

import "strings"

// PascalToSnake converts a Go identifier in PascalCase or camelCase to snake_case.
// Acronym-aware: leading or trailing acronyms are lowercased without internal
// underscores within the acronym.
//
//	Name        → name
//	ZipCode     → zip_code
//	CPF         → cpf
//	URLPath     → url_path
//	HTTPStatus  → http_status
//	OrderLineV2 → order_line_v2
//
// Pure string operation, no IO. Lives in domain because both domain (wire
// path rendering for aggregate validations) and infra (table/column/FK
// inference) consume the same convention — sharing avoids drift.
func PascalToSnake(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if i > 0 && isUpperRune(r) {
			prev := runes[i-1]
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			prevLower := prev >= 'a' && prev <= 'z'
			prevUpper := isUpperRune(prev)
			nextLower := next >= 'a' && next <= 'z'
			if prevLower || (prevUpper && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(toLowerRune(r))
	}
	return b.String()
}

// PluralizeSnake applies basic English plural rules sufficient for both
// table names and wire-format collection segments. Irregulars (person/people,
// datum/data, mouse/mice) are NOT covered — services with such cases use
// per-Repository overrides (fwinfra.RepoConfig.Table /
// RepoConfig.ChildTableOverrides).
//
// Rules:
//   - ends with sh / ch → +es           (watch → watches)
//   - ends with s / x / z → +es         (box → boxes)
//   - ends with consonant + y → -y +ies (city → cities)
//   - everything else → +s              (user → users; address → addresses)
//
// Note: "address" → "addresses" works because it ends in 's' → "+es" rule.
func PluralizeSnake(s string) string {
	if s == "" {
		return ""
	}
	n := len(s)
	if n >= 2 {
		tail := s[n-2:]
		if tail == "sh" || tail == "ch" {
			return s + "es"
		}
	}
	last := s[n-1]
	switch last {
	case 's', 'x', 'z':
		return s + "es"
	case 'y':
		if n >= 2 && !isVowelByte(s[n-2]) {
			return s[:n-1] + "ies"
		}
	}
	return s + "s"
}

func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}
