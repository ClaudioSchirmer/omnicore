package graphql

import (
	"fmt"
	"reflect"
	"strings"

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
	_, p.orderField = orderFieldMap(entity, p.projSchema)
	return p
}

// listOps are the operators whose GraphQL value is a list (and whose criteria
// emission splits a comma-joined string).
var listOps = map[string]bool{
	queryschema.OpIn: true, queryschema.OpNin: true,
	queryschema.OpIIn: true, queryschema.OpINin: true,
}

// buildCriteria translates a read field's GraphQL arguments into a
// ReadCriteria, reusing the REST emission (queryschema.ApplyFilterParam) so the
// where clause folds to the IDENTICAL Mongo criteria a `?name.startswith=…`
// REST call produces. Pagination / orderBy / search / includeArchived map 1:1.
// Cursor context-hash validation is left to the reader (it already rejects a
// stale cursor against the rebuilt criteria).
//
// The returned Controls is the canonical snapshot for the control gateway —
// the resolver completes it (only-total selection shape) and runs
// queryschema.ValidateControls before dispatch. The SDL already cut
// undeclared args from the schema (gqlparser rejects them as unknown
// arguments), so the gate here is defense in depth; the directional rule and
// the only-total conflicts are the live checks.
func (p *criteriaPlan) buildCriteria(args map[string]any) (queries.ReadCriteria, queryschema.Controls, *GraphQLError) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}
	var controls queryschema.Controls

	if raw, ok := args["where"].(map[string]any); ok {
		for field, v := range raw {
			wirePath, known := p.whereLeaf[field]
			if !known {
				return crit, controls, errf("where: unknown filter field %q", field)
			}
			ops, ok := v.(map[string]any)
			if !ok {
				continue
			}
			spec := p.reqSchema.Filters[wirePath]
			for op, val := range ops {
				queryschema.ApplyFilterParam(crit.Filter, spec, op, gqlValueToWire(op, val))
			}
		}
	}

	// Relay direction: `first`/`after` page forward, `last`/`before` page
	// backward. `last` is the only arg that carries direction on its own (it can
	// stand without a cursor, walking back from the end), so it sets Backward;
	// `before` reaches the reader as a cursor, which already implies backward
	// there. A forward+backward mix is the gateway's directional violation.
	if n, ok := toInt64(args["first"]); ok {
		first := n
		controls.First = &first
		crit.Limit = n
	}
	if n, ok := toInt64(args["last"]); ok {
		last := n
		controls.Last = &last
		crit.Limit = n
		crit.Backward = true
	}
	if s, ok := args["after"].(string); ok {
		controls.After = true
		crit.After = s
	}
	if s, ok := args["before"].(string); ok {
		controls.Before = true
		crit.Before = s
	}
	if s, ok := args["search"].(string); ok {
		controls.Search = true
		crit.Search = s
	}
	if b, ok := args["includeArchived"].(bool); ok {
		controls.IncludeArchived = true
		crit.IncludeArchived = b
	}
	// orderBy is a list of `<Entity>Order` inputs — `{ field: <enum>, direction:
	// ASC|DESC }`. gqlparser already validated enum membership against the SDL,
	// so the lookup miss below is defense in depth. The fold lands on the SAME
	// OrderByField terms the REST `?orderBy=-name` tokens produce (wire path →
	// Go doc path via the projection schema), so keyset cursors stay valid and
	// interchangeable across surfaces. An absent direction is ASC.
	if raw, ok := args["orderBy"]; ok {
		if list, lok := raw.([]any); lok && len(list) > 0 {
			controls.OrderBy = true
			terms := make([]queries.OrderByField, 0, len(list))
			for _, item := range list {
				term, tok := item.(map[string]any)
				if !tok {
					return crit, controls, errf("orderBy: malformed order term %v", item)
				}
				val := asString(term["field"])
				wire, known := p.orderField[val]
				if !known {
					return crit, controls, errf("orderBy: %q is not a sortable field", val)
				}
				terms = append(terms, queries.OrderByField{
					Field: p.projSchema.Paths[wire],
					Desc:  asString(term["direction"]) == "DESC",
				})
			}
			crit.OrderBy = terms
		}
	}
	return crit, controls, nil
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
func (p *criteriaPlan) projectionFromSelection(sel ast.SelectionSet, frags ast.FragmentDefinitionList) map[string]int {
	nodeSel := relayNodeSelection(sel, frags)
	if nodeSel == nil {
		return nil
	}
	paths := flattenWirePaths("", nodeSel, frags)
	if len(paths) == 0 {
		return nil
	}
	proj, _, _, ok := queryschema.ParseProjection(strings.Join(paths, ","), p.projSchema)
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

// gqlValueToWire renders a GraphQL argument value into the string form
// queryschema.ApplyFilterParam coerces (by the leaf's Go kind). List operators
// receive a comma-joined string (ApplyFilterParam splits on comma), mirroring
// the REST `?x.in=a,b,c` wire.
func gqlValueToWire(op string, v any) string {
	if listOps[op] {
		if list, ok := v.([]any); ok {
			parts := make([]string, len(list))
			for i, e := range list {
				parts[i] = fmt.Sprintf("%v", e)
			}
			return strings.Join(parts, ",")
		}
	}
	return fmt.Sprintf("%v", v)
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

