package queryschema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// RequestSchema is the cached reflection result for a Request DTO type:
// every accepted wire key (flat or dotted) → its FilterSpec, the reserved
// pagination/control set (top-level only — embed groups carry filter leaves,
// not pagination keys), and the ordering vocabulary.
type RequestSchema struct {
	Filters  map[string]FilterSpec
	Reserved map[string]bool

	// Sortable is the ordering vocabulary: wire path → the Go field path it
	// addresses plus the directions the declaration allows. A leaf enters it
	// by carrying a `sort:"asc"` / `sort:"desc"` / `sort:"asc,desc"`
	// tag, at ANY depth — a leaf inside an embed group contributes its dotted
	// path exactly as a filter leaf does.
	//
	// It is the VOCABULARY half of the ordering contract: which paths
	// `?orderBy=` accepts and in which directions. The SWITCH half — whether
	// the endpoint accepts the parameter at all — is the `query:"orderBy"`
	// control in Reserved, and validateOrderingPair enforces that the two
	// travel together. Ordering is a store operation whose cost is
	// proportional to the matching set unless an index covers it, so the
	// vocabulary is closed and explicit — an endpoint sorts by what it says it
	// sorts by, and nothing else.
	Sortable map[string]SortSpec
}

// SortSpec is one entry of the ordering vocabulary: the Go field path the wire
// token addresses (the reader translates it to a physical column through the
// view's TableSchema, exactly as it does for a filter leaf) and the directions
// the declaration admits.
type SortSpec struct {
	GoPath string
	Asc    bool
	Desc   bool
}

// Allows reports whether this declaration admits the requested direction.
func (s SortSpec) Allows(desc bool) bool {
	if desc {
		return s.Desc
	}
	return s.Asc
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
//   - Ops == nil && Sort != nil → vocabulary leaf (`query:"id" sort:"asc"`);
//     part of the endpoint's query vocabulary and orderable, but not
//     filterable and carrying no value on the wire. Legal at any depth.
//   - Ops == nil && Sort == nil && !Group → reserved/control scalar
//     (`query:"first"` etc.); honored as a reserved key only when TopLevel,
//     and only for the canonical ControlKeys vocabulary (anything
//     else is a boot fail — see ExtractRequestSchema).
//
// Sort is orthogonal to the rest: it holds the directions declared by the
// `sort:` tag, in tag order, and may accompany a filter leaf (filterable
// AND orderable) or stand alone (the vocabulary leaf above).
type RequestField struct {
	WirePath string
	GoPath   string
	Field    reflect.StructField
	Ops      []string
	Sort     []string
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
		// The ordering declaration rides along on whatever the field turns out
		// to be: a filter leaf may also be orderable, and a leaf that is ONLY
		// orderable is a legal shape of its own. The classification below does
		// not branch on it — ExtractRequestSchema does.
		sortDirs := splitSort(f)

		if ftag := f.Tag.Get("filter"); ftag != "" {
			*out = append(*out, RequestField{
				WirePath: wirePath, GoPath: goPath, Field: f,
				Ops: splitOps(ftag), Sort: sortDirs, TopLevel: topLevel,
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
				Sort: sortDirs, Group: true, TopLevel: topLevel,
			})
			walkRequestLevel(ft, wirePath, goPath, false, out)
			continue
		}

		// Vocabulary leaf (carries `sort:`) or reserved control scalar.
		*out = append(*out, RequestField{
			WirePath: wirePath, GoPath: goPath, Field: f,
			Sort: sortDirs, TopLevel: topLevel,
		})
	}
}

// SortTag is the Request struct tag that puts a field in the endpoint's
// ordering vocabulary, listing the directions it admits:
//
//	Code *string `query:"code" filter:"eq,startswith" sort:"asc,desc"`
//	ID   *string `query:"id"                          sort:"asc"`
//
// It mirrors `filter:` — a comma-separated list of what the leaf accepts —
// and is legal on a leaf at any depth, including inside an embed group.
const SortTag = "sort"

