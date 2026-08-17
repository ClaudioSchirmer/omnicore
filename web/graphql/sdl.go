// Package graphql exposes the framework's read/write handlers through a single
// POST /graphql endpoint whose schema is reflected from the same Request/
// Response DTOs the REST wrappers consume. A consumer "just attaches"
// handlers to a registry; the SDL, the argument set (where/pagination/sort),
// the Relay connection envelope and the criteria translation are derived
// automatically.
//
// The schema is emitted as SDL and loaded/validated by vektah/gqlparser; the
// framework owns only the executor (a sibling file). The package sits above
// web/queryschema (the shared read-side reflection + operator vocabulary) and
// depends on application/* exactly like the REST wrappers — never on infra.
package graphql

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
	"github.com/google/uuid"
)

var (
	timeType    = reflect.TypeOf(time.Time{})
	uuidType    = reflect.TypeOf(uuid.UUID{})
	domainIDTyp = reflect.TypeOf(domain.ID{})
)

// sdlBuilder accumulates the SDL type definitions a schema needs, deduped by
// type name (GraphQL requires unique names per schema) and emitted in a stable
// order so the generated document is reproducible.
type sdlBuilder struct {
	defs         map[string]string
	order        []string
	objectByType map[reflect.Type]string    // Go type → emitted object name (recursion break)
	objectNames  map[string]bool            // names claimed by entity/response OBJECT types (vs derived/infra types)
	objectFields map[string]map[string]bool // object name → wire field set (alignment guard)
	objectSource map[string]reflect.Type    // object name → the Go type that DEFINED it
	needDateTime bool
}

func newSDLBuilder() *sdlBuilder {
	return &sdlBuilder{
		defs:         map[string]string{},
		order:        []string{},
		objectByType: map[reflect.Type]string{},
		objectNames:  map[string]bool{},
		objectFields: map[string]map[string]bool{},
		objectSource: map[string]reflect.Type{},
	}
}

// put registers a DERIVED/INFRASTRUCTURE definition (connections, edges,
// PageInfo, where/operator/order inputs, enums, payload shells) once. The
// first writer wins for infra-vs-infra (the body is identical by construction
// — PageInfo, OrderDirection, one entity's connection emitted by its sibling
// fields); an infra name landing on a name already claimed by an entity/
// response OBJECT is a naming collision that would silently corrupt the graph
// — it panics (a boot fail: the registry builds at boot).
func (b *sdlBuilder) put(name, body string) {
	if b.objectNames[name] {
		panic("graphql: derived schema type " + strconv.Quote(name) +
			" collides with an entity/object type of the same name — rename the entity or the field")
	}
	if _, ok := b.defs[name]; ok {
		return
	}
	b.record(name, body)
}

// record inserts a definition unconditionally (callers own dedupe/guards).
func (b *sdlBuilder) record(name, body string) {
	b.defs[name] = body
	b.order = append(b.order, name)
}

// scalarName returns the GraphQL scalar name for a Go scalar / well-known type,
// or "" when t is not a leaf scalar. time.Time maps to the custom DateTime
// scalar (flagged for declaration); uuid.UUID / domain.ID map to ID.
func (b *sdlBuilder) scalarName(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t {
	case timeType:
		b.needDateTime = true
		return "DateTime"
	case uuidType, domainIDTyp:
		return "ID"
	}
	switch t.Kind() {
	case reflect.String:
		return "String"
	case reflect.Bool:
		return "Boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "Int"
	case reflect.Float32, reflect.Float64:
		return "Float"
	}
	return ""
}

// typeRef returns the SDL type reference for a Response DTO field type,
// registering any named object types it reaches. Scalars map directly;
// structs become (registered) objects; slices become list types; unknown
// shapes degrade to String so the schema still loads.
func (b *sdlBuilder) typeRef(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if s := b.scalarName(t); s != "" {
		return s
	}
	switch t.Kind() {
	case reflect.Struct:
		return b.objectType(t)
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() == reflect.Uint8 { // []byte → base64 string
			return "String"
		}
		return "[" + b.typeRef(elem) + "]"
	default:
		// maps and anything exotic — expose as String of the marshaled value.
		return "String"
	}
}

