package graphql

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
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
