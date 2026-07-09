package graphql

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
)

// Execute runs one GraphQL request against the registry: gqlparser parses +
// validates the query against the generated schema, then the framework walks
// the operation — resolving each root field through its registered handler and
// trimming the returned value tree to the selection set. Parse/validation
// faults and per-field resolver errors both surface in Response.Errors;
// successful fields populate Response.Data. operationName selects among
// multiple operations (empty → the first).
func (r *Registry) Execute(ctx *configuration.AppContext, query string, vars map[string]any, operationName string) Response {
	if err := r.build(); err != nil {
		return Response{Errors: []GraphQLError{{Message: "schema build failed: " + err.Error()}}}
	}
	doc, errs := gqlparser.LoadQueryWithRules(r.schema, query, nil)
	if errs != nil {
		return Response{Errors: fromGqlErrors(errs)}
	}
	op := doc.Operations.ForName(operationName)
	if op == nil && operationName == "" && len(doc.Operations) > 0 {
		op = doc.Operations[0]
	}
	if op == nil {
		return Response{Errors: []GraphQLError{{Message: "no operation to execute"}}}
	}

	data := map[string]any{}
	var outErrs []GraphQLError
	for _, fld := range collectFields(op.SelectionSet, doc.Fragments) {
		key := responseKey(fld)
		res, ok := r.resolvers[fld.Name]
		if !ok {
			outErrs = append(outErrs, GraphQLError{Message: "no resolver for field " + fld.Name, Path: []any{key}})
			data[key] = nil
			continue
		}
		val, gerrs := res(ctx, fld.ArgumentMap(vars), fld.SelectionSet, doc.Fragments)
		if len(gerrs) > 0 {
			for i := range gerrs {
				if gerrs[i].Path == nil {
					gerrs[i].Path = []any{key}
				}
			}
			outErrs = append(outErrs, gerrs...)
			data[key] = nil
			continue
		}
		data[key] = applySelection(val, fld.SelectionSet, doc.Fragments)
	}
	return Response{Data: data, Errors: outErrs}
}

// applySelection trims an already-wire-shaped value tree to the requested
// selection set. Scalars (empty selection) pass through; objects pick the
// requested fields (honoring aliases); lists recurse element-wise. Fragments
// are flattened via collectFields.
func applySelection(value any, sel ast.SelectionSet, frags ast.FragmentDefinitionList) any {
	fields := collectFields(sel, frags)
	if len(fields) == 0 {
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(fields))
		for _, f := range fields {
			out[responseKey(f)] = applySelection(v[f.Name], f.SelectionSet, frags)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = applySelection(e, sel, frags)
		}
		return out
	default:
		return value
	}
}

// collectFields flattens a selection set into its concrete fields, resolving
// inline fragments and named fragment spreads. Type conditions are not
// enforced — the framework's objects are concrete (no unions/interfaces in the
// generated schema), so every fragment's fields apply.
func collectFields(sel ast.SelectionSet, frags ast.FragmentDefinitionList) []*ast.Field {
	var out []*ast.Field
	for _, s := range sel {
		switch v := s.(type) {
		case *ast.Field:
			out = append(out, v)
		case *ast.InlineFragment:
			out = append(out, collectFields(v.SelectionSet, frags)...)
		case *ast.FragmentSpread:
			if def := frags.ForName(v.Name); def != nil {
				out = append(out, collectFields(def.SelectionSet, frags)...)
			}
		}
	}
	return out
}

// responseKey is the output key for a field — its alias when set, else its name.
func responseKey(f *ast.Field) string {
	if f.Alias != "" {
		return f.Alias
	}
	return f.Name
}