// objectType registers (once) the SDL object for a Response struct under its
// Go type name and returns it. Used for nested object types reached while
// walking a Response.
func (b *sdlBuilder) objectType(t reflect.Type) string {
	return b.objectTypeAs(graphqlName(t), t)
}

// objectTypeAs registers (once) the SDL object for a Response struct under the
// given name and returns it. The root node of a registered entity is emitted
// under the entity name (e.g. "User"), not the Go Response DTO name; nested
// objects fall back to their Go type name via objectType. Fields are named by
// their wire (json) name; the value the executor resolves against is the
// Go-field-keyed view document.
//
// One name, one object: when the name is already defined from ANOTHER Go type
// (the list and by-id Response DTOs of the same entity both registering under
// "User"), the first registration defines the object and later types map onto
// it without re-walking — so their nested types never leak into the SDL as
// orphans. Sharing an entity name REQUIRES the Response DTOs to be
// wire-aligned: the builder compares the later type's wire field set against
// the defined object and boot-fails on any difference — a field only one DTO
// carries would otherwise be silently unselectable (or resolve to null) on
// the other registration.
func (b *sdlBuilder) objectTypeAs(name string, t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if existing, ok := b.objectByType[t]; ok {
		return existing
	}
	if _, ok := b.defs[name]; ok {
		// The name is taken. Mapping onto it is legitimate ONLY when it is
		// another entity/response OBJECT (the shared-node-type contract);
		// landing on a derived/infrastructure type (PageInfo, a sibling's
		// Connection/Edge, an input, an enum) would silently point the node
		// at the wrong type — panic instead (a boot fail: the registry
		// builds at boot).
		if !b.objectNames[name] {
			panic("graphql: entity/object name " + strconv.Quote(name) +
				" collides with a derived/infrastructure schema type of the same name — pick a different entity name")
		}
		// Wire-alignment guard: the later DTO must carry EXACTLY the wire
		// fields the defining DTO emitted under this name.
		defined := b.objectFields[name]
		later := map[string]bool{}
		for _, f := range exportedJSONFields(t) {
			later[f.wire] = true
		}
		var diffs []string
		for w := range later {
			if !defined[w] {
				diffs = append(diffs, "field "+strconv.Quote(w)+" exists only on "+t.String())
			}
		}
		for w := range defined {
			if !later[w] {
				diffs = append(diffs, "field "+strconv.Quote(w)+" is missing on "+t.String())
			}
		}
		if len(diffs) > 0 {
			sort.Strings(diffs)
			src := "the defining DTO"
			if def, ok := b.objectSource[name]; ok {
				src = def.String()
			}
			panic("graphql: entity " + strconv.Quote(name) + " is shared by wire-misaligned Response DTOs (" +
				src + " defined it): " + strings.Join(diffs, "; ") +
				". Response DTOs sharing an entity name must expose the same wire field set")
		}
		b.objectByType[t] = name
		return name
	}
	b.objectByType[t] = name // break recursion before walking fields
	b.objectNames[name] = true
	fields := exportedJSONFields(t)
	fieldSet := make(map[string]bool, len(fields))
	var sb strings.Builder
	sb.WriteString("type " + name + " {\n")
	for _, f := range fields {
		fieldSet[f.wire] = true
		sb.WriteString("  " + f.wire + ": " + b.typeRef(f.field.Type) + "\n")
	}
	sb.WriteString("}")
	b.objectFields[name] = fieldSet
	b.objectSource[name] = t
	b.record(name, sb.String())
	return name
}