// Sort direction tokens accepted by the SortTag. The vocabulary is closed:
// anything else is a boot fail, which keeps room to widen the tag later
// without silently reinterpreting a value that used to be rejected.
const (
	SortAsc  = "asc"
	SortDesc = "desc"
)

// splitSort returns the directions declared by f's `sort:` tag, in tag
// order. Returns nil when the tag is ABSENT (the field is not orderable) and a
// possibly-empty slice when it is present, so the caller can tell "not
// declared" from "declared empty" — the latter is a boot fail.
func splitSort(f reflect.StructField) []string {
	raw, ok := f.Tag.Lookup(SortTag)
	if !ok {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// sortSpecOf folds the declared directions into a SortSpec. Reports false when
// the list is empty, names something outside the closed {asc, desc} vocabulary,
// or repeats a direction.
func sortSpecOf(goPath string, dirs []string) (SortSpec, bool) {
	spec := SortSpec{GoPath: goPath}
	if len(dirs) == 0 {
		return spec, false
	}
	for _, d := range dirs {
		switch d {
		case SortAsc:
			if spec.Asc {
				return spec, false
			}
			spec.Asc = true
		case SortDesc:
			if spec.Desc {
				return spec, false
			}
			spec.Desc = true
		default:
			return spec, false
		}
	}
	return spec, true
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
		Sortable: map[string]SortSpec{},
	}
	for _, leaf := range WalkRequest(t) {
		key := leaf.Field.Tag.Get("query")
		// The ordering declaration is read before the classification because it
		// is orthogonal to it: it may ride on a filter leaf or stand alone, and
		// it is legal at any depth (an embed group's leaves contribute their
		// dotted paths exactly as filter leaves do).
		if leaf.Sort != nil {
			if leaf.Group {
				panic(fmt.Sprintf(
					"queryschema: %s.%s declares %s on an embed group — a group carries no value to order by; put the tag on the leaves inside it",
					t.String(), leaf.Field.Name, SortTag))
			}
			if leaf.Ops == nil && ControlKeys[key] {
				panic(fmt.Sprintf(
					"queryschema: %s.%s declares %s on the reserved control key %q — a control is not part of the ordering vocabulary",
					t.String(), leaf.Field.Name, SortTag, key))
			}
			spec, ok := sortSpecOf(leaf.GoPath, leaf.Sort)
			if !ok {
				panic(fmt.Sprintf(
					"queryschema: %s.%s declares %s:%q — each entry must be %q or %q, listed at most once",
					t.String(), leaf.Field.Name, SortTag,
					leaf.Field.Tag.Get(SortTag), SortAsc, SortDesc))
			}
			s.Sortable[leaf.WirePath] = spec
		}
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
		case leaf.Sort != nil:
			// Vocabulary leaf — recorded above. It declares no filter and
			// consumes no wire key of its own; it exists to name a path the
			// endpoint can order by.
		case leaf.TopLevel && ControlKeys[key]:
			s.Reserved[key] = true
		default:
			// Every remaining shape is a declaration that opts nothing in
			// while the OpenAPI generator would advertise it. Fail loud at
			// construction, like the fields-response guard.
			panic(deadQueryTag(t, leaf, key))
		}
	}
	if err := validateOrderingPair(t, s); err != "" {
		panic(err)
	}
	schemaCache.Store(t, s)
	return s
}

