package graphql

import (
	"encoding/json"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// introspectionResolvers returns the `__schema` / `__type` root resolvers that
// answer GraphQL introspection over the generated schema, so tooling (the
// playground, client codegen) can discover types. Registered only when the
// registry has introspection enabled; otherwise a `__schema` query falls
// through to the "no resolver" path and introspection is effectively off.
//
// The data is the full introspection tree built once from the *ast.Schema; the
// executor's selection trim picks exactly the fields a tool asked for, so the
// fixed-depth GraphiQL introspection query resolves without per-field wiring.
func (r *Registry) introspectionResolvers() map[string]resolver {
	schemaData := buildIntrospectionSchema(r.schema)
	typesByName := map[string]map[string]any{}
	for _, t := range schemaData["types"].([]any) {
		tm := t.(map[string]any)
		if name, _ := tm["name"].(string); name != "" {
			typesByName[name] = tm
		}
	}
	return map[string]resolver{
		"__schema": func(_ *configuration.AppContext, _ map[string]any, _ ast.SelectionSet, _ ast.FragmentDefinitionList) (any, []GraphQLError) {
			return schemaData, nil
		},
		"__type": func(_ *configuration.AppContext, args map[string]any, _ ast.SelectionSet, _ ast.FragmentDefinitionList) (any, []GraphQLError) {
			name, _ := args["name"].(string)
			if t, ok := typesByName[name]; ok {
				return t, nil
			}
			return nil, nil
		},
	}
}

// buildIntrospectionSchema renders the __Schema introspection object from the
// parsed schema: every type as a full __Type, plus the query/mutation root
// references. Directives are emitted as an empty list (the generated schema
// declares none beyond the built-ins, which tooling tolerates).
func buildIntrospectionSchema(schema *ast.Schema) map[string]any {
	types := make([]any, 0, len(schema.Types))
	for _, def := range schema.Types {
		types = append(types, fullType(def, schema))
	}
	out := map[string]any{
		"queryType":        rootTypeRef(schema.Query),
		"mutationType":     rootTypeRef(schema.Mutation),
		"subscriptionType": rootTypeRef(schema.Subscription),
		"types":            types,
		"directives":       []any{},
	}
	return out
}

func rootTypeRef(def *ast.Definition) any {
	if def == nil {
		return nil
	}
	return map[string]any{"kind": "OBJECT", "name": def.Name, "ofType": nil}
}

// fullType renders one __Type. fields/inputFields/interfaces/enumValues/
// possibleTypes follow the GraphQL spec's per-kind nullability (fields only on
// OBJECT/INTERFACE, inputFields only on INPUT_OBJECT, interfaces [] on OBJECT,
// the rest null for the kinds the generated schema produces).
func fullType(def *ast.Definition, schema *ast.Schema) map[string]any {
	t := map[string]any{
		"kind":          introspectionKind(def.Kind),
		"name":          def.Name,
		"description":   emptyToNil(def.Description),
		"fields":        nil,
		"inputFields":   nil,
		"interfaces":    nil,
		"enumValues":    nil,
		"possibleTypes": nil,
	}
	switch def.Kind {
	case ast.Object, ast.Interface:
		fields := make([]any, 0, len(def.Fields))
		for _, f := range def.Fields {
			// Introspection meta-fields (`__schema`, `__type`, `__typename`) are
			// injected onto the Query definition's Fields by gqlparser, but the
			// GraphQL spec says a type's `fields` list must NOT contain them —
			// and GraphiQL's client-side schema validation hard-rejects a schema
			// that declares a `__`-prefixed field name. Skip them here.
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			fields = append(fields, fieldEntry(f, schema))
		}
		t["fields"] = fields
		t["interfaces"] = []any{}
	case ast.InputObject:
		inputs := make([]any, 0, len(def.Fields))
		for _, f := range def.Fields {
			inputs = append(inputs, inputValue(f.Name, f.Type, f.DefaultValue, schema))
		}
		t["inputFields"] = inputs
	case ast.Enum:
		vals := make([]any, 0, len(def.EnumValues))
		for _, v := range def.EnumValues {
			vals = append(vals, map[string]any{
				"name": v.Name, "description": emptyToNil(v.Description),
				"isDeprecated": false, "deprecationReason": nil,
			})
		}
		t["enumValues"] = vals
	}
	return t
}

func fieldEntry(f *ast.FieldDefinition, schema *ast.Schema) map[string]any {
	args := make([]any, 0, len(f.Arguments))
	for _, a := range f.Arguments {
		args = append(args, inputValue(a.Name, a.Type, a.DefaultValue, schema))
	}
	return map[string]any{
		"name":              f.Name,
		"description":       emptyToNil(f.Description),
		"args":              args,
		"type":              typeRef(f.Type, schema),
		"isDeprecated":      false,
		"deprecationReason": nil,
	}
}

// inputValue renders one __InputValue. defaultValue is the spec's
// GraphQL-literal string rendering of the declared default (e.g. `ASC` for an
// enum value, `"x"` for a string), or null when the input declares none — the
// shape GraphiQL and codegen read to surface defaults.
func inputValue(name string, t *ast.Type, def *ast.Value, schema *ast.Schema) map[string]any {
	var defaultValue any
	if def != nil {
		defaultValue = def.String()
	}
	return map[string]any{
		"name":         name,
		"description":  nil,
		"type":         typeRef(t, schema),
		"defaultValue": defaultValue,
	}
}

// typeRef renders a __Type reference, unwrapping NON_NULL and LIST wrappers
// into the nested ofType chain the introspection query walks.
func typeRef(t *ast.Type, schema *ast.Schema) map[string]any {
	if t == nil {
		return nil
	}
	if t.NonNull {
		inner := *t
		inner.NonNull = false
		return map[string]any{"kind": "NON_NULL", "name": nil, "ofType": typeRef(&inner, schema)}
	}
	if t.Elem != nil {
		return map[string]any{"kind": "LIST", "name": nil, "ofType": typeRef(t.Elem, schema)}
	}
	return map[string]any{"kind": namedKind(t.NamedType, schema), "name": t.NamedType, "ofType": nil}
}

func namedKind(name string, schema *ast.Schema) string {
	if def, ok := schema.Types[name]; ok {
		return introspectionKind(def.Kind)
	}
	return "SCALAR"
}

func introspectionKind(k ast.DefinitionKind) string {
	switch k {
	case ast.Scalar:
		return "SCALAR"
	case ast.Object:
		return "OBJECT"
	case ast.Interface:
		return "INTERFACE"
	case ast.Union:
		return "UNION"
	case ast.Enum:
		return "ENUM"
	case ast.InputObject:
		return "INPUT_OBJECT"
	default:
		return "SCALAR"
	}
}

// ── The public-introspection gate ───────────────────────────────────────────

// introspectionRootFields is the exact set of root fields an introspection-only
// document may select. `__typename` is included because tooling appends it to
// the introspection query; every other `__`-prefixed name is a field of the
// introspection TYPES (reached under one of these roots), never a root of its
// own.
var introspectionRootFields = map[string]bool{
	"__schema":   true,
	"__type":     true,
	"__typename": true,
}

// IsIntrospectionOnlyRequest reports whether a raw GraphQL POST body carries a
// document that can ONLY describe the schema — never read or write data. It is
// the GraphQL analogue of "this request is just fetching /openapi.json", and
// bootstrap uses it to let the schema through the auth middleware when
// graphql.introspection is on, so the playground reaches parity with Swagger UI
// (whose spec document is public by the same reasoning).
//
// The predicate is deliberately unforgiving — anything it cannot prove is
// introspection-only is NOT (false), so a document that mixes `__schema` with a
// real field, spreads a fragment at the root, mutates, or fails to parse stays
// behind the bearer. It never touches the schema or the resolvers: a document
// that passes here still goes through the executor's own parse + validation.
func IsIntrospectionOnlyRequest(body []byte) bool {
	var req gqlRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}
	return isIntrospectionOnlyQuery(req.Query)
}

// isIntrospectionOnlyQuery applies the rule to the query document itself: EVERY
// operation must be a query whose root selections are all plain introspection
// fields. Judging every operation (not just the one operationName selects)
// keeps the decision independent of the operationName the caller sends — the
// bypass cannot be smuggled past by naming a decoy operation.
func isIntrospectionOnlyQuery(query string) bool {
	if strings.TrimSpace(query) == "" {
		return false
	}
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil || doc == nil || len(doc.Operations) == 0 {
		return false
	}
	for _, op := range doc.Operations {
		if op.Operation != ast.Query || len(op.SelectionSet) == 0 {
			return false
		}
		for _, sel := range op.SelectionSet {
			// Only a plain field qualifies. A fragment spread or inline
			// fragment at the root is refused rather than followed: the
			// gate answers a security question, and "cheap and obviously
			// correct" beats "clever" here.
			fld, ok := sel.(*ast.Field)
			if !ok || !introspectionRootFields[fld.Name] {
				return false
			}
		}
	}
	return true
}
