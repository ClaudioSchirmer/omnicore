package queryschema

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// RequestSchema is the cached reflection result for a Request DTO type:
// every accepted wire key (flat or dotted) → its FilterSpec, plus the
// reserved pagination/control set (top-level only — embed groups carry
// filter leaves, not pagination keys).
type RequestSchema struct {
	Filters  map[string]FilterSpec
	Reserved map[string]bool
}

// RequestField is one query-tagged field discovered on a Request DTO, yielded
// in declaration order by WalkRequest. It is the shared traversal both the
// runtime allowlist (ExtractRequestSchema) and the OpenAPI parameter generator
// consume, so the rules that classify a field — filter leaf vs embed group vs
// reserved/scalar control, the eq-has-no-suffix operator convention, the
// embed-prefix path building — live in exactly one place.
//
// Classification:
//   - Ops != nil          → filter leaf (`query:"X" filter:"ops"`); Ops holds
//     the declared operators in tag order.
//   - Group == true       → embed group (`query:"prefix"` on a struct field
//     with no filter tag); WalkRequest descends into it and also yields its
//     inner fields. The group itself carries no value on the wire.
//   - Ops == nil && !Group → reserved/control scalar (`query:"first"` etc.);
//     honored as a reserved key only when TopLevel, and only for the
//     canonical ControlKeys vocabulary (anything else is a boot fail —
//     see ExtractRequestSchema).
type RequestField struct {
	WirePath string
	GoPath   string
	Field    reflect.StructField
	Ops      []string
	Group    bool
	TopLevel bool
}

// schemaCache memoizes ExtractRequestSchema by reflect.Type. The first
// consumer construction pays the inspection; later calls for the same
// Request DTO reuse the schema.
var schemaCache sync.Map // map[reflect.Type]*RequestSchema

// WalkRequest walks a Request DTO type and returns every query-tagged exported
// field in declaration order (descending into embed groups), classified per
// RequestField. Pointer types are dereferenced transparently. Unexported
// fields are skipped — a query parameter cannot bind to one.
func WalkRequest(t reflect.Type) []RequestField {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []RequestField
	walkRequestLevel(t, "", "", true, &out)
	return out
}

func walkRequestLevel(t reflect.Type, wirePrefix, docPrefix string, topLevel bool, out *[]RequestField) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		qkey := f.Tag.Get("query")
		if qkey == "" {
			continue
		}
		wirePath := joinPath(wirePrefix, qkey)
		// The criteria key is the Go field path (the struct field name, e.g.
		// "Email" / "Addresses.ZipCode") — NOT a physical column. The reader
		// translates it to the column via the view's TableSchema. No convention,
		// no `view:` tag.
		goPath := joinPath(docPrefix, f.Name)

		if ftag := f.Tag.Get("filter"); ftag != "" {
			*out = append(*out, RequestField{
				WirePath: wirePath, GoPath: goPath, Field: f,
				Ops: splitOps(ftag), TopLevel: topLevel,
			})
			continue
		}

		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			// Embed group — yield a marker (consumers that document the type
			// still see it) and recurse with the prefixes extended.
			*out = append(*out, RequestField{
				WirePath: wirePath, GoPath: goPath, Field: f,
				Group: true, TopLevel: topLevel,
			})
			walkRequestLevel(ft, wirePath, goPath, false, out)
			continue
		}

		// Reserved pagination/control scalar.
		*out = append(*out, RequestField{
			WirePath: wirePath, GoPath: goPath, Field: f, TopLevel: topLevel,
		})
	}
}

