package queryschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ProjectionSchema is the cached reflection result for a Response DTO:
// every accepted dotted wire path → its corresponding Go field path. Used by
// QueryWithParams to validate `?fields=` / `?orderBy=` tokens against the
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
	// to the physical Mongo column (top-level "ID" → the ID column, etc.) using
	// the view's TableSchema.
	Paths map[string]string

	// Computed maps the wire path of a COMPUTED field to the Go field paths
	// that feed it — declared by the Response field's `computed:"A,B"` tag,
	// naming Result fields. A computed field has no column behind it: it is
	// produced by the Query's FromQueryResult hook from other Result fields.
	// So `?fields=<computed>` must push the SOURCES down to the store (never
	// the computed path itself, which would not resolve to a column), and
	// `?orderBy=<computed>` is refused — ordering happens in the store, and
	// the keyset cursor is built from stored values.
	//
	// The sources are OPTIONAL on the Response (a source that is not declared
	// there is read, feeds the computation, and never reaches the wire) but
	// MANDATORY on the Result — ValidateComputedSources enforces that at boot.
	Computed map[string][]string
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
	s := &ProjectionSchema{Paths: map[string]string{}, Computed: map[string][]string{}}
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
		// A `computed:"A,B"` tag declares that this field carries no column:
		// the Query's FromQueryResult derives it from the named Result fields.
		// The sources are recorded under the SAME level prefix, so a computed
		// field nested in a segment names its siblings.
		if sources := computedSources(f, docPrefix); len(sources) > 0 {
			s.Computed[wirePath] = sources
		}
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

// ComputedTag is the Response struct tag declaring that a field is derived by
// the Query's FromQueryResult hook rather than read from a column, listing the
// Result fields that feed it:
//
//	Display *string `json:"display,omitempty" computed:"Name,UserName"`
const ComputedTag = "computed"

// computedSources parses f's `computed:` tag into the Go field paths to push
// down for that field, prefixed by the level the field lives at. Returns nil
// when the tag is absent or lists nothing.
func computedSources(f reflect.StructField, docPrefix string) []string {
	raw, ok := f.Tag.Lookup(ComputedTag)
	if !ok {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, joinPath(docPrefix, p))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseProjection turns the selected wire tokens into a projection map keyed by
// Go field path (value=1 for inclusion); the reader translates each Go path to
// the physical column via the view's TableSchema. When projSchema is non-nil,
// each token is validated against the Response DTO's declared wire paths and
// translated to the corresponding Go field path (nested paths walked
// segment-by-segment). An unknown token returns (nil, nil, token, false). When
// projSchema is nil (a manual handler over an untyped Response), tokens become
// inclusion entries verbatim — the pass-through.
//
// It takes TOKENS, not a comma-separated value: the comma is how one wire
// spells a list, and the surfaces that spell it otherwise — a GraphQL
// selection, a proto FieldMask — would otherwise have to join a list just to
// have it split back.
//
// wireSet returns which wire names appeared in the input; the caller uses it to
// drive the top-level `id` auto-exclusion (the framework adds `_id: 0` when
// `id` is absent from the wire set).
func ParseProjection(tokens []string, projSchema *ProjectionSchema) (proj map[string]int, wireSet map[string]bool, badToken string, ok bool) {
	if len(tokens) == 0 {
		return nil, nil, "", true
	}
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
		// A computed field has no column: push its SOURCES down instead, so the
		// reader returns what FromQueryResult needs to derive it. Requesting
		// the computed path itself would not resolve to a column.
		if sources, isComputed := projSchema.Computed[t]; isComputed {
			for _, src := range sources {
				proj[src] = 1
			}
			wireSet[t] = true
			continue
		}
		proj[docPath] = 1
		wireSet[t] = true
	}
	return proj, wireSet, "", true
}

// Violation is a rejected read control, carrying BOTH the canonical wire
// spelling of the offending field and the notification that explains it. It
// exists so every surface — the auto wrappers, the tabular exports and the
// manual QueryParser path — renders the SAME typed, translated envelope
// instead of collapsing every rejection into a generic schema violation.
//
// A zero Notification means the canonical SchemaViolationNotification.
type Violation struct {
	// Field is the wire spelling the consumer sees, e.g. "orderBy[display]".
	Field string
	// Notification explains the refusal; nil → SchemaViolationNotification.
	Notification domain.Notification
}