// validateOrderingPair enforces that the two halves of the ordering contract
// travel together, and says which half is missing.
//
// They answer different questions and are declared in different places, so the
// diagnostics keep them apart:
//
//   - `query:"orderBy"` is the SWITCH. It decides whether the endpoint accepts
//     the parameter at all — on every connector — and it is where a
//     `description:` for it lives.
//   - `sort:"asc,desc"` on a leaf is the VOCABULARY. It decides which paths the
//     parameter accepts and in which directions.
//
// Either alone is a dead declaration, and the framework fails loud on those
// rather than shipping a parameter that can only ever answer 400.
func validateOrderingPair(t reflect.Type, s *RequestSchema) string {
	switch {
	case s.Reserved[KeyOrderBy] && len(s.Sortable) == 0:
		return fmt.Sprintf(
			"queryschema: %s declares query:%q — the ordering SWITCH — but no field declares the VOCABULARY it switches on. "+
				"The endpoint would accept `?orderBy=` and then refuse every token it could be given. "+
				"Tag the orderable leaves (%s:%q / %s:%q / %s:%q), or drop the control field.",
			t.String(), KeyOrderBy,
			SortTag, SortAsc, SortTag, SortDesc, SortTag, SortAsc+","+SortDesc)
	case !s.Reserved[KeyOrderBy] && len(s.Sortable) > 0:
		return fmt.Sprintf(
			"queryschema: %s declares the ordering VOCABULARY on %s — but no query:%q field to switch it on. "+
				"The endpoint does not accept `?orderBy=`, so those tags reach no wire. "+
				"Add the control field (OrderBy *string, tagged query:%q), or drop the %s tags.",
			t.String(), strings.Join(sortableWirePaths(s), ", "), KeyOrderBy, KeyOrderBy, SortTag)
	}
	return ""
}

// sortableWirePaths lists the declared ordering vocabulary, sorted, so the
// diagnostic names the exact leaves instead of pointing at the DTO at large.
func sortableWirePaths(s *RequestSchema) []string {
	out := make([]string, 0, len(s.Sortable))
	for wire := range s.Sortable {
		out = append(out, wire)
	}
	sort.Strings(out)
	return out
}

// ParseOrderBy turns a comma-separated `?orderBy=` value into the ordering
// terms a ReadCriteria carries. A token may carry a `-` prefix (descending);
// bare is ascending.
//
// Each token is validated against the endpoint's declared ordering vocabulary
// and translated to the Go field path, which the reader resolves to a physical
// column through the view's TableSchema — the same two hops a filter leaf
// takes. A token outside the vocabulary, or one asking for a direction the
// declaration does not admit, is rejected with the canonical schema violation
// naming the wire token verbatim, `-` prefix included.
//
// A path may appear at most ONCE. A repeated token is not a harmless no-op: the
// terms become the reader's sort document, and a duplicated key is a malformed
// one — Mongo refuses the whole read. `?orderBy=name,-name` is the same
// mistake wearing a direction, so both spellings are refused on the SECOND
// occurrence, which is the token the consumer has to remove.
//
// An empty vocabulary rejects everything, which is unreachable in practice:
// with nothing declared orderable the endpoint does not accept `?orderBy=` at
// all, and the control gateway refuses it before this parser is consulted.
func ParseOrderBy(s string, sortable map[string]SortSpec) (orderBy []queries.OrderByField, violation *Violation, ok bool) {
	if s == "" {
		return nil, nil, true
	}
	tokens := strings.Split(s, ",")
	orderBy = make([]queries.OrderByField, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
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
		spec, declared := sortable[wireName]
		if !declared || !spec.Allows(desc) {
			return nil, SchemaViolation(OrderByField(t)), false
		}
		if seen[wireName] {
			return nil, SchemaViolation(OrderByField(t)), false
		}
		seen[wireName] = true
		orderBy = append(orderBy, queries.OrderByField{Field: spec.GoPath, Desc: desc})
	}
	return orderBy, nil, true
}

// deadQueryTag builds the boot-panic diagnostic for a `query:`-tagged field
// that opts nothing in. Three shapes reach it, each with its own fix.
func deadQueryTag(t reflect.Type, leaf RequestField, key string) string {
	switch {
	case ControlKeys[key]:
		return fmt.Sprintf(
			"queryschema: %s.%s declares query:%q inside an embed group — the reserved controls are endpoint-wide and are honored only at the top level of the Request DTO",
			t.String(), leaf.Field.Name, key)
	default:
		return fmt.Sprintf(
			"queryschema: %s.%s declares query:%q, which opts nothing in — a query-tagged scalar must carry filter:\"…\" to be filterable, %s:\"…\" to be orderable, or be one of the canonical top-level controls (%s)",
			t.String(), leaf.Field.Name, key, SortTag, strings.Join(controlKeyList, ", "))
	}
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
