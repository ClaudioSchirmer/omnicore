package graphql

import (
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/vektah/gqlparser/v2/ast"
)

// criteriaPlan is the per-field reflection captured once at schema build: the
// runtime filter allowlist, the wire→Go projection map (for orderBy), and the
// flattened where-field → wire-path lookup. Reused on every request for the
// field; never mutated.
type criteriaPlan struct {
	reqSchema  *queryschema.RequestSchema
	projSchema *queryschema.ProjectionSchema
	whereLeaf  map[string]string // flattened where field name → dotted wire path
	orderField map[string]string // <Entity>OrderField enum value → wire path
}

func newCriteriaPlan(entity string, reqType, respType reflect.Type) *criteriaPlan {
	p := &criteriaPlan{
		reqSchema:  queryschema.ExtractRequestSchema(reqType),
		projSchema: queryschema.ExtractProjectionSchema(respType),
		whereLeaf:  map[string]string{},
	}
	for _, leaf := range filterLeaves(reqType) {
		p.whereLeaf[wireLeafName(leaf.WirePath)] = leaf.WirePath
	}
	_, p.orderField = orderFieldMap(entity, p.reqSchema.Sortable)
	return p
}

// listOps are the operators whose GraphQL value is a list (and whose criteria
// emission splits a comma-joined string).
var listOps = map[string]bool{
	queryschema.OpIn: true, queryschema.OpNin: true,
	queryschema.OpIIn: true, queryschema.OpINin: true,
}

// decodeArgs turns a GraphQL argument map into the surface-neutral
// [queryschema.Read] — the ONE thing this surface owns: how its language spells
// a value. A `where` entry is `{field: {op: value}}`, a list operand is a real
// array, a direction is an enum member, and two controls have no argument at
// all because the language already expresses them (the selection IS the
// projection, and its shape is the only-total switch).
//
// It decides nothing. gqlparser has already cut every argument the SDL does not
// declare, so the lookups here are defense in depth; what the endpoint accepts
// is the Request DTO's answer, applied in [queryschema.BuildCriteria].
//
// The second result is the field name to report when a term resolves to no
// declaration — this surface's own spelling (the enum member), so the consumer
// reads back what it sent.
func (p *criteriaPlan) decodeArgs(args map[string]any) (queryschema.Read, string, *GraphQLError) {
	var in queryschema.Read

	if raw, ok := args["where"].(map[string]any); ok {
		for field, v := range raw {
			wirePath, known := p.whereLeaf[field]
			if !known {
				return in, "", errf("where: unknown filter field %q", field)
			}
			ops, isObject := v.(map[string]any)
			if !isObject {
				continue
			}
			for op, val := range ops {
				in.Filters = append(in.Filters, queryschema.FilterTerm{
					Path: wirePath, Op: op, Values: operandValues(op, val), Raw: field,
				})
			}
		}
	}

	// Relay direction: `first`/`after` page forward, `last`/`before` page
	// backward. A forward+backward mix is the gateway's directional violation.
	if n, ok := toInt64(args["first"]); ok {
		in.Controls.First = &n
	}
	if n, ok := toInt64(args["last"]); ok {
		in.Controls.Last = &n
	}
	if s, ok := args["after"].(string); ok {
		in.Controls.After, in.After = true, s
	}
	if s, ok := args["before"].(string); ok {
		in.Controls.Before, in.Before = true, s
	}
	if s, ok := args["search"].(string); ok {
		in.Controls.Search, in.Search = true, s
	}
	if b, ok := args["includeArchived"].(bool); ok {
		in.Controls.IncludeArchived, in.IncludeArchived = true, b
	}

	// orderBy is a list of `<Entity>Order` inputs — `{ field: <enum>, direction:
	// ASC|DESC }`, an absent direction meaning ASC. The enum member is folded
	// back to the DTO's own wire path and travels with its own spelling, so a
	// refusal names the member the consumer wrote.
	if raw, ok := args["orderBy"]; ok {
		if list, isList := raw.([]any); isList && len(list) > 0 {
			in.Controls.OrderBy = true
			for _, item := range list {
				term, isObject := item.(map[string]any)
				if !isObject {
					return in, "", errf("orderBy: malformed order term %v", item)
				}
				member := asString(term["field"])
				wirePath, known := p.orderField[member]
				if !known {
					return in, queryschema.OrderByField(member), nil
				}
				in.OrderBy = append(in.OrderBy, queryschema.OrderTerm{
					Path: wirePath,
					Desc: asString(term["direction"]) == "DESC",
					Raw:  member,
				})
			}
		}
	}
	return in, "", nil
}

// projectionFromSelection derives a ReadCriteria.Projection (Go field path → 1)
// from the Relay node sub-selection (`edges { node { … } }`) of a read field's
// selection set. Two effects, both matching the REST `?fields=` path: (1) an
// explicitly selected restricted field trips ReadCriteria.Restrict's
// active-reference 403 in ToCriteria (referencesField sees Projection[goPath]==1),
// and (2) Mongo projects only the requested fields (pushdown). Returns nil when no
// node leaf is selected — the resolver then leaves Projection empty (whole-doc,
// the prior behavior). The selection set was already validated against the schema
// by gqlparser, so every leaf resolves through projSchema; a stray token (defensive)
// drops the projection rather than erroring.
//
// Selecting a COMPUTED leaf needs nothing special here: ParseProjection pushes
// that field's declared SOURCES down instead of its own path (which backs no
// column), exactly as REST's `?fields=<computed>` does. The value is then
// derived by the Query's FromQueryResult and rendered from the projected
// Response — what this surface resolves node fields against.
func (p *criteriaPlan) projectionFromSelection(sel ast.SelectionSet, frags ast.FragmentDefinitionList) map[string]int {
	nodeSel := relayNodeSelection(sel, frags)
	if nodeSel == nil {
		return nil
	}
	return projectionFromNode(nodeSel, frags, p.projSchema)
}

