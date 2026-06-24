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
}

func newCriteriaPlan(reqType, respType reflect.Type) *criteriaPlan {
	p := &criteriaPlan{
		reqSchema:  queryschema.ExtractRequestSchema(reqType),
		projSchema: queryschema.ExtractProjectionSchema(respType),
		whereLeaf:  map[string]string{},
	}
	for _, leaf := range filterLeaves(reqType) {
		p.whereLeaf[wireLeafName(leaf.WirePath)] = leaf.WirePath
	}
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
// REST call produces. Pagination / sort / search / includeArchived map 1:1.
// Cursor context-hash validation is left to the reader (it already rejects a
// stale cursor against the rebuilt criteria).
func (p *criteriaPlan) buildCriteria(args map[string]any) (queries.ReadCriteria, *GraphQLError) {
	crit := queries.ReadCriteria{Filter: map[string]any{}}

	if raw, ok := args["where"].(map[string]any); ok {
		for field, v := range raw {
			wirePath, known := p.whereLeaf[field]
			if !known {
				return crit, errf("where: unknown filter field %q", field)
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

	if n, ok := toInt64(args["first"]); ok {
		crit.Limit = n
	}
	if n, ok := toInt64(args["last"]); ok {
		crit.Limit = n
	}
	if s, ok := args["after"].(string); ok {
		crit.After = s
	}
	if s, ok := args["before"].(string); ok {
		crit.Before = s
	}
	if s, ok := args["search"].(string); ok {
		crit.Search = s
	}
	if b, ok := args["includeArchived"].(bool); ok {
		crit.IncludeArchived = b
	}
	if raw, ok := args["orderBy"]; ok {
		tokens := toStringSlice(raw)
		if len(tokens) > 0 {
			sortFields, bad, sok := queryschema.ParseSortWithSchema(strings.Join(tokens, ","), p.projSchema)
			if !sok {
				return crit, errf("orderBy: %q is not a sortable field", bad)
			}
			crit.Sort = sortFields
		}
	}
	return crit, nil
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

// conflictingPaginationArg returns the name of the first pagination/sort
// argument present (first / last / after / before / orderBy), or "" when none.
// These conflict with a count-only (totalCount-only) selection — there is no
// page to order or seek into when only the count is asked — so the resolver
// rejects the combination with a SchemaViolation, REST parity with
// handle_query.go's onlyTotalConflicts (sort / limit / after / before). Filter
// arguments (where / search / includeArchived) are NOT here: they bound the
// count and stay compatible, exactly as REST keeps them valid with onlyTotal.
func conflictingPaginationArg(args map[string]any) string {
	for _, k := range []string{"first", "last", "after", "before", "orderBy"} {
		if v, ok := args[k]; ok && v != nil {
			return k
		}
	}
	return ""
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

// toStringSlice coerces a GraphQL list argument into a []string.
func toStringSlice(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, 0, len(list))
		for _, e := range list {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return list
	default:
		return nil
	}
}
