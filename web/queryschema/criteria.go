package queryschema

import (
	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// Read is one read request as the WIRE carried it: decoded, not interpreted.
//
// Every surface fills it from its own idiom — a query string, a GraphQL
// argument map, a proto message — and hands it to [BuildCriteria]. What any of
// it MEANS is decided there, once: the DTO opt-in gate, the directional rule,
// the only-total conflict matrix, the operator allowlist, the ordering
// vocabulary and its direction and duplicate rules, the projection allowlist
// and the `_id` auto-exclusion.
//
// A surface therefore owns exactly two things, which is all that genuinely
// differs between them: how its wire spells a value, and how it renders a
// refusal. It owns no opinion about what the endpoint accepts — that is the
// Request DTO's, and it is the same answer on every wire.
//
// The paths here are the DTO's OWN wire spelling (`createdAt`,
// `addresses.city`). A surface whose wire uses another spelling — gRPC's
// snake_case, GraphQL's SCREAMING_SNAKE enum — translates on the way in and
// reports on the way out through Raw, so the consumer reads its own spelling
// back on a violation.
type Read struct {
	// Controls is the reserved-control snapshot the canonical gate consumes:
	// presence, plus the values the gate needs.
	Controls Controls
	// Natural names the controls this surface expresses without a wire key, so
	// the opt-in gate does not ask a DTO to declare something the language
	// itself provides (GraphQL: the selection IS the projection, and its shape
	// is the only-total switch).
	Natural map[string]bool

	Filters []FilterTerm
	OrderBy []OrderTerm
	// Fields are the projection tokens in the Response's wire spelling.
	Fields []string
	// Projection is a projection the surface resolved ITSELF, for a wire whose
	// selection vocabulary is its own output shape rather than the Response's
	// tokens: a GraphQL selection set, a proto FieldMask over the item
	// message. It is applied verbatim — the surface owns what its own shape
	// admits — and Fields is the token path for the wires that have one.
	Projection map[string]int

	// The values behind the presence flags on Controls.
	Search          string
	IncludeArchived bool
	After           string
	Before          string
}

// FilterTerm is one filter the wire carried, resolved to the DTO's vocabulary.
type FilterTerm struct {
	// Path is the leaf's declared wire path; Op is the declared operator, ""
	// meaning equality.
	Path string
	Op   string
	// Values are the operand(s) as strings. Every surface reaches this shape:
	// the coercion to the leaf's Go kind is the schema's job, not the wire's.
	Values []string
	// Raw is the token as THIS wire spelled it, reported verbatim on a
	// refusal (`code.startswith` on REST, the field name on the others).
	Raw string
}

// OrderTerm is one ordering term the wire carried, resolved to the DTO's
// vocabulary.
type OrderTerm struct {
	Path string
	Desc bool
	// Raw is the token as THIS wire spelled it — `-code` on REST, the enum
	// member on GraphQL, the entry on gRPC — so a refusal names what the
	// consumer actually sent.
	Raw string
}

// BuildCriteria turns a decoded request into the criteria the handler receives,
// or the first violation that stops it.
//
// The order is deliberate and is the same on every surface. A malformed or
// undeclared FILTER wins over the control gate, because it names a key the
// endpoint does not have at all. The gate then answers for the controls —
// opt-in, direction, only-total conflicts — before the page window is
// materialized, so a directional conflict is reported as such rather than as a
// cursor that does not fit. The cursor's STRUCTURE is checked last, against the
// criteria the wire produced.
//
// selectedWire is the set of Response wire paths the consumer selected via
// Fields, which the render uses to blank sources read only to feed a selected
// computed field. It is nil when the request selected nothing.
func BuildCriteria(s *RequestSchema, proj *ProjectionSchema, in Read) (queries.ReadCriteria, map[string]bool, *Violation, bool) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}

	for _, term := range in.Filters {
		spec, declared := s.Filters[term.Path]
		op := term.Op
		if op == "" {
			op = OpEq
		}
		if !declared || !spec.Ops[op] {
			return crit, nil, SchemaViolation(term.Raw), false
		}
		ApplyFilterValues(crit.Filter, spec, op, term.Values)
	}

	crit.Search = in.Search
	crit.IncludeArchived = in.IncludeArchived
	crit.After = in.After
	crit.Before = in.Before
	if in.Controls.OnlyTotal != nil {
		crit.OnlyTotal = *in.Controls.OnlyTotal
	}

	// The ordering vocabulary is consulted only on an endpoint that declares
	// the control. Without it the gate below refuses the parameter itself, and
	// reporting a token would blame the consumer's spelling for something the
	// endpoint never offered.
	if s.Reserved[KeyOrderBy] {
		order, violation, ok := resolveOrdering(s.Sortable, in.OrderBy)
		if !ok {
			return crit, nil, violation, false
		}
		crit.OrderBy = order
	}

	var selectedWire map[string]bool
	if len(in.Projection) > 0 {
		crit.Projection = in.Projection
	}
	if len(in.Fields) > 0 {
		projection, wireSet, bad, ok := ParseProjection(in.Fields, proj)
		if !ok {
			return crit, nil, SchemaViolation(FieldsField(bad)), false
		}
		if proj != nil && !wireSet["id"] {
			// Mongo always returns `_id` unless explicitly excluded. The
			// consumer did not ask for `id`, so drop it: the Response renders
			// every field as an omitempty pointer, and an id nobody requested
			// would still reach the wire.
			projection["_id"] = 0
		}
		crit.Projection = projection
		selectedWire = wireSet
	}

	if violations := ValidateControls(s.Reserved, in.Controls, in.Natural); len(violations) > 0 {
		v := violations[0]
		return crit, nil, &Violation{Field: v.Field(), Notification: v.Message().Notification}, false
	}

	// The Relay direction pair becomes the internal size + direction: first=N
	// is a forward window of N, last=N a backward one (with no cursor, the LAST
	// N of the set).
	if in.Controls.First != nil {
		crit.Limit = *in.Controls.First
	}
	if in.Controls.Last != nil {
		crit.Limit = *in.Controls.Last
		crit.Backward = true
	}

	for _, c := range []struct {
		value string
		key   string
	}{{crit.After, KeyAfter}, {crit.Before, KeyBefore}} {
		if c.value == "" {
			continue
		}
		if !cursorFitsCriteria(c.value, crit) {
			return crit, nil, SchemaViolation(c.key), false
		}
	}
	return crit, selectedWire, nil, true
}