// projectionFromNode turns a NODE selection — the leaves of one entity, however
// the field reached them — into a ReadCriteria.Projection. The connection field
// digs its node out of `edges { node { … } }` first; a by-id field IS the node,
// so its own selection set arrives here directly.
//
// It is the same seat for both because the two effects are the same on both:
// Mongo (and the relational loader) return only the requested fields, and an
// explicitly selected restricted field is the ACTIVE reference that trips
// ReadCriteria.Restrict's 403 in ToCriteria. A by-id read that skipped this
// answered 403 on the listing and scrubbed silently on the singular field — the
// same query, two verdicts, decided by which field the consumer happened to
// call.
//
// Returns nil when nothing resolves, which leaves the read whole-document.
func projectionFromNode(sel ast.SelectionSet, frags ast.FragmentDefinitionList, projSchema *queryschema.ProjectionSchema) map[string]int {
	paths := flattenWirePaths("", sel, frags)
	if len(paths) == 0 {
		return nil
	}
	proj, _, _, ok := queryschema.ParseProjection(paths, projSchema)
	if !ok {
		return nil
	}
	return proj
}

// relayNodeSelection returns the selection set under `edges { node { … } }` of a
// connection field, or nil when the query selects no node (e.g. only totalCount).
func relayNodeSelection(sel ast.SelectionSet, frags ast.FragmentDefinitionList) ast.SelectionSet {
	for _, f := range collectFields(sel, frags) {
		if f.Name == "edges" {
			for _, ef := range collectFields(f.SelectionSet, frags) {
				if ef.Name == "node" {
					return ef.SelectionSet
				}
			}
		}
	}
	return nil
}

// onlyTotalSelected reports whether the connection selection requests totalCount
// and neither edges nor pageInfo — the GraphQL idiom for REST's ?onlyTotal=true.
// When true the resolver sets ReadCriteria.OnlyTotal so the reader short-circuits
// to CountDocuments, skipping item materialization and cursor work. __typename is
// a meta field and does not count as a data selection. pageInfo forces the full
// read because its cursors derive from the page items. Filter / search /
// includeArchived still bound the count; a pagination/sort argument
// (conflictingPaginationArg) is instead rejected by the resolver — see there.
func onlyTotalSelected(sel ast.SelectionSet, frags ast.FragmentDefinitionList) bool {
	sawTotal := false
	for _, f := range collectFields(sel, frags) {
		switch f.Name {
		case "totalCount":
			sawTotal = true
		case "edges", "pageInfo":
			return false
		}
	}
	return sawTotal
}

// graphqlNaturalControls are the controls this surface expresses natively,
// with no wire name for the DTO gate to police: the selection IS the
// projection (`fields`), and the selection shape is the only-total switch
// (`onlyTotal`). The gateway's conflict matrix still applies to both.
var graphqlNaturalControls = map[string]bool{
	queryschema.KeyFields:    true,
	queryschema.KeyOnlyTotal: true,
}

// pageInfoOnlySelected reports whether the connection selection asks for
// pageInfo (window edges) with NO edges — the pagination probe: "give me the
// page's boundaries, not its rows". The resolver narrows the read to the
// keyset essentials (keysOnlyProjection) so the reader materializes only the
// ordering values + _id it needs to build cursors and the beyond-edge flags.
// totalCount alongside is fine (the reader counts on every paged read).
func pageInfoOnlySelected(sel ast.SelectionSet, frags ast.FragmentDefinitionList) bool {
	sawPageInfo := false
	for _, f := range collectFields(sel, frags) {
		switch f.Name {
		case "pageInfo":
			sawPageInfo = true
		case "edges":
			return false
		}
	}
	return sawPageInfo
}

// keysOnlyProjection is the minimal inclusion projection a pagination probe
// needs: the ordering fields (whose values compose the keyset tuple). Mongo
// includes _id implicitly on inclusion projections — the tuple's trailing
// element — so an unordered probe degenerates to {_id: 1}.
func keysOnlyProjection(orderBy []queries.OrderByField) map[string]int {
	proj := make(map[string]int, len(orderBy)+1)
	for _, f := range orderBy {
		proj[f.Field] = 1
	}
	if len(proj) == 0 {
		proj["_id"] = 1
	}
	return proj
}

// flattenWirePaths flattens a node selection into dotted wire paths
// (`addresses.city`), recursing into nested object selections; leaf fields (no
// sub-selection) terminate a path. Fragments are flattened via collectFields.
func flattenWirePaths(prefix string, sel ast.SelectionSet, frags ast.FragmentDefinitionList) []string {
	var out []string
	for _, f := range collectFields(sel, frags) {
		wp := f.Name
		if prefix != "" {
			wp = prefix + "." + f.Name
		}
		if len(f.SelectionSet) == 0 {
			out = append(out, wp)
		} else {
			out = append(out, flattenWirePaths(wp, f.SelectionSet, frags)...)
		}
	}
	return out
}

// operandValues renders a GraphQL operand as the operand list the shared
// emitter consumes. A list operator receives a real array here — this language
// spells a list as one — so nothing is joined and re-split, and a comma inside
// a value is just a comma.
func operandValues(op string, v any) []string {
	if queryschema.OperatorTakesList(op) {
		if list, ok := v.([]any); ok {
			out := make([]string, len(list))
			for i, e := range list {
				out[i] = fmt.Sprintf("%v", e)
			}
			return out
		}
	}
	return []string{fmt.Sprintf("%v", v)}
}

// toInt64 coerces a GraphQL numeric argument (int / int64 / float64) to int64.
func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
