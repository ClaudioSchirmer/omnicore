package openapi

import (
	"reflect"
	"strings"
)

// canonicalParameters extracts the parameter list for a canonical
// operation. Three sources flow in:
//
//  1. path:"X" struct tags on the Request DTO → path parameters.
//  2. query:"X" filter:"ops" struct tags on the Request DTO →
//     one query parameter per declared filter operator, plus the
//     reserved pagination keys (limit/after/before/sort/fields/
//     search/includeArchived) when present.
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
			out = append(out, omitQueryParams(walkQueryTags(t, gen), op.Spec.OmittedQueryParams)...)
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
// limit/after/before/onlyTotal are removed from the rendered spec. Returns the
// input untouched when the omit list is empty (the common case).
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

// walkQueryTags iterates the Request DTO's fields and emits query
// parameter objects from `query:"X"` (+ optional `filter:"ops"`) tags.
// Recurses into nested struct fields carrying `query:"prefix"` (with no
// `filter:` tag) — each leaf below them appears with the prefix as a
// dotted parameter name (e.g. `addresses.city`, `addresses.city.istartswith`),
// matching the wire shape extractAllowedKeys accepts at runtime.
//
// A field with `query:"q"` and no `filter:` tag and a scalar type is a
// reserved pagination/control key — emits ONE query parameter named "q".
// A field with `query:"name" filter:"eq,in,gte"` emits THREE query
// parameters: "name", "name.in", "name.gte". A struct-typed field with
// `query:"addresses"` and no filter tag is an embed group — the walker
// recurses with the prefix attached.
func walkQueryTags(t reflect.Type, gen *Generator) []map[string]any {
	return walkQueryTagsAt(t, "", gen)
}

func walkQueryTagsAt(t reflect.Type, wirePrefix string, gen *Generator) []map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := []map[string]any{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		qkey := f.Tag.Get("query")
		if qkey == "" {
			continue
		}
		wireName := qkey
		if wirePrefix != "" {
			wireName = wirePrefix + "." + qkey
		}
		ftag := f.Tag.Get("filter")
		fieldSchema := gen.Generate(f.Type)

		if ftag == "" {
			// No filter tag — either a reserved control key (top-level
			// scalar like limit/sort) or an embed group when the field
			// type is a struct. Recurse into the struct; emit a single
			// query entry otherwise.
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, walkQueryTagsAt(ft, wireName, gen)...)
				continue
			}
			out = append(out, queryEntry(wireName, fieldSchema, f))
			continue
		}
		for _, op := range strings.Split(ftag, ",") {
			op = strings.TrimSpace(op)
			if op == "" {
				continue
			}
			name := wireName
			if op != "eq" {
				name = wireName + "." + op
			}
			out = append(out, queryEntry(name, fieldSchema, f))
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
