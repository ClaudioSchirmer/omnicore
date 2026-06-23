// Package queryschema is the single source of truth for the read-side DTO
// reflection the framework's HTTP surfaces share: the filter operator
// vocabulary, the Request DTO filter allowlist, and the Response DTO
// projection map. The REST wrappers (web), the OpenAPI generator
// (web/openapi) and the GraphQL endpoint (web/graphql) all consume this
// package, so a new operator or a change to the wire↔Go translation rule
// lives in exactly one place — no per-surface drift.
//
// The package imports only application/queries (for the criteria types the
// emission produces) and stdlib; it never imports web, web/openapi or
// web/graphql, so it sits at the bottom of the read-side dependency DAG.
package queryschema

import (
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
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

// ApplyFilterParam writes a single filter into the criteria map under the
// operator declared on the wire. Empty op maps to equality; the others use
// Mongo-style operator keys ($in, $gte, …) because that is what the canonical
// MongoViewReader consumes verbatim.
//
// Value coercion is driven by spec.GoKind (the leaf's declared Go base
// type). A `*string` leaf keeps "95014" as the literal string "95014"; a
// `*int64` leaf coerces "25" into int64(25). This matches the column type
// the read-side adapter stored so `eq` / `in` / `gte` queries hit the
// canonical index without silent type mismatches.
//
// Partial-match operators (`startswith`, `contains`) and case-insensitive
// variants (`ieq`, `ine`, `istartswith`, `icontains`) emit Mongo `$regex`
// sub-documents at field level — every metacharacter in the user-supplied
// value is escaped via regexp.QuoteMeta so the wire input is treated as a
// literal. The list variants (`iin`, `inin`) emit a queries.RegexMatchList
// sentinel because Mongo `$in` requires the native bson.Regex type, which
// MongoViewReader assembles via translateFilter.
//
// When the same field receives more than one operator on the same call
// (e.g. `?name.startswith=Bob&name.icontains=ob`), the clauses are folded
// into a queries.MultiClause sentinel via mergeClause — the canonical
// MongoViewReader expands MultiClause into a top-level `$and` array so
// every declared operator is honored simultaneously instead of having only
// the last one survive on the map.
func ApplyFilterParam(filter map[string]any, spec FilterSpec, op, value string) {
	field := spec.DocPath
	var clause any
	switch op {
	case "", OpEq:
		clause = coerceValue(value, spec.GoKind)
	case OpIn:
		clause = map[string]any{"$in": coerceList(value, spec.GoKind)}
	case OpNin:
		clause = map[string]any{"$nin": coerceList(value, spec.GoKind)}
	case OpNe:
		clause = map[string]any{"$ne": coerceValue(value, spec.GoKind)}
	case OpGte:
		clause = map[string]any{"$gte": coerceValue(value, spec.GoKind)}
	case OpLte:
		clause = map[string]any{"$lte": coerceValue(value, spec.GoKind)}
	case OpGt:
		clause = map[string]any{"$gt": coerceValue(value, spec.GoKind)}
	case OpLt:
		clause = map[string]any{"$lt": coerceValue(value, spec.GoKind)}
	case OpStartsWith:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value)}
	case OpContains:
		clause = map[string]any{"$regex": regexp.QuoteMeta(value)}
	case OpIEq:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value) + "$", "$options": "i"}
	case OpINe:
		clause = map[string]any{"$not": map[string]any{"$regex": "^" + regexp.QuoteMeta(value) + "$", "$options": "i"}}
	case OpIStartsWith:
		clause = map[string]any{"$regex": "^" + regexp.QuoteMeta(value), "$options": "i"}
	case OpIContains:
		clause = map[string]any{"$regex": regexp.QuoteMeta(value), "$options": "i"}
	case OpIIn:
		clause = queries.RegexMatchList{Patterns: quoteList(value, true), CaseInsensitive: true}
	case OpINin:
		clause = queries.RegexMatchList{Patterns: quoteList(value, true), CaseInsensitive: true, Negate: true}
	default:
		return
	}
	mergeClause(filter, field, clause)
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

// quoteList splits a comma-separated value and applies regexp.QuoteMeta to
// each entry, optionally wrapping with ^...$ to preserve the equality
// semantic of the `iin` / `inin` operators (each pattern matches the whole
// value, not a substring).
func quoteList(value string, anchored bool) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		q := regexp.QuoteMeta(p)
		if anchored {
			q = "^" + q + "$"
		}
		out = append(out, q)
	}
	return out
}

// coerceList splits a comma-separated wire value and coerces each element
// to kind. Used by the in/nin operators where the wire is one key carrying
// multiple values.
func coerceList(value string, kind reflect.Kind) []any {
	vals := strings.Split(value, ",")
	items := make([]any, len(vals))
	for i, v := range vals {
		items[i] = coerceValue(strings.TrimSpace(v), kind)
	}
	return items
}

// coerceValue converts a wire string into the Go type declared by the leaf.
// String-typed leaves keep the value verbatim (no silent int/float parse —
// "95014" stays "95014" so it matches a string-typed Mongo field). Numeric
// leaves attempt the matching parse; on parse failure the value falls back
// to the string verbatim so the downstream query simply returns zero hits
// instead of crashing the wrapper. The kind is the leaf's base kind after
// pointer stripping (collected in walkSchemaLevel).
func coerceValue(s string, kind reflect.Kind) any {
	switch kind {
	case reflect.String:
		return s
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		return s
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
		return s
	case reflect.Float32, reflect.Float64:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return s
	case reflect.Bool:
		if s == "true" {
			return true
		}
		if s == "false" {
			return false
		}
		return s
	default:
		// Unknown / composite kinds (slice, struct surrogates) — pass through
		// as string. The walker only stores scalar leaves today, so this
		// branch is defensive.
		return s
	}
}
