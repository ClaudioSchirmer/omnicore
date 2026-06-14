package web

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// projectionSchema is the cached reflection result for a Response DTO:
// every accepted dotted wire path → its corresponding doc path. Used by
// HandleQueryWithParams to validate `?fields=` tokens against the
// declared Response shape and translate them into Mongo's doc-key
// vocabulary (PascalToSnake default; `view:` override; nested struct +
// slice-of-struct paths walked segment-by-segment).
//
// Built once per reflect.Type at wrapper construction time (sync.Map
// cache). The boot guard validateFieldsResponse runs in the same place,
// so a Response DTO that opted into `?fields=` is guaranteed to satisfy
// the all-pointer-with-omitempty rule before any request lands.
type projectionSchema struct {
	// paths maps wire path → doc path. Top-level "id" maps to itself by
	// default (the framework stores the PG `id` column verbatim in Mongo
	// alongside the Upsert-injected `_id`); a Response declaring
	// `view:"_id"` would override it.
	paths map[string]string
}

var projectionSchemaCache sync.Map // reflect.Type → *projectionSchema

// extractProjectionSchema returns (and memoizes) the projection schema for
// the Response DTO type t. Pointer types are dereferenced; non-struct
// types yield a schema with no paths (which causes every `?fields=` token
// to surface as a 400 — the boot guard already rejects the case at
// construction, so the runtime branch is defensive).
func extractProjectionSchema(t reflect.Type) *projectionSchema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v, ok := projectionSchemaCache.Load(t); ok {
		return v.(*projectionSchema)
	}
	s := &projectionSchema{paths: map[string]string{}}
	if t.Kind() == reflect.Struct {
		walkProjectionLevel(t, "", "", s)
	}
	projectionSchemaCache.Store(t, s)
	return s
}

// walkProjectionLevel recurses through t, accumulating dotted wire paths
// into s.paths keyed by the wire name and valued by the doc key.
// wirePrefix/docPrefix carry the path built so far.
func walkProjectionLevel(t reflect.Type, wirePrefix, docPrefix string, s *projectionSchema) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// Anonymous struct embedded — promote its fields up to this level
		// without renaming, matching encoding/json's promotion rule.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walkProjectionLevel(ft, wirePrefix, docPrefix, s)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		wireName, skip := projectionWireName(f)
		if skip {
			continue
		}
		wirePath := joinPath(wirePrefix, wireName)
		docSeg := domain.PascalToSnake(wireName)
		if v := f.Tag.Get("view"); v != "" && v != "-" {
			docSeg = v
		}
		docPath := joinPath(docPrefix, docSeg)
		s.paths[wirePath] = docPath
		// Recurse into struct / slice-of-struct element type so nested wire
		// paths (addresses.city, addresses.lines.qty) are discoverable.
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			walkProjectionLevel(ft, wirePath, docPath, s)
		case reflect.Slice:
			elem := ft.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				walkProjectionLevel(elem, wirePath, docPath, s)
			}
		}
	}
}

// projectionWireName resolves the JSON wire name for f. Returns
// ("", true) when f carries `json:"-"` (skipped). Falls back to the Go
// field name when the json tag is absent or empty; strips the
// `,omitempty` / `,string` modifiers.
func projectionWireName(f reflect.StructField) (string, bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	if tag == "" {
		return f.Name, false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name, false
	}
	return name, false
}

// validateFieldsResponse walks t recursively and reports every Response
// field that violates the "pointer + omitempty" rule the `?fields=` path
// relies on. The rule:
//
//   - Scalar / struct fields → must be declared `*T` (pointer). A non-
//     pointer scalar renders its zero value (`"name":""`) when Mongo
//     drops it from the projection, defeating the point of `fields`.
//   - Slice fields → tolerated as `[]T` or `[]*T`. The encoding/json
//     `omitempty` modifier elides empty slices (len==0) just as it elides
//     nil pointers, so no extra wrapping is required.
//   - Every kept field → `json:"<wire>,omitempty"`. Without `omitempty`
//     the empty value still renders.
//
// Recursion enters struct fields and slice element types so nested
// shapes (addresses[].city etc.) are validated under the same rule.
//
// Returns the empty slice when the type is fully compliant; otherwise
// the slice of human-readable violation lines used by
// formatFieldsResponseGuard to assemble the boot panic.
func validateFieldsResponse(t reflect.Type) []string {
	var errs []string
	walkResponseGuard(t, "", &errs)
	return errs
}

func walkResponseGuard(t reflect.Type, path string, errs *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		// Anonymous struct promotion follows the same rule path-wise: keep
		// the parent's path so the diagnostic surfaces the wire-visible
		// position of the offending field.
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				walkResponseGuard(ft, path, errs)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		fieldPath := joinPath(path, name)

		// Rule 1 — every kept field must declare ,omitempty.
		if !hasOmitempty(tag) {
			*errs = append(*errs, fmt.Sprintf("%s: missing ,omitempty in json tag (got %q)", fieldPath, tag))
		}

		// Rule 2 — scalar / struct fields must be pointer; slices are OK as
		// `[]T` (omitempty handles nil/empty). Recurse into struct and
		// slice-of-struct element types so the rule applies at every depth.
		switch f.Type.Kind() {
		case reflect.Pointer:
			elem := f.Type.Elem()
			if elem.Kind() == reflect.Struct {
				walkResponseGuard(elem, fieldPath, errs)
			}
		case reflect.Slice:
			elem := f.Type.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}
			if elem.Kind() == reflect.Struct {
				walkResponseGuard(elem, fieldPath, errs)
			}
		case reflect.Map:
			// Maps render as `{}` when empty but omitempty elides them via
			// len==0 — accept without pointer wrapping, by analogy to
			// slices.
		default:
			*errs = append(*errs, fmt.Sprintf("%s: must be *%s with omitempty (got %s)", fieldPath, f.Type.String(), f.Type.Kind()))
		}
	}
}

func hasOmitempty(tag string) bool {
	for _, part := range strings.Split(tag, ",") {
		if strings.TrimSpace(part) == "omitempty" {
			return true
		}
	}
	return false
}

// formatFieldsResponseGuard builds the boot-panic diagnostic emitted when
// a Response DTO opts into `?fields=` but does not satisfy the all-
// pointer-with-omitempty rule. The framework's posture for structural
// contract violations is boot panic — fail loud at construction so the
// shape is impossible to ship.
func formatFieldsResponseGuard(t reflect.Type, errs []string) string {
	// Deterministic ordering so the diagnostic is stable across runs.
	sortedErrs := append([]string(nil), errs...)
	sort.Strings(sortedErrs)
	var b strings.Builder
	fmt.Fprintf(&b, "[fields] %s declares query:\"fields\" via its Request DTO but the Response shape violates the sparse-render contract:\n", t.String())
	for _, line := range sortedErrs {
		fmt.Fprintf(&b, "  - %s\n", line)
	}
	b.WriteString("Every exported field (recursively, including nested struct and slice element types) must be either *T or a slice and must carry ,omitempty in its json tag — `?fields=` reduces what Mongo returns, and encoding/json elides absent values only when the type tolerates the empty form.")
	return b.String()
}