// whereInput registers the `<Entity>WhereInput` input object and its per-leaf
// operator inputs from a Request DTO's filter leaves. Returns the input type
// name and whether any filter leaf exists (when false, the field omits the
// `where` argument).
func (b *sdlBuilder) whereInput(entity string, reqType reflect.Type) (string, bool) {
	leaves := filterLeaves(reqType)
	if len(leaves) == 0 {
		return "", false
	}
	var sb strings.Builder
	whereName := entity + "WhereInput"
	sb.WriteString("input " + whereName + " {\n")
	for _, leaf := range leaves {
		opName := b.operatorInput(entity, leaf)
		sb.WriteString("  " + wireLeafName(leaf.WirePath) + ": " + opName + "\n")
	}
	sb.WriteString("}")
	b.put(whereName, sb.String())
	return whereName, true
}

// operatorInput registers (once) the per-leaf operator input object — one
// field per declared operator, the list operators (in/nin/iin/inin) taking a
// list of the leaf's scalar.
func (b *sdlBuilder) operatorInput(entity string, leaf queryschema.RequestField) string {
	name := entity + "_" + sanitize(leaf.WirePath) + "_Op"
	scalar := b.scalarName(leaf.Field.Type)
	if scalar == "" {
		scalar = "String"
	}
	var sb strings.Builder
	sb.WriteString("input " + name + " {\n")
	for _, op := range leaf.Ops {
		switch op {
		case queryschema.OpIn, queryschema.OpNin, queryschema.OpIIn, queryschema.OpINin:
			sb.WriteString("  " + op + ": [" + scalar + "!]\n")
		default:
			sb.WriteString("  " + op + ": " + scalar + "\n")
		}
	}
	sb.WriteString("}")
	b.put(name, sb.String())
	return name
}

// orderEnumValue converts a dotted wire path into its GraphQL enum value —
// SCREAMING_SNAKE, path dots as segment separators: `userName` → USER_NAME,
// `addresses.zipCode` → ADDRESSES_ZIP_CODE.
func orderEnumValue(wirePath string) string {
	var sb strings.Builder
	prevLowerOrDigit := false
	for _, r := range wirePath {
		switch {
		case r == '.' || r == '-' || r == '_':
			sb.WriteRune('_')
			prevLowerOrDigit = false
		case unicode.IsUpper(r):
			if prevLowerOrDigit {
				sb.WriteRune('_')
			}
			sb.WriteRune(r)
			prevLowerOrDigit = false
		case r >= '0' && r <= '9':
			sb.WriteRune(r)
			prevLowerOrDigit = true
		default:
			sb.WriteRune(unicode.ToUpper(r))
			prevLowerOrDigit = true
		}
	}
	return sb.String()
}

// orderFieldMap derives an entity's sortable vocabulary — ENUM value → wire
// path — from the Response DTO's projection schema, the SAME allowlist the
// REST `?orderBy=` tokens are validated against (ParseOrderByWithSchema), so
// the two surfaces cannot drift. Deterministic (values sorted by wire path);
// empty when the Response carries no typed paths (the orderBy argument is
// then omitted from the SDL). Two wire paths colliding on one enum value is a
// Response-DTO modeling error and panics at boot with both names.
//
// A COMPUTED path (`computed:"…"` on the Response — derived by the Query's
// FromQueryResult after the read, backed by no column) is EXCLUDED: it stays
// selectable, but it is not a member of the `<Entity>OrderField` enum, so
// gqlparser rejects an ordering by it during validation, before any resolver
// runs. That is this surface's native idiom for the refusal REST answers with
// ComputedFieldNotSortableNotification — the cut lands in the schema itself,
// the same posture the reserved-control arguments carry.
func orderFieldMap(entity string, projSchema *queryschema.ProjectionSchema) (values []string, byValue map[string]string) {
	if projSchema == nil || len(projSchema.Paths) == 0 {
		return nil, nil
	}
	wires := make([]string, 0, len(projSchema.Paths))
	for w := range projSchema.Paths {
		if _, isComputed := projSchema.Computed[w]; isComputed {
			continue
		}
		wires = append(wires, w)
	}
	if len(wires) == 0 {
		return nil, nil
	}
	sort.Strings(wires)
	byValue = make(map[string]string, len(wires))
	values = make([]string, 0, len(wires))
	for _, w := range wires {
		v := orderEnumValue(w)
		if prev, dup := byValue[v]; dup {
			panic("graphql: orderBy enum value collision on entity " + strconv.Quote(entity) +
				": wire paths " + strconv.Quote(prev) + " and " + strconv.Quote(w) +
				" both map to " + v + " — rename one of the Response DTO wire (json) names")
		}
		byValue[v] = w
		values = append(values, v)
	}
	return values, byValue
}

