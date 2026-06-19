package domain

import "strings"

// renderPath turns a structured PathSegment slice into the wire-format field
// string. Name segments are rendered via lowerCamel (PascalCase → camelCase,
// acronym-aware: "URL" → "url", "ZipCode" → "zipCode"); names that already
// start with a lowercase character are passed through verbatim so legacy
// already-lowercase identifiers ("id", "name") stay unchanged. Index segments
// are appended in the form "[N]" with no preceding separator.
//
// Examples:
//
//	[{Name:"Name"}]                                       → "name"
//	[{Name:"URL"}]                                        → "url"
//	[{Name:"ZipCode"}]                                    → "zipCode"
//	[{Name:"Addresses"}, {Index:0}, {Name:"ZipCode"}]     → "addresses[0].zipCode"
//	[{Name:"id"}]                                         → "id"
func renderPath(path []PathSegment) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	wroteAny := false
	for _, seg := range path {
		if seg.Index != nil {
			b.WriteByte('[')
			b.WriteString(itoa(*seg.Index))
			b.WriteByte(']')
			wroteAny = true
			continue
		}
		if seg.Name == "" {
			continue
		}
		if wroteAny {
			b.WriteByte('.')
		}
		b.WriteString(toLowerCamel(seg.Name))
		wroteAny = true
	}
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// childCollectionSegment renders an aggregate child's collection segment for the
// notification wire path: the type name in camelCase, pluralized. JSON-facing —
// the wire path is camelCase everywhere, so this is too (Address → addresses,
// OrderLine → orderLines), matching the client's JSON array name. The framework
// no longer has column/table convention; this is the one remaining wire-naming
// derivation, and it stays camelCase.
func childCollectionSegment(typeName string) string {
	return PluralizeWord(toLowerCamel(typeName))
}

// PluralizeWord applies basic English plural rules to a word, preserving its
// case (the last word carries the plural): "Address" → "Addresses",
// "OrderLine" → "OrderLines", "Category" → "Categories". Irregulars are not
// covered. Exported so infra can derive a local view embed's Go segment from
// its schema's type name.
func PluralizeWord(s string) string {
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
	switch s[n-1] {
	case 's', 'x', 'z':
		return s + "es"
	case 'y':
		if n >= 2 && !isVowelByte(s[n-2]) {
			return s[:n-1] + "ies"
		}
	}
	return s + "s"
}

// toLowerCamel converts a Go identifier to a JSON-friendly camelCase string.
// Acronym handling: a run of two or more leading uppercase runes is fully
// lowercased ("URL" → "url", "URLPath" → "urlPath", "HTTPStatusCode" →
// "httpStatusCode"). A single leading uppercase becomes a single lowercase
// ("Name" → "name"). Strings starting with a non-uppercase rune are returned
// as-is so already-lowercase literals ("id", "service") stay stable.
func toLowerCamel(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	if !isUpperRune(runes[0]) {
		return s
	}
	// Find the length of the leading uppercase run.
	upperRun := 1
	for upperRun < len(runes) && isUpperRune(runes[upperRun]) {
		upperRun++
	}
	switch {
	case upperRun == 1:
		runes[0] = toLowerRune(runes[0])
	case upperRun == len(runes):
		// All-uppercase string → all lowercase.
		for i := range runes {
			runes[i] = toLowerRune(runes[i])
		}
	default:
		// Acronym followed by a Camel word: lowercase the acronym except its
		// last letter, which starts the next word ("URLPath" → "urlPath").
		for i := 0; i < upperRun-1; i++ {
			runes[i] = toLowerRune(runes[i])
		}
	}
	return string(runes)
}

func isUpperRune(r rune) bool { return r >= 'A' && r <= 'Z' }

func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}

func isVowelByte(b byte) bool {
	switch b {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	}
	return false
}
