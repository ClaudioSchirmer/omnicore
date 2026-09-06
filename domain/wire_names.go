package domain

import (
	"reflect"
	"strings"
)

// childCollectionSegment renders an aggregate child's collection segment for
// the notification wire path: the DECLARED collection name (CollectionSegmentOf)
// in camelCase. JSON-facing — the wire path is camelCase everywhere, so this is
// too ("Addresses" → addresses, "OrderLines" → orderLines), matching the
// client's JSON array name. The framework derives no name of its own: the
// domain declares the segment, this only cases it for the wire.
func childCollectionSegment(t reflect.Type) string {
	return toLowerCamel(CollectionSegmentOf(t))
}

// childFieldPath is the wire field for a notification that points at an
// aggregate child collection as a whole (add/change/remove rejections): the
// DECLARED collection segment, emitted as a Path segment so renderPath cases
// it exactly like the scoped-context prefix and the read side do ("Addresses"
// → "addresses"). The Go type name never reaches the wire.
func childFieldPath(item AggregateValueObject) []PathSegment {
	return []PathSegment{{Name: CollectionSegmentOf(reflect.TypeOf(item))}}
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

// ToLowerCamel is the exported form of toLowerCamel. Infra uses it to derive a
// field's JSON-friendly wire token (the same acronym-aware camelCase the
// notification wire paths use) when building a view's tabular-export plan, so a
// CSV/Excel `?fields=` token matches the wire name the rest of the framework
// produces ("ZipCode" → "zipCode", "URLPath" → "urlPath").
func ToLowerCamel(s string) string { return toLowerCamel(s) }

// WireFieldPath renders a dotted Go field path into the wire-format field
// token notifications carry: each "."-separated segment goes through the same
// acronym-aware lower-camel rendering the notification wire paths use
// ("Address.ZipCode" → "address.zipCode", "URLPath" → "urlPath"). A segment
// that already starts lowercase passes through verbatim, so wire-format input
// is idempotent ("cursor" → "cursor"). Emitters that hold a field name in Go
// casing — infra refusals, schema mismatches, read-side restrictions — resolve
// through here so every surface reports the vocabulary the consumer sent,
// never the Go identifier.
func WireFieldPath(goPath string) string {
	if !strings.Contains(goPath, ".") {
		return toLowerCamel(goPath)
	}
	segs := strings.Split(goPath, ".")
	for i, s := range segs {
		segs[i] = toLowerCamel(s)
	}
	return strings.Join(segs, ".")
}

func isUpperRune(r rune) bool { return r >= 'A' && r <= 'Z' }

func toLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r + ('a' - 'A')
	}
	return r
}