// orderInput registers (once) the order vocabulary for an entity — the shared
// OrderDirection enum, the per-entity `<Entity>OrderField` enum (one value per
// sortable wire path) and the `<Entity>Order` input `{ field!, direction =
// ASC }` — and returns the input name. ok=false when the entity has no
// sortable paths (a Response with no reflectable shape, or one whose every
// path is computed): the caller then omits the orderBy argument entirely.
func (b *sdlBuilder) orderInput(entity string, projSchema *queryschema.ProjectionSchema) (string, bool) {
	values, _ := orderFieldMap(entity, projSchema)
	if len(values) == 0 {
		return "", false
	}
	b.put("OrderDirection", "enum OrderDirection {\n  ASC\n  DESC\n}")
	fieldEnum := entity + "OrderField"
	b.put(fieldEnum, "enum "+fieldEnum+" {\n  "+strings.Join(values, "\n  ")+"\n}")
	in := entity + "Order"
	b.put(in, "input "+in+" {\n  field: "+fieldEnum+"!\n  direction: OrderDirection = ASC\n}")
	return in, true
}

// connection registers the Relay connection + edge types for an entity over
// the given node object, plus the shared PageInfo once. Returns the connection
// type name.
func (b *sdlBuilder) connection(entity, nodeType string) string {
	b.put("PageInfo", "type PageInfo {\n"+
		"  hasNextPage: Boolean!\n"+
		"  hasPreviousPage: Boolean!\n"+
		"  startCursor: String\n"+
		"  endCursor: String\n}")
	edge := entity + "Edge"
	b.put(edge, "type "+edge+" {\n  node: "+nodeType+"!\n  cursor: String!\n}")
	conn := entity + "Connection"
	// totalCount is NonNull: every list envelope carries the total on every
	// surface (pageToConnection always populates it) — GitHub-parity Int!.
	b.put(conn, "type "+conn+" {\n"+
		"  edges: ["+edge+"!]!\n"+
		"  pageInfo: PageInfo!\n"+
		"  totalCount: Int!\n}")
	return conn
}

// queryFieldSDL returns the SDL line for one read root field: the connection
// return type plus the where / keyset-pagination / orderBy / search /
// includeArchived arguments the endpoint's Request DTO declares.
//
// The DTO is the single source of truth for what a list endpoint exposes —
// on this surface the cut lands in the SCHEMA itself: an argument whose
// `query:"…"` key the DTO does not declare is not emitted, so introspection
// and the playground never advertise it and gqlparser rejects it as an
// unknown argument before any resolver runs (the same posture the OpenAPI
// parameters carry on REST). The `where:` input follows the same rule via
// the DTO's filter tags. What the DTO cannot govern here is what the
// language expresses natively: the selection IS the projection, and the
// only-total mode is a selection shape — both gate-exempt by nature.
func (b *sdlBuilder) queryFieldSDL(name, entity string, reqType, respType reflect.Type) string {
	node := b.objectTypeAs(entity, respType)
	conn := b.connection(entity, node)
	reserved := queryschema.ExtractRequestSchema(reqType).Reserved
	args := []string{}
	if whereName, ok := b.whereInput(entity, reqType); ok {
		args = append(args, "where: "+whereName)
	}
	// orderBy is the one reserved control with a typed, per-entity argument:
	// `orderBy: [<Entity>Order!]` over the reflected sortable-field enum. When
	// the Response carries no typed paths the argument is omitted even under
	// the DTO opt-in — there is nothing to enumerate.
	orderBySDL := ""
	if reserved[queryschema.KeyOrderBy] {
		if in, ok := b.orderInput(entity, queryschema.ExtractProjectionSchema(respType)); ok {
			orderBySDL = "orderBy: [" + in + "!]"
		}
	}
	for _, arg := range []struct {
		key string
		sdl string
	}{
		{queryschema.KeyFirst, "first: Int"},
		{queryschema.KeyAfter, "after: String"},
		{queryschema.KeyLast, "last: Int"},
		{queryschema.KeyBefore, "before: String"},
		{queryschema.KeyOrderBy, orderBySDL},
		{queryschema.KeySearch, "search: String"},
		{queryschema.KeyIncludeArchived, "includeArchived: Boolean"},
	} {
		if reserved[arg.key] && arg.sdl != "" {
			args = append(args, arg.sdl)
		}
	}
	if len(args) == 0 {
		return "  " + name + ": " + conn + "!"
	}
	return "  " + name + "(" + strings.Join(args, ", ") + "): " + conn + "!"
}

