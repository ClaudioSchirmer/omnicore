package queryschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// ProjectionSchema is the cached reflection result for a Response DTO:
// every accepted dotted wire path → its corresponding Go field path. Used by
// QueryWithParams to validate `?fields=` / `?sort=` tokens against the
// declared Response shape and translate them into the Go field vocabulary
// (the struct field-name path; nested struct + slice-of-struct paths walked
// segment-by-segment). The MongoViewReader then maps the Go path → physical
// column via the view's TableSchema — there is no `view:` tag or snake
// convention at this layer.
//
// Built once per reflect.Type at consumer construction time (sync.Map
// cache). The boot guard ValidateFieldsResponse runs in the same place,
// so a Response DTO that opted into `?fields=` is guaranteed to satisfy
// the all-pointer-with-omitempty rule before any request lands.
type ProjectionSchema struct {
	// Paths maps wire path → Go field path. The reader translates the Go path
	// to the physical Mongo column (top-level "ID" → the PK column, etc.) using
	// the view's TableSchema.
	Paths map[string]string
}

var projectionSchemaCache sync.Map // reflect.Type → *ProjectionSchema

// ExtractProjectionSchema returns (and memoizes) the projection schema for
// the Response DTO type t. Pointer types are dereferenced; non-struct
// types yield a schema with no paths (which causes every `?fields=` token
// to surface as a 400 — the boot guard already rejects the case at
// construction, so the runtime branch is defensive).
func ExtractProjectionSchema(t reflect.Type) *ProjectionSchema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if v, ok := projectionSchemaCache.Load(t); ok {
		return v.(*ProjectionSchema)
	}
	s := &ProjectionSchema{Paths: map[string]string{}}
	if t.Kind() == reflect.Struct {
		walkProjectionLevel(t, "", "", s)
	}
	projectionSchemaCache.Store(t, s)
	return s
}

// walkProjectionLevel recurses through t, accumulating dotted wire paths
// into s.Paths keyed by the wire name and valued by the doc key.
// wirePrefix/docPrefix carry the path built so far.
func walkProjectionLevel(t reflect.Type, wirePrefix, docPrefix string, s *ProjectionSchema) {
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
		// The schema maps the wire path to the Go field path (struct field name
		// path) — the criteria vocabulary. The reader translates Go→column via
		// the view's TableSchema; there is no `view:` tag or snake convention.
		docPath := joinPath(docPrefix, f.Name)
		s.Paths[wirePath] = docPath
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

// ParseProjection turns a comma-separated wire value into a projection map
// keyed by Go field path (value=1 for inclusion); the reader translates each
// Go path to the physical Mongo column via the view's TableSchema. When
// projSchema is non-nil, each token is validated against the Response DTO's
// declared wire paths and translated to the corresponding Go field path
// (nested paths walked segment-by-segment). An unknown token returns
// (nil, nil, token, false). When projSchema is nil (manual handlers via
// ParseCriteria), tokens become inclusion entries verbatim — legacy
// pass-through.
//
// wireSet returns which wire names appeared in the input; the caller uses
// it to drive the top-level `id` auto-exclusion (the framework adds
// `_id: 0` when `id` is absent from the wire set).
func ParseProjection(s string, projSchema *ProjectionSchema) (proj map[string]int, wireSet map[string]bool, badToken string, ok bool) {
	if s == "" {
		return nil, nil, "", true
	}
	tokens := strings.Split(s, ",")
	proj = make(map[string]int, len(tokens))
	wireSet = make(map[string]bool, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if projSchema == nil {
			proj[t] = 1
			wireSet[t] = true
			continue
		}
		docPath, allowed := projSchema.Paths[t]
		if !allowed {
			return nil, nil, t, false
		}
		proj[docPath] = 1
		wireSet[t] = true
	}
	return proj, wireSet, "", true
}

// ParseSortWithSchema turns a comma-separated wire value into a list of
// SortField entries. Each token may carry a `-` prefix (descending);
// otherwise ascending. When projSchema is non-nil, the wire name (without
// the prefix) is validated against the Response DTO's declared paths and
// translated to the corresponding Go field path (nested paths walked
// segment-by-segment); the reader maps Go → column via the view's TableSchema.
// An unknown token returns the verbatim wire token (including any `-` prefix) so the
// caller can surface it on the canonical 400 envelope as `sort[<token>]`.
// When projSchema is nil — manual handlers via ParseCriteria, or wrappers
// paired with a RawDoc-style projector that carries no typed Response —
// tokens become SortField entries verbatim (no allowlist, no translation).
func ParseSortWithSchema(s string, projSchema *ProjectionSchema) (sortFields []queries.SortField, badToken string, ok bool) {
	if s == "" {
		return nil, "", true
	}
	tokens := strings.Split(s, ",")
	sortFields = make([]queries.SortField, 0, len(tokens))
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		desc := false
		wireName := t
		if strings.HasPrefix(t, "-") {
			desc = true
			wireName = t[1:]
		}
		if projSchema == nil {
			sortFields = append(sortFields, queries.SortField{Field: wireName, Desc: desc})
			continue
		}
		docPath, allowed := projSchema.Paths[wireName]
		if !allowed {
			return nil, t, false
		}
		sortFields = append(sortFields, queries.SortField{Field: docPath, Desc: desc})
	}
	return sortFields, "", true
}

// ValidateFieldsResponse walks t recursively and reports every Response
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
// FormatFieldsResponseGuard to assemble the boot panic.
func ValidateFieldsResponse(t reflect.Type) []string {
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

// FormatFieldsResponseGuard builds the boot-panic diagnostic emitted when
// a Response DTO opts into `?fields=` but does not satisfy the all-
// pointer-with-omitempty rule. The framework's posture for structural
// contract violations is boot panic — fail loud at construction so the
// shape is impossible to ship.
func FormatFieldsResponseGuard(t reflect.Type, errs []string) string {
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
