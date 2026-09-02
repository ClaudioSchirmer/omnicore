// Package queryschema is the single source of truth for the read-side DTO
// reflection the framework's HTTP surfaces share: the filter operator
// vocabulary, the Request DTO filter allowlist, and the Response DTO
// projection map. The REST wrappers (web), the OpenAPI generator
// (web/openapi) and the GraphQL endpoint (web/graphql) all consume this
// package, so a new operator or a change to the wire↔Go translation rule
// lives in exactly one place — no per-surface drift.
//
// The package imports application/queries (for the criteria types the emission
// produces), domain (for the notifications a Violation carries) and stdlib; it
// never imports web, web/openapi or web/graphql, so it sits at the bottom of
// the read-side dependency DAG.
package queryschema

import (
	"reflect"
	"strconv"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Operator constants for filter declarations in a Request DTO's `filter:"..."`
// struct tag. The Op prefix avoids collision with any consumer-defined Op
// values (e.g. an OpenAPI operation enum) and groups them as a coherent set.
//
// The string operators come in case-sensitive and case-insensitive variants
// (prefixed with `i`): a field declaring `filter:"eq,ieq,startswith,istartswith"`
// accepts `?name=Bob` (exact), `?name.ieq=bob` (case-insensitive equality),
// `?name.startswith=Bob` (prefix), and `?name.istartswith=bob` (case-insensitive
// prefix) — each is opt-in per call. Numeric operators (gte/lte/gt/lt) have no
// `i` variant by design — case-folding has no meaning on ordinal comparisons.
const (
	OpEq          = "eq"
	OpNe          = "ne"
	OpIn          = "in"
	OpNin         = "nin"
	OpGte         = "gte"
	OpLte         = "lte"
	OpGt          = "gt"
	OpLt          = "lt"
	OpStartsWith  = "startswith"
	OpContains    = "contains"
	OpIEq         = "ieq"
	OpINe         = "ine"
	OpIIn         = "iin"
	OpINin        = "inin"
	OpIStartsWith = "istartswith"
	OpIContains   = "icontains"
)

// FilterSpec lists the operators declared by a single filter leaf via the
// `filter:"eq,in"` struct tag, together with the Go field path the leaf maps
// to and the leaf's declared Go base kind used to coerce wire values into
// typed criteria. DocPath holds the dotted Go field path (the `query:` keys
// joined down the embed groups, e.g. `Addresses.ZipCode`); the
// MongoViewReader translates it to the physical column via the view's
// TableSchema. There is no `view:` tag. GoKind drives value coercion at
// runtime — a `*string` leaf keeps "95014" as the literal string "95014" (no
// silent int parse), matching the column type Mongo stored; a `*int64` leaf
// parses "25" into int64(25) so the criteria type matches the field's stored
// type.
type FilterSpec struct {
	Ops     map[string]bool
	DocPath string
	GoKind  reflect.Kind
}

// knownOps is the membership set of every declared operator constant. Drives
// ParseKeyAgainstSchema's "peel only if last segment is a known op" rule —
// keeping it in sync with the OpXxx constants above is the contract.
var knownOps = map[string]bool{
	OpEq:          true,
	OpNe:          true,
	OpIn:          true,
	OpNin:         true,
	OpGte:         true,
	OpLte:         true,
	OpGt:          true,
	OpLt:          true,
	OpStartsWith:  true,
	OpContains:    true,
	OpIEq:         true,
	OpINe:         true,
	OpIIn:         true,
	OpINin:        true,
	OpIStartsWith: true,
	OpIContains:   true,
}

// ParseKeyAgainstSchema resolves a wire key into (wirePath, op). The logic
// is whole-key-first: if the literal key is a declared filter, no operator
// is peeled (handles fields whose name happens to match an op, e.g. a leaf
// declared `query:"in"`). Otherwise, if the key ends in a known operator
// suffix and the remaining prefix is a declared filter, the suffix is
// honored as the operator. Returns ("", "") when the key matches neither —
// caller surfaces the wire key verbatim in the 400 envelope.
func ParseKeyAgainstSchema(key string, s *RequestSchema) (string, string) {
	if _, ok := s.Filters[key]; ok {
		return key, ""
	}
	idx := strings.LastIndexByte(key, '.')
	if idx < 0 {
		return "", ""
	}
	wirePath, op := key[:idx], key[idx+1:]
	if !knownOps[op] {
		return "", ""
	}
	if _, ok := s.Filters[wirePath]; !ok {
		return "", ""
	}
	return wirePath, op
}

// OperatorTakesList reports whether an operator consumes MANY operands. It is
// the one fact a wire needs in order to spell a list: a query string packs
// them comma-separated, a proto sends `repeated`, a GraphQL input sends an
// array. How the list is spelled is the wire's business; which operators take
// one is not, so it is answered here.
func OperatorTakesList(op string) bool {
	switch op {
	case OpIn, OpNin, OpIIn, OpINin:
		return true
	default:
		return false
	}
}

// ApplyFilterValues is the wire-neutral filter emitter — the single place the
// canonical ReadCriteria.Filter clauses are built, and the only one. Every
// surface reaches it with the operands already separated: a query string split
// its commas, a proto passed its `repeated` values verbatim, a GraphQL input
// passed its array. List operators consume every element; scalar operators
// consume values[0], which is why an EMPTY operand (`?name.contains=`) must
// arrive as one empty string and not as no values at all. Unknown operators are
// ignored — the caller validates the allowlist before emission.
//
// Returns a Violation when a value cannot be the type the leaf declares
// ("abc" on an int leaf): the refusal is born here, at the one place that
// knows BOTH the declared kind and the wire key the consumer wrote, so every
// surface reports the same typed 400 instead of letting the probe travel to
// the driver. Returns nil when the clause was emitted.
func ApplyFilterValues(filter map[string]any, spec FilterSpec, wireKey, op string, values []string) *Violation {
	if len(values) == 0 {
		return nil
	}
	invalid := func(v string) *Violation {
		return &Violation{
			Field:        wireKey,
			Notification: domain.InvalidFilterValueNotification{},
			Value:        v,
		}
	}
	field := spec.DocPath
	value := values[0]
	var clause any
	switch op {
	case "", OpEq:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = coerced
	case OpIn:
		items, bad, ok := coerceValues(values, spec.GoKind)
		if !ok {
			return invalid(bad)
		}
		clause = queries.Clause{Op: queries.FilterIn, Values: items}
	case OpNin:
		items, bad, ok := coerceValues(values, spec.GoKind)
		if !ok {
			return invalid(bad)
		}
		clause = queries.Clause{Op: queries.FilterNin, Values: items}
	case OpNe:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = queries.Clause{Op: queries.FilterNe, Values: []any{coerced}}
	case OpGte:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = queries.Clause{Op: queries.FilterGte, Values: []any{coerced}}
	case OpLte:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = queries.Clause{Op: queries.FilterLte, Values: []any{coerced}}
	case OpGt:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = queries.Clause{Op: queries.FilterGt, Values: []any{coerced}}
	case OpLt:
		coerced, ok := coerceValue(value, spec.GoKind)
		if !ok {
			return invalid(value)
		}
		clause = queries.Clause{Op: queries.FilterLt, Values: []any{coerced}}
	case OpStartsWith:
		clause = queries.TextMatch{Value: value, Kind: queries.TextPrefix}
	case OpContains:
		clause = queries.TextMatch{Value: value, Kind: queries.TextContains}
	case OpIEq:
		clause = queries.TextMatch{Value: value, Kind: queries.TextExact, CaseInsensitive: true}
	case OpINe:
		clause = queries.TextMatch{Value: value, Kind: queries.TextExact, CaseInsensitive: true, Negate: true}
	case OpIStartsWith:
		clause = queries.TextMatch{Value: value, Kind: queries.TextPrefix, CaseInsensitive: true}
	case OpIContains:
		clause = queries.TextMatch{Value: value, Kind: queries.TextContains, CaseInsensitive: true}
	case OpIIn:
		clause = queries.TextMatchList{Values: values, CaseInsensitive: true}
	case OpINin:
		clause = queries.TextMatchList{Values: values, CaseInsensitive: true, Negate: true}
	default:
		return nil
	}
	mergeClause(filter, field, clause)
	return nil
}

// splitTrim splits a comma-separated wire value into trimmed elements —
// the query-string list convention.
func splitTrim(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// mergeClause folds a new clause into the criteria map under field. The
// first clause for a field lands as a plain value (scalar for `eq`, the
// operator sub-document for the variants); a second clause for the same
// field promotes both into queries.MultiClause; further clauses append to
// the existing MultiClause. The canonical MongoViewReader expands
// MultiClause into a top-level `$and` array — every declared operator is
// honored simultaneously instead of having only the last write on the map
// survive.
func mergeClause(filter map[string]any, field string, clause any) {
	existing, ok := filter[field]
	if !ok {
		filter[field] = clause
		return
	}
	if mc, isMulti := existing.(queries.MultiClause); isMulti {
		mc.Clauses = append(mc.Clauses, clause)
		filter[field] = mc
		return
	}
	filter[field] = queries.MultiClause{Clauses: []any{existing, clause}}
}

// coerceValues coerces each element to kind — the list-level core shared by
// both wires.
// A list refuses on its FIRST unusable element and names it: an `in` list is
// one predicate, and silently dropping the member that did not parse would
// answer a question nobody asked.
func coerceValues(values []string, kind reflect.Kind) (items []any, bad string, ok bool) {
	items = make([]any, len(values))
	for i, v := range values {
		coerced, valid := coerceValue(v, kind)
		if !valid {
			return nil, v, false
		}
		items[i] = coerced
	}
	return items, "", true
}

// coerceValue converts a wire string into the Go type declared by the leaf.
// String-typed leaves keep the value verbatim (no silent int/float parse —
// "95014" stays "95014" so it matches a string-typed Mongo field). A typed leaf
// attempts the matching parse and REFUSES on failure (ok=false): the old
// fallback to the string verbatim was only harmless on Mongo, where a mistyped
// probe matches nothing — on every relational backing it reached the driver and
// came back as a 500. The kind is the leaf's base kind after pointer stripping
// (collected in walkSchemaLevel).
func coerceValue(s string, kind reflect.Kind) (any, bool) {
	switch kind {
	case reflect.String:
		return s, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, 64)
		return n, err == nil
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case reflect.Bool:
		if s == "true" {
			return true, true
		}
		if s == "false" {
			return false, true
		}
		return nil, false
	default:
		// Unknown / composite kinds (slice, struct surrogates) — pass through
		// as string. The walker only stores scalar leaves today, so this
		// branch is defensive. An identity or time leaf lands here as a plain
		// string and is guarded by the reader instead, which is the only layer
		// that knows the column's real type.
		return s, true
	}
}
