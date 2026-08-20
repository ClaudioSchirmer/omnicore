package openapi

import (
	"reflect"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// canonicalParameters extracts the parameter list for a canonical
// operation. Three sources flow in:
//
//  1. path:"X" struct tags on the Request DTO → path parameters.
//  2. query:"X" filter:"ops" struct tags on the Request DTO →
//     one query parameter per declared filter operator, plus the
//     reserved control keys (first/last/after/before/orderBy/fields/
//     search/includeArchived/onlyTotal) the DTO declares.
//  3. URL path segments not covered by (1) — stub `{type: string}`
//     entries are emitted so a `:id` segment auto-bound by the
//     wrapper (HasPathID) still appears in the rendered spec.
func canonicalParameters(op Operation, gen *Generator) []map[string]any {
	out := []map[string]any{}
	covered := map[string]bool{} // path-param names already emitted

	if op.Spec.RequestType != nil {
		t := op.Spec.RequestType
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() == reflect.Struct {
			for _, p := range walkPathTags(t, gen) {
				out = append(out, p)
				covered[p["name"].(string)] = true
			}
			params := omitQueryParams(walkQueryTags(t, gen), op.Spec.OmittedQueryParams)
			describeFieldsParam(params, op.Spec.ResponseType)
			describeOrderByParam(params, t)
			out = append(out, params...)
		}
	}

	for _, name := range pathSegmentNames(op.Path) {
		if covered[name] {
			continue
		}
		out = append(out, map[string]any{
			"in":       "path",
			"name":     name,
			"required": true,
			"schema":   map[string]any{"type": "string"},
		})
	}
	return out
}

// omitQueryParams drops the query parameter objects whose name appears in
// the omit list (RouteSpec.OmittedQueryParams). Used when a route reuses a
// richer Request DTO but honors only a subset of its query keys — e.g. the
// tabular-export routes reuse the JSON list's DTO yet ignore pagination, so
// first/last/after/before/onlyTotal are removed from the rendered spec.
// Returns the input untouched when the omit list is empty (the common case).
func omitQueryParams(params []map[string]any, omit []string) []map[string]any {
	if len(omit) == 0 {
		return params
	}
	skip := make(map[string]bool, len(omit))
	for _, n := range omit {
		skip[n] = true
	}
	kept := make([]map[string]any, 0, len(params))
	for _, p := range params {
		if name, _ := p["name"].(string); skip[name] {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// rawParameters maps the MountRaw-declared []Parameter to OpenAPI
// parameter objects. Path parameters are forced Required at MountRaw
// time, so the flag here is honored verbatim.
func rawParameters(op Operation, gen *Generator) []map[string]any {
	out := make([]map[string]any, 0, len(op.Raw.Parameters))
	for _, p := range op.Raw.Parameters {
		entry := map[string]any{
			"in":   string(p.In),
			"name": p.Name,
		}
		if p.Description != "" {
			entry["description"] = p.Description
		}
		if p.Required {
			entry["required"] = true
		}
		entry["schema"] = paramSchema(p.Type, gen)
		out = append(out, entry)
	}
	return out
}

// paramSchema returns the OpenAPI schema for a Parameter. nil Type
// defaults to `{type: string}` per the §3 convenience contract
// documented on Parameter.Type; otherwise the schema generator owns
// the conversion (primitives, pointers→nullable, structs→$ref).
func paramSchema(t reflect.Type, gen *Generator) any {
	if t == nil {
		return map[string]any{"type": "string"}
	}
	return gen.Generate(t)
}

// walkPathTags iterates the Request DTO's fields and emits one path
// parameter object per `path:"name"` tag found. Field type drives the
// schema; missing tag value falls back to the Go field name.
func walkPathTags(t reflect.Type, gen *Generator) []map[string]any {
	out := []map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Tag.Get("path")
		if name == "" {
			continue
		}
		entry := map[string]any{
			"in":       "path",
			"name":     name,
			"required": true,
			"schema":   gen.Generate(f.Type),
		}
		if desc := f.Tag.Get("description"); desc != "" {
			entry["description"] = desc
		}
		out = append(out, entry)
	}
	return out
}

// walkQueryTags emits query parameter objects from a Request DTO's
// `query:"X"` (+ optional `filter:"ops"`) tags. It consumes the shared
// queryschema.WalkRequest traversal — the single source of the tag-walk
// rules (embed-group recursion, the eq-has-no-suffix operator convention,
// dotted wire paths) the runtime allowlist also folds — so the parameter set
// the spec advertises cannot drift from the keys the wrapper accepts.
//
// A field with `query:"q"` and no `filter:` tag and a scalar type is a
// reserved pagination/control key — emits ONE query parameter named "q".
// A field with `query:"name" filter:"eq,in,gte"` emits THREE query
// parameters: "name", "name.in", "name.gte". A struct-typed field with
// `query:"addresses"` and no filter tag is an embed group — WalkRequest
// recurses and each leaf below it appears with the prefix as a dotted
// parameter name (e.g. `addresses.city`, `addresses.city.istartswith`).
func walkQueryTags(t reflect.Type, gen *Generator) []map[string]any {
	out := []map[string]any{}
	for _, leaf := range queryschema.WalkRequest(t) {
		// Generate the field schema for every query-tagged field, including
		// embed-group markers, so the type graph each field references is
		// registered in Components exactly as before — even though a group
		// emits no parameter of its own.
		schema := gen.Generate(leaf.Field.Type)
		if leaf.Group {
			continue
		}
		if len(leaf.Ops) == 0 {
			// A VOCABULARY leaf (`query:"id" sort:"asc"`, no filter tag)
			// carries no value on the wire: it names a path `?orderBy=` may
			// order by, and the request parser rejects `?id=` like any
			// undeclared key. Emitting it would advertise a parameter that can
			// only ever answer 400 — the exact dead-parameter shape
			// ExtractRequestSchema panics to prevent. The reserved controls are
			// the ones that DO take a value.
			if leaf.Sort != nil && !queryschema.ControlKeys[leaf.Field.Tag.Get("query")] {
				continue
			}
			out = append(out, queryEntry(leaf.WirePath, schema, leaf.Field))
			continue
		}
		for _, op := range leaf.Ops {
			name := leaf.WirePath
			if op != queryschema.OpEq {
				name = leaf.WirePath + "." + op
			}
			out = append(out, queryEntry(name, schema, leaf.Field))
		}
	}
	return out
}

// queryEntry assembles one OpenAPI query parameter map. Required is
// emitted only when the underlying field is non-pointer (mirrors the
// "lenient" required-set rule of the schema generator) so the spec
// matches the wrapper's runtime behavior: pointer-typed query fields
// are optional, non-pointer aren't.
func queryEntry(name string, schema *Schema, f reflect.StructField) map[string]any {
	entry := map[string]any{
		"in":     "query",
		"name":   name,
		"schema": schema,
	}
	if desc := f.Tag.Get("description"); desc != "" {
		entry["description"] = desc
	}
	if f.Type.Kind() != reflect.Pointer {
		entry["required"] = true
	}
	return entry
}

// hasBodyFields returns true when t has at least one field that the
// schema generator would emit as a body property — i.e. an exported,
// non-anonymous, non-path/query/json-skip field. Used to decide
// whether the canonical operation emits a requestBody block at all.
func hasBodyFields(t reflect.Type) bool {
	if t == nil {
		return false
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && hasBodyFields(ft) {
				return true
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if f.Tag.Get("path") != "" || f.Tag.Get("query") != "" {
			continue
		}
		if f.Tag.Get("json") == "-" {
			continue
		}
		return true
	}
	return false
}

// describeFieldsParam fills in the `?fields=` parameter's description when the
// DTO field did not declare one of its own.
//
// It states the RULE rather than the vocabulary. The `?fields=` tokens are the
// whole Response tree — on a real DTO that runs to dozens of paths plus every
// nested level below them, and enumerating it would bury the parameter instead
// of documenting it. (`?orderBy=` enumerates precisely because its vocabulary
// is a short, deliberate declaration.)
//
// What the rule alone cannot convey is the spelling of a nested token, so when
// the Response has one the sentence carries a REAL example lifted from that
// Response — the endpoint's own syntax, not an invented one. A flat Response
// gets no example, because there is no second spelling to show.
func describeFieldsParam(params []map[string]any, respType reflect.Type) {
	entry := paramNamed(params, queryschema.KeyFields)
	if entry == nil {
		return
	}
	if _, declared := entry["description"]; declared {
		return
	}
	desc := "Comma-separated subset of the response to return, naming fields by their wire (json) names. " +
		"Any field the response declares is a legal token; anything else is refused with 400."
	if root, nested, ok := fieldsExample(respType); ok {
		desc += " A nested field takes the dotted path through its parent, e.g. `" + root + "," + nested + "`."
	}
	entry["description"] = desc
}

// fieldsExample picks a real (root, nested) token pair off the Response so the
// description can show the dotted spelling on this endpoint's own shape.
// Deterministic: the alphabetically first nested path, and the root it hangs
// from. Reports false for a flat or untyped Response.
func fieldsExample(respType reflect.Type) (root, nested string, ok bool) {
	if respType == nil {
		return "", "", false
	}
	for respType.Kind() == reflect.Pointer {
		respType = respType.Elem()
	}
	if respType.Kind() != reflect.Struct {
		return "", "", false
	}
	paths := queryschema.ExtractProjectionSchema(respType).Paths
	nesteds := make([]string, 0, len(paths))
	for p := range paths {
		if strings.Contains(p, ".") {
			nesteds = append(nesteds, p)
		}
	}
	if len(nesteds) == 0 {
		return "", "", false
	}
	sort.Strings(nesteds)
	nested = nesteds[0]
	return nested[:strings.Index(nested, ".")], nested, true
}

// describeOrderByParam fills in the `?orderBy=` parameter's description when the
// DTO field did not declare one of its own.
//
// Unlike `?fields=`, this one ENUMERATES: the ordering vocabulary is a short,
// deliberate declaration — the leaves that carry a `sort:` tag — so listing the
// accepted tokens with the directions each admits states the whole contract in
// one line, and those are the only values the parameter ever takes.
func describeOrderByParam(params []map[string]any, reqType reflect.Type) {
	entry := paramNamed(params, queryschema.KeyOrderBy)
	if entry == nil {
		return
	}
	if _, declared := entry["description"]; declared {
		return
	}
	sortable := queryschema.ExtractRequestSchema(reqType).Sortable
	if len(sortable) == 0 {
		return
	}
	wires := make([]string, 0, len(sortable))
	for wire := range sortable {
		wires = append(wires, wire)
	}
	sort.Strings(wires)
	tokens := make([]string, 0, len(wires))
	for _, wire := range wires {
		spec := sortable[wire]
		switch {
		case spec.Asc && spec.Desc:
			tokens = append(tokens, "`"+wire+"` (asc, desc)")
		case spec.Desc:
			tokens = append(tokens, "`"+wire+"` (desc only, prefix with `-`)")
		default:
			tokens = append(tokens, "`"+wire+"` (asc only)")
		}
	}
	entry["description"] = "Comma-separated ordering, applied in the order given. Prefix a token with `-` for descending. Accepted: " +
		strings.Join(tokens, ", ") + "."
}

// paramNamed finds the emitted parameter carrying name, or nil.
func paramNamed(params []map[string]any, name string) map[string]any {
	for _, p := range params {
		if p["name"] == name {
			return p
		}
	}
	return nil
}