// queryByIDFieldSDL returns the SDL line for one by-id read root field: the
// entity node (nullable — a missing document resolves to null beside the
// canonical not-found error) with the mandatory id argument plus
// includeArchived when the endpoint's Request DTO declares the reserved key —
// the same DTO-governed cut queryFieldSDL applies to the list arguments.
func (b *sdlBuilder) queryByIDFieldSDL(name, entity string, respType reflect.Type, includeArchived bool) string {
	node := b.objectTypeAs(entity, respType)
	args := "id: ID!"
	if includeArchived {
		args += ", includeArchived: Boolean"
	}
	return "  " + name + "(" + args + "): " + node
}

// document assembles the full SDL: the custom scalar (when used), the
// accumulated type definitions in registration order, and the supplied root
// blocks (Query / Mutation). Root blocks are passed in by the registry so this
// builder stays read/write-agnostic.
func (b *sdlBuilder) document(roots ...string) string {
	var sb strings.Builder
	if b.needDateTime {
		sb.WriteString("scalar DateTime\n\n")
	}
	for _, name := range b.order {
		sb.WriteString(b.defs[name])
		sb.WriteString("\n\n")
	}
	for _, r := range roots {
		sb.WriteString(r)
		sb.WriteString("\n\n")
	}
	return strings.TrimRight(sb.String(), "\n") + "\n"
}

// ── reflection helpers (Response field walk + name sanitizing) ──────────────

type jsonField struct {
	field reflect.StructField
	wire  string
}

// exportedJSONFields returns the wire-relevant fields of a Response struct in
// declaration order: exported, not `json:"-"`, anonymous structs promoted
// (mirroring encoding/json and queryschema's projection reflection).
func exportedJSONFields(t reflect.Type) []jsonField {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []jsonField
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				out = append(out, exportedJSONFields(ft)...)
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
		wire := f.Name
		if tag != "" {
			if name, _, _ := strings.Cut(tag, ","); name != "" {
				wire = name
			}
		}
		out = append(out, jsonField{field: f, wire: wire})
	}
	return out
}

// filterLeaves returns the filter leaves of a Request DTO in declaration order
// via the shared queryschema traversal.
func filterLeaves(reqType reflect.Type) []queryschema.RequestField {
	var out []queryschema.RequestField
	for _, f := range queryschema.WalkRequest(reqType) {
		if f.Ops != nil {
			out = append(out, f)
		}
	}
	return out
}

// graphqlName returns the GraphQL type name for a Go type — its bare Go type
// name, or a stable synthetic name for anonymous types.
func graphqlName(t reflect.Type) string {
	if n := t.Name(); n != "" {
		return n
	}
	return "Anon" + sanitize(t.String())
}

// wireLeafName flattens a dotted filter wire path into a single GraphQL input
// field name (dots are illegal in GraphQL names): `addresses.zipCode` →
// `addresses_zipCode`.
func wireLeafName(wirePath string) string { return strings.ReplaceAll(wirePath, ".", "_") }

// sanitize keeps only GraphQL-name-legal runes.
func sanitize(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b = append(b, r)
		}
	}
	return string(b)
}