// resolveOrdering validates the ordering terms against the declared vocabulary
// and translates them to Go field paths, which the reader resolves to physical
// columns through the view's TableSchema — the same two hops a filter leaf
// takes.
//
// A token outside the vocabulary, one asking for a direction the declaration
// does not admit, and a path named twice are all refused the same way, on the
// token the consumer sent. The duplicate is not pedantry: the terms become the
// reader's sort document, where a repeated key is malformed and the store
// refuses the whole read.
func resolveOrdering(sortable map[string]SortSpec, terms []OrderTerm) ([]queries.OrderByField, *Violation, bool) {
	if len(terms) == 0 {
		return nil, nil, true
	}
	out := make([]queries.OrderByField, 0, len(terms))
	seen := make(map[string]bool, len(terms))
	for _, t := range terms {
		spec, declared := sortable[t.Path]
		if !declared || !spec.Allows(t.Desc) || seen[t.Path] {
			return nil, SchemaViolation(OrderByField(t.Raw)), false
		}
		seen[t.Path] = true
		out = append(out, queries.OrderByField{Field: spec.GoPath, Desc: t.Desc})
	}
	return out, nil, true
}

// cursorFitsCriteria asserts the cursor's STRUCTURE against the criteria the
// wire produced: it must decode, and its key tuple must be one longer than the
// ordering (the trailing element is always `_id`). Both failures are the
// canonical 400 on the cursor's own wire key.
//
// The CONTEXT-HASH check deliberately does not run here. At this layer the
// criteria is the WIRE snapshot, taken before the Query's ToCriteria(ctx)
// layers identity overlays onto it, while the reader stamps outgoing cursors
// from the post-ToCriteria criteria it received. Comparing the two would reject
// every legitimate cursor the moment a paged query carries an overlay. The
// authoritative check lives in the reader, against the same snapshot it stamps.
func cursorFitsCriteria(cursor string, crit queries.ReadCriteria) bool {
	decoded, err := queries.DecodeCursor(cursor)
	if err != nil {
		return false
	}
	return len(decoded.K)-1 == len(crit.OrderBy)
}
