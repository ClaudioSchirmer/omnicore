package queryschema

import (
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

// schemaCache memoizes ExtractRequestSchema by reflect.Type. The first
// consumer construction pays the inspection; later calls for the same
// Request DTO reuse the schema.
var schemaCache sync.Map // map[reflect.Type]*RequestSchema

// ExtractRequestSchema inspects a Request DTO's struct tags to produce its
// schema. Three field kinds per level:
//
//   - leaf filter — `query:"X" filter:"ops..."` declares wire key X (prefixed
//     by parent embed when nested) and the operator allowlist. The leaf maps
//     to the Go field path; the reader translates it to the column via the
//     view's TableSchema (no `view:` tag).
//   - reserved control — `query:"limit"` (etc.) with no `filter:` tag. Only
//     honored at the TOP LEVEL; reserved keys inside an embed group are
//     ignored at runtime (the framework's reserved set is endpoint-wide,
//     not per-embed).
//   - embed group — `query:"prefix"` on a struct-typed field with no
//     `filter:` tag. Recurses into the inner type, prefixing both the wire
//     keys and the Go field paths with the group segment.
//
// Pointer-to-struct is supported transparently — both pointer and value
// nested groups recurse the underlying struct type.
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
	walkSchemaLevel(t, "", "", s, true)
	schemaCache.Store(t, s)
	return s
}

// walkSchemaLevel recurses through a Request DTO type, accumulating leaf
// filters into s.Filters keyed by the dotted wire path. wirePrefix and
// docPrefix carry the path built so far; topLevel gates the recognition of
// reserved pagination keys (only honored at depth 0).
func walkSchemaLevel(t reflect.Type, wirePrefix, docPrefix string, s *RequestSchema, topLevel bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
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

		ftag := f.Tag.Get("filter")
		if ftag != "" {
			ops := map[string]bool{}
			for _, op := range strings.Split(ftag, ",") {
				ops[strings.TrimSpace(op)] = true
			}
			// Capture the leaf's base kind for type-driven value coercion.
			// Pointer indirection is collapsed — `*string` leaves coerce
			// identically to `string`. Composite kinds (slices, structs)
			// fall back to string at the coercion site.
			leafType := f.Type
			for leafType.Kind() == reflect.Pointer {
				leafType = leafType.Elem()
			}
			s.Filters[wirePath] = FilterSpec{Ops: ops, DocPath: goPath, GoKind: leafType.Kind()}
			continue
		}

		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			// Embed group — recurse with the Go-path prefix extended.
			walkSchemaLevel(ft, wirePath, goPath, s, false)
			continue
		}

		// Reserved pagination/control keys are recognized only at the top
		// level. An embed group's "limit" (or similar) is silently ignored
		// — only the endpoint-wide reserved set is honored.
		if topLevel {
			s.Reserved[qkey] = true
		}
	}
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