// SchemaViolation builds the generic rejection for an offending wire field.
func SchemaViolation(field string) *Violation { return &Violation{Field: field} }

// Message renders the violation as the notification message a surface adds to
// its "Schema" context.
func (v Violation) Message() domain.NotificationMessage {
	n := v.Notification
	if n == nil {
		n = domain.SchemaViolationNotification{}
	}
	return domain.NotificationMessage{FieldName: v.Field, Notification: n}
}

// SelectedComputedPaths returns the Go field paths of the COMPUTED fields the
// consumer selected, given the wire set ParseProjection produced. They are
// absent from the store projection by construction (their sources went down
// instead), so any consumer that shapes output from the projection — the
// tabular export's column pruning — must be told about them separately.
// Returns nil when nothing computed was selected. Deterministic order.
func SelectedComputedPaths(projSchema *ProjectionSchema, wireSet map[string]bool) []string {
	if projSchema == nil || len(projSchema.Computed) == 0 || len(wireSet) == 0 {
		return nil
	}
	var out []string
	for wirePath := range wireSet {
		if _, isComputed := projSchema.Computed[wirePath]; !isComputed {
			continue
		}
		if goPath, ok := projSchema.Paths[wirePath]; ok {
			out = append(out, goPath)
		}
	}
	sort.Strings(out)
	return out
}

// UnrequestedComputedSources returns the Result Go paths that were read ONLY
// to feed a selected computed field and that the consumer did not ask for.
//
// A computed field's sources must reach the store, but `?fields=` shapes the
// WIRE: if a source happens to be declared on the Response too, reading it
// would otherwise render it beside the computed value even though nobody
// selected it. The wrapper blanks these paths on the Result before projecting,
// so the sparse Response elides them exactly as it would any unselected field.
// A source the Response does not declare needs no blanking — it has no wire
// slot to leak into.
//
// Returns nil when `?fields=` was not used, nothing computed was selected, or
// every source was requested outright. Deterministic order.
func UnrequestedComputedSources(projSchema *ProjectionSchema, wireSet map[string]bool) []string {
	if projSchema == nil || len(projSchema.Computed) == 0 || len(wireSet) == 0 {
		return nil
	}
	// Go path → wire path, to tell "the consumer asked for this" from "we read
	// it as a source".
	wireByGo := make(map[string]string, len(projSchema.Paths))
	for wirePath, goPath := range projSchema.Paths {
		wireByGo[goPath] = wirePath
	}
	blank := map[string]bool{}
	for wirePath := range wireSet {
		sources, isComputed := projSchema.Computed[wirePath]
		if !isComputed {
			continue
		}
		for _, src := range sources {
			srcWire, onTheWire := wireByGo[src]
			if !onTheWire {
				continue // read-only source: no wire slot, nothing to hide
			}
			if wireSet[srcWire] {
				continue // the consumer asked for it too
			}
			blank[src] = true
		}
	}
	if len(blank) == 0 {
		return nil
	}
	out := make([]string, 0, len(blank))
	for p := range blank {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// OrderByField renders the canonical wire spelling for a rejected `?orderBy=`
// token — `orderBy[<token>]`, the prefix every surface reports.
func OrderByField(token string) string { return KeyOrderBy + "[" + token + "]" }

// OrderByToken is the inverse: the token inside an `orderBy[<token>]` field
// name, for a surface whose wire has no bracket form to render. gRPC's order_by
// is already a typed field on the request message, so the prefix would name
// nothing there — the RULE that refused the token is shared, the spelling of
// the refusal is the surface's.
func OrderByToken(field string) (string, bool) {
	prefix := KeyOrderBy + "["
	if !strings.HasPrefix(field, prefix) || !strings.HasSuffix(field, "]") {
		return "", false
	}
	return field[len(prefix) : len(field)-1], true
}

// FieldsField renders the canonical wire spelling for a rejected `?fields=`
// token — `fields[<token>]`.
func FieldsField(token string) string { return KeyFields + "[" + token + "]" }

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