// splitOps splits a `filter:"eq,in"` tag into the declared operators in order,
// trimming whitespace and dropping empty tokens.
func splitOps(tag string) []string {
	parts := strings.Split(tag, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ExtractRequestSchema inspects a Request DTO's struct tags to produce its
// runtime schema, folding the shared WalkRequest traversal into the keyed
// maps the wire-parsing layer consumes:
//
//   - filter leaf → Filters[wirePath] = {ops set, Go field path, base kind}.
//     The leaf maps to the Go field path; the reader translates it to the
//     column via the view's TableSchema (no `view:` tag).
//   - reserved control scalar at the TOP LEVEL → Reserved[queryKey]. Reserved
//     keys inside an embed group are ignored (the framework's reserved set is
//     endpoint-wide, not per-embed).
//   - embed group → no entry of its own (its inner leaves carry the wire keys).
//
// Pointer-to-struct is supported transparently. Cached by reflect.Type.
func ExtractRequestSchema(t reflect.Type) *RequestSchema {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v, ok := schemaCache.Load(t); ok {
		return v.(*RequestSchema)
	}
	s := &RequestSchema{
		Filters:  map[string]FilterSpec{},
		Reserved: map[string]bool{},
	}
	for _, leaf := range WalkRequest(t) {
		switch {
		case leaf.Group:
			// Marker only — inner leaves carry the keys.
		case leaf.Ops != nil:
			ops := map[string]bool{}
			for _, op := range leaf.Ops {
				ops[op] = true
			}
			// Capture the leaf's base kind for type-driven value coercion.
			// Pointer indirection is collapsed — `*string` leaves coerce
			// identically to `string`. Composite kinds (slices, structs)
			// fall back to string at the coercion site.
			leafType := leaf.Field.Type
			for leafType.Kind() == reflect.Pointer {
				leafType = leafType.Elem()
			}
			s.Filters[leaf.WirePath] = FilterSpec{Ops: ops, DocPath: leaf.GoPath, GoKind: leafType.Kind()}
		case leaf.TopLevel:
			key := leaf.Field.Tag.Get("query")
			// The control vocabulary is CLOSED. A non-canonical key here is
			// always a mistake (typo, or a stale spelling from another
			// framework's vocabulary): it would opt nothing in — every wire
			// use keeps rejecting — while the OpenAPI generator advertised
			// the dead parameter. Fail loud at construction, like the
			// fields-response guard.
			if !ControlKeys[key] {
				panic(fmt.Sprintf(
					"queryschema: %s.%s declares query:%q, which is not a reserved control key — a top-level query-tagged scalar without a filter:\"…\" tag must be one of the canonical controls (%s); for a filter leaf, add its filter:\"…\" operator tag",
					t.String(), leaf.Field.Name, key, strings.Join(controlKeyList, ", ")))
			}
			s.Reserved[key] = true
		}
	}
	schemaCache.Store(t, s)
	return s
}

// ReadIncludeArchived reports the value bound to the Request DTO field tagged
// `query:"includeArchived"` — the one reserved control a by-id read accepts.
// Both `bool` and `*bool` are honored (a nil pointer reads as false) and
// promoted anonymous structs are walked, so the reader sees exactly what the
// surface's binder wrote. Returns false when the DTO declares no such field.
//
// The by-id wrappers call it to build the wire ReadCriteria they hand to
// ToQuery(criteria) — the same seat the paged wrappers feed from
// buildCriteria, whose control vocabulary is the full set.
func ReadIncludeArchived(v reflect.Value) bool {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return false
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			if ReadIncludeArchived(v.Field(i)) {
				return true
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		tag, _, _ := strings.Cut(f.Tag.Get("query"), ",")
		if tag != KeyIncludeArchived {
			continue
		}
		fv := v.Field(i)
		if fv.Kind() == reflect.Pointer {
			if fv.IsNil() {
				return false
			}
			fv = fv.Elem()
		}
		if fv.Kind() == reflect.Bool {
			return fv.Bool()
		}
		return false
	}
	return false
}

// joinPath concatenates two non-empty segments with a single dot, returning
// either one verbatim when the other is empty.
func joinPath(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	if segment == "" {
		return prefix
	}
	return prefix + "." + segment
}
