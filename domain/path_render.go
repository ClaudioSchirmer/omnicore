package domain

import "strings"

// renderPath turns a structured PathSegment slice into the wire-format field
// string. Name segments are rendered via lowerCamel (PascalCase → camelCase,
// acronym-aware: "CPF" → "cpf", "ZipCode" → "zipCode"); names that already
// start with a lowercase character are passed through verbatim so legacy
// already-lowercase identifiers ("id", "name") stay unchanged. Index segments
// are appended in the form "[N]" with no preceding separator.
//
// Examples:
//
//	[{Name:"Name"}]                                       → "name"
//	[{Name:"CPF"}]                                        → "cpf"
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

// toLowerCamel converts a Go identifier to a JSON-friendly camelCase string.
// Acronym handling: a run of two or more leading uppercase runes is fully
// lowercased ("CPF" → "cpf", "URLPath" → "urlPath", "HTTPStatusCode" →
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
