package queryschema

import (
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Canonical reserved read-side control keys — the DTO vocabulary every surface
// speaks. A surface renders the key in its own wire convention (gRPC snake_case,
// GraphQL argument names), but the DTO declaration, the gate, and the violation
// reporting all anchor on these spellings.
const (
	KeyFirst           = "first"
	KeyLast            = "last"
	KeyAfter           = "after"
	KeyBefore          = "before"
	KeyOrderBy         = "orderBy"
	KeyFields          = "fields"
	KeySearch          = "search"
	KeyIncludeArchived = "includeArchived"
	KeyOnlyTotal       = "onlyTotal"
)

// ControlKeys is the canonical vocabulary as a set — the exact spellings a
// Request DTO may declare as `query:"…"` control scalars, and the wire keys
// the REST parser recognizes as controls. The set is CLOSED: a query-tagged
// scalar carrying neither `filter:"…"` nor `sort:"…"` whose key is not in it is
// a boot fail at ExtractRequestSchema (a dead declaration would opt nothing in
// while OpenAPI advertised it — fail loud at construction instead).
var ControlKeys = map[string]bool{
	KeyFirst: true, KeyLast: true,
	KeyAfter: true, KeyBefore: true,
	KeyOrderBy: true, KeyFields: true,
	KeySearch: true, KeyIncludeArchived: true,
	KeyOnlyTotal: true,
}

// ParseControlBool reads the wire value of a boolean control key. The accepted
// spellings are exactly "true" and "false"; anything else is refused.
//
// Strictness is the point. The same value has to mean the same thing on every
// connector, and the other two already enforce it by wire format — proto `bool`
// and GraphQL `Boolean` cannot carry "1" or "TRUE". A REST parser that quietly
// read anything-but-"true" as false made `?includeArchived=1` behave one way on
// a list route and another on a by-id route.
func ParseControlBool(val string) (bool, bool) {
	switch val {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}

// controlKeyList is the canonical keys in declaration order, for diagnostics.
var controlKeyList = []string{
	KeyFirst, KeyLast, KeyAfter, KeyBefore, KeyOrderBy,
	KeyFields, KeySearch, KeyIncludeArchived, KeyOnlyTotal,
}

// Controls is the surface-neutral snapshot of the reserved read-side controls
// one request carries — presence, plus the values the canonical checks need.
// Each surface adapter fills only what its wire actually received: REST from
// the query string, GraphQL from the connection arguments + selection shape,
// gRPC from the shared omnicore.v1 request components. Nil / false means the
// control was NOT on the wire.
type Controls struct {
	// First / Last carry the requested page size when present (nil = absent).
	// Wire-shape parsing (non-numeric input) is the surface's job; the gate
	// owns positivity and direction.
	First *int64
	Last  *int64

	// Presence flags for the remaining controls. Cursor decodability and
	// filter/operator validation stay with the surface + schema layers; the
	// gate consumes only "was this control asked for".
	After           bool
	Before          bool
	OrderBy         bool
	Fields          bool
	Search          bool
	IncludeArchived bool

	// OnlyTotal separates presence from activation, like First/Last: nil =
	// not on the wire, &false = on the wire but inactive (REST's
	// `?onlyTotal=false`), &true = the only-total mode requested. The DTO
	// opt-in gate keys on presence; the conflict matrix keys on activation —
	// an inactive spelling is a no-op, not a page-shaping conflict.
	OnlyTotal *bool
}

// ViolationKind classifies what a ControlViolation is about, so a surface can
// shape its idiomatic rendering (message detail, metadata) without string
// matching. The wire semantics are identical for all kinds: schema violation,
// rejected before the handler runs.
type ViolationKind int

const (
	// ViolationNotDeclared — the control is on the wire but the endpoint's
	// Request DTO does not declare its `query:"…"` key. The DTO is the single
	// source of truth for what an endpoint exposes; an undeclared control is
	// an endpoint-contract violation, not a framework default.
	ViolationNotDeclared ViolationKind = iota
	// ViolationDirection — forward (first/after) and backward (last/before)
	// controls were mixed. Paging runs in one direction at a time.
	ViolationDirection
	// ViolationNonPositiveSize — first/last carried a size ≤ 0.
	ViolationNonPositiveSize
	// ViolationOnlyTotalConflict — onlyTotal combined with a page-shaping
	// control (fields/orderBy/first/last/after/before). A response carrying
	// solely the total has no page to shape.
	ViolationOnlyTotalConflict
)

// ControlViolation is one canonical-gate rejection. Key is the canonical DTO
// key the violation anchors to (for ViolationOnlyTotalConflict it is the
// conflicting key; Field() renders the composite "onlyTotal[key]" form).
type ControlViolation struct {
	Kind ViolationKind
	Key  string
}

// Field returns the canonical field name the violation is reported under —
// the exact string REST places on the 400 envelope and the other surfaces
// echo in their own error idiom ("onlyTotal[first]", "last", "orderBy", …).
func (v ControlViolation) Field() string {
	if v.Kind == ViolationOnlyTotalConflict {
		return KeyOnlyTotal + "[" + v.Key + "]"
	}
	return v.Key
}

// Message returns the violation as the framework's typed notification message
// — the cross-layer error currency. Every surface renders it through its own
// standard path (REST: 400 envelope via the pipeline; GraphQL: GraphQLError
// via the same translation; gRPC: INVALID_ARGUMENT + google.rpc details), so
// the translation stays contained in each surface while the declaration is
// canonical and drift-free.
func (v ControlViolation) Message() domain.NotificationMessage {
	return domain.NotificationMessage{
		FieldName:    v.Field(),
		Notification: domain.SchemaViolationNotification{},
	}
}

// ValidateControls is the canonical validation gateway for the reserved
// read-side controls. Every surface passes its Controls snapshot through it
// BEFORE dispatching to the handler; the three checks run in a fixed order
// and every violation found is returned (deterministic, most callers render
// the first):
//
//  1. DTO opt-in gate — each control present on the wire must be declared on
//     the Request DTO (reserved = RequestSchema.Reserved). natural exempts
//     the keys a surface expresses natively with no wire name to gate
//     (GraphQL: fields — the selection IS the projection — and onlyTotal —
//     the selection shape is the switch).
//  2. Directional rule — sizes must be positive; forward (first/after) and
//     backward (last/before) are mutually exclusive. One sentence covers
//     first+last, first+before, last+after and after+before. The violation
//     reports the backward-side key (forward is the conventional default).
//  3. Only-total conflict matrix — onlyTotal rejects every page-shaping
//     control present alongside it (fields, orderBy, first, last, after,
//     before). Filters, search and includeArchived stay valid: counting a
//     filtered subset is the canonical use case.
func ValidateControls(reserved map[string]bool, c Controls, natural map[string]bool) []ControlViolation {
	var out []ControlViolation

	// 1. DTO opt-in gate, in canonical key order.
	type presence struct {
		key     string
		present bool
	}
	for _, p := range []presence{
		{KeyFirst, c.First != nil},
		{KeyLast, c.Last != nil},
		{KeyAfter, c.After},
		{KeyBefore, c.Before},
		{KeyOrderBy, c.OrderBy},
		{KeyFields, c.Fields},
		{KeySearch, c.Search},
		{KeyIncludeArchived, c.IncludeArchived},
		{KeyOnlyTotal, c.OnlyTotal != nil},
	} {
		if p.present && !natural[p.key] && !reserved[p.key] {
			out = append(out, ControlViolation{Kind: ViolationNotDeclared, Key: p.key})
		}
	}

	// 2. Directional rule.
	if c.First != nil && *c.First <= 0 {
		out = append(out, ControlViolation{Kind: ViolationNonPositiveSize, Key: KeyFirst})
	}
	if c.Last != nil && *c.Last <= 0 {
		out = append(out, ControlViolation{Kind: ViolationNonPositiveSize, Key: KeyLast})
	}
	forward := c.First != nil || c.After
	backward := c.Last != nil || c.Before
	if forward && backward {
		key := KeyBefore
		if c.Last != nil {
			key = KeyLast
		}
		out = append(out, ControlViolation{Kind: ViolationDirection, Key: key})
	}

	// 3. Only-total conflict matrix — the ACTIVE mode only; a present-but-
	// inactive spelling shapes nothing, so it conflicts with nothing.
	if c.OnlyTotal != nil && *c.OnlyTotal {
		for _, p := range []presence{
			{KeyFields, c.Fields},
			{KeyOrderBy, c.OrderBy},
			{KeyFirst, c.First != nil},
			{KeyLast, c.Last != nil},
			{KeyAfter, c.After},
			{KeyBefore, c.Before},
		} {
			if p.present {
				out = append(out, ControlViolation{Kind: ViolationOnlyTotalConflict, Key: p.key})
			}
		}
	}
	return out
}
