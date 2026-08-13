package core

import (
	"reflect"
	"sync"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Exported accessors over TableSchema's internals, for the framework's Mongo
// read-side layer (view / view_export / view_mongo_spec in package infra), which
// consumes the schema cross-package now that TableSchema lives here.

// IsExternal reports whether the schema is a type-less external source
// (NewExternalSchema) — i.e. an upstream Mongo collection embed, not a local
// relational table.
func (s *TableSchema) IsExternal() bool { return s.isExternal() }

// TypeName is the Go type name the schema is anchored on ("" for an external
// schema). It names the schema, never a document segment — the read side nests
// a child collection under CollectionSegment.
func (s *TableSchema) TypeName() string { return s.typeName() }

// CollectionSegment is the document segment (and Go segment) a schema occupies
// inside its owner: the name its Go type declares via CollectionName. It is a
// property of the TYPE, so it answers the same for every schema instance over
// that type and does not depend on the schema having been registered with
// Child(...) — resolution is cached in the domain, so repeated calls are a map
// read.
//
// Empty only for a type-less external source (NewExternalSchema), which is never
// an aggregate child. Panics for a type that declares no valid CollectionName —
// the same declaration error Child(...) rejects at boot.
func (s *TableSchema) CollectionSegment() string {
	if s.typ == nil {
		return ""
	}
	return domain.CollectionSegmentOf(s.typ)
}

// HasPKDeclared reports whether a ID column was declared (every local schema must
// declare one; the read-side boot guard checks it).
func (s *TableSchema) HasPKDeclared() bool { return s.hasPKDeclared() }

// HasChildren reports whether the schema declares any aggregate child schemas.
func (s *TableSchema) HasChildren() bool { return len(s.children) > 0 }

// GoFields returns the Go field names of the mapped (non-ID, non-managed)
// persisted fields, in declaration order — the business columns the tabular
// export and the view spec iterate.
func (s *TableSchema) GoFields() []string {
	out := make([]string, 0, len(s.fields))
	for _, f := range s.fields {
		out = append(out, f.goName)
	}
	return out
}

// labelKeyBySchemaCache memoizes the Go-field-name → catalog key map per schema
// so the reflection over the schema's struct tags runs once per TableSchema.
var labelKeyBySchemaCache sync.Map // map[*TableSchema]map[string]string

// labelKeysByGoField reads each field's labelKey tag (explicit on the schema
// field, else the Go struct tag) into a Go-field-name → catalog key map. Lives
// here on the schema foundation (it touches TableSchema internals); the audit
// builder and tabular export reach it through the exported LabelKeysByGoField.
func labelKeysByGoField(schema *TableSchema) map[string]string {
	if schema == nil {
		return nil
	}
	if cached, ok := labelKeyBySchemaCache.Load(schema); ok {
		return cached.(map[string]string)
	}
	var out map[string]string
	for _, f := range schema.fields {
		tag := f.labelKey
		if tag == "" && schema.typ != nil && f.index >= 0 {
			if t, ok := schema.typ.Field(f.index).Tag.Lookup("labelKey"); ok {
				tag = t
			}
		}
		if tag == "" || tag == "-" {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[f.goName] = tag
	}
	labelKeyBySchemaCache.Store(schema, out)
	return out
}

// LabelKeysByGoField returns the labelKey-tag map (Go field name → catalog key)
// for the schema's fields. The audit builder + tabular export render labels from
// it; the map is empty when no field carries a labelKey tag.
func (s *TableSchema) LabelKeysByGoField() map[string]string { return labelKeysByGoField(s) }

// anchoredLabelKeyCache memoizes the composed map per (schema, anchor) pair so
// the reflection over the anchor's struct tags runs once per pair. Only the
// single-anchor form — the audit path's per-event shape — is cached; the
// multi-anchor form is resolved at route-mount time and needs no memo.
var anchoredLabelKeyCache sync.Map // map[anchoredLabelKey]map[string]string

type anchoredLabelKey struct {
	schema *TableSchema
	anchor reflect.Type
}

// LabelKeysByGoFieldAnchoredOn is the label map of a TYPE-LESS schema composed
// with the `labelKey:"…"` struct tags of the Go types that carry its fields
// flat. A shared base (NewSharedBaseSchema) and an upstream source
// (NewExternalSchema) have no struct of their own, so LabelKeysByGoField only
// sees the labels declared inline on Field(goName, column, labelKey) — while for
// a shared base the domain label naturally lives on the ROLE struct, whose flat
// fields the base partitions. Each anchor is matched by Go field name, exactly
// as the value composition matches it.
//
// Precedence, per field: the inline schema-level label wins (it is the explicit
// declaration); otherwise the first anchor, in the order given, whose struct
// declares a non-empty `labelKey` tag for that field. Passing no anchor — or a
// nil / non-struct one — degrades to LabelKeysByGoField.
//
// A TYPE-ANCHORED schema ignores the anchors entirely: its own struct is the
// single source of its labels (which is why Field(...) boot-panics on an inline
// label there), so no outside type may inject one.
//
// A SharedBaseView hands every declared role in declaration order (a role may
// legitimately omit a shared field, or leave it untagged); the role-rooted
// paths hand the single role type.
func (s *TableSchema) LabelKeysByGoFieldAnchoredOn(anchors ...reflect.Type) map[string]string {
	if s == nil {
		return nil
	}
	if len(anchors) == 0 || !s.isExternal() {
		return labelKeysByGoField(s)
	}
	if len(anchors) == 1 {
		key := anchoredLabelKey{schema: s, anchor: anchors[0]}
		if cached, ok := anchoredLabelKeyCache.Load(key); ok {
			return cached.(map[string]string)
		}
		out := composeAnchoredLabelKeys(s, anchors)
		anchoredLabelKeyCache.Store(key, out)
		return out
	}
	return composeAnchoredLabelKeys(s, anchors)
}

// composeAnchoredLabelKeys resolves one label per schema field: the inline
// declaration when present, else the first anchor tagging that field name.
func composeAnchoredLabelKeys(s *TableSchema, anchors []reflect.Type) map[string]string {
	inline := labelKeysByGoField(s)
	out := make(map[string]string, len(s.fields))
	for k, v := range inline {
		out[k] = v
	}
	for _, anchor := range anchors {
		for anchor != nil && anchor.Kind() == reflect.Pointer {
			anchor = anchor.Elem()
		}
		if anchor == nil || anchor.Kind() != reflect.Struct {
			continue
		}
		for _, f := range s.fields {
			if _, taken := out[f.goName]; taken {
				continue
			}
			sf, ok := anchor.FieldByName(f.goName)
			if !ok || sf.PkgPath != "" {
				continue
			}
			if tag, ok := sf.Tag.Lookup("labelKey"); ok && tag != "" && tag != "-" {
				out[f.goName] = tag
			}
		}
	}
	return out
}

// CreatedAtColumn / UpdatedAtColumn return the managed timestamp column names
// ("" when not declared) — the Mongo view spec enumerates them as part of the
// physical column set the composer emits.
func (s *TableSchema) CreatedAtColumn() string { return s.createdAt }
func (s *TableSchema) UpdatedAtColumn() string { return s.updatedAt }

// MappedColumns returns every mapped physical column (the byCol keys) — the
// business columns plus any renamed ones.
func (s *TableSchema) MappedColumns() []string {
	out := make([]string, 0, len(s.byCol))
	for col := range s.byCol {
		out = append(out, col)
	}
	return out
}

// ColumnForRead / GoNameForRead are the read-path Go↔column translators the Mongo
// view reader uses (exported wrappers over the internal forms).
func (s *TableSchema) ColumnForRead(goName string) (string, bool) { return s.columnForRead(goName) }
func (s *TableSchema) GoNameForRead(column string) (string, bool) { return s.goNameForRead(column) }

// ChildSchemaNames returns the Go type names of the declared aggregate child
// schemas (the child map keys). The aggregate loader iterates them to hydrate
// children; len()==0 means a flat entity. Order is unspecified (map iteration).
func (s *TableSchema) ChildSchemaNames() []string {
	out := make([]string, 0, len(s.children))
	for name := range s.children {
		out = append(out, name)
	}
	return out
}

// GoType is the reflect.Type the schema is anchored on (nil for a type-less
// external schema). The aggregate loader uses it to allocate a child struct
// (reflect.New) before scanning a child row into it.
func (s *TableSchema) GoType() reflect.Type { return s.typ }

// IDIndex is the reflect struct-field index of the ID on the schema's Go type,
// or < 0 when there is no struct-field ID (e.g. an external schema). The
// aggregate loader uses it to decode a child's own ID column on scan.
func (s *TableSchema) IDIndex() int { return s.idIndex }

// GoFieldValues returns the schema's persisted fields of e keyed by Go field
// name (PascalCase) — the faithful domain vocabulary the audit timeline speaks.
// Non-struct fields (index < 0) are skipped. Map-blind: never the physical
// column. Lives here on the schema foundation (it reflects over schema internals)
// and the audit builder consumes it through this exported method.
func (s *TableSchema) GoFieldValues(e any) map[string]any {
	if s == nil {
		return map[string]any{}
	}
	v := reflect.ValueOf(e)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return map[string]any{}
	}
	out := make(map[string]any, len(s.fields))
	for _, f := range s.fields {
		if f.index < 0 {
			continue
		}
		// A value-object field surfaces as its underlying scalar in the audit
		// timeline (unwrapVO is a no-op for a plain field).
		out[f.goName] = unwrapVO(v.Field(f.index).Interface())
	}
	return out
}

// PayloadColumnTypes returns the Go type of every SCALAR column the outbox
// payload can carry flat at the top for this schema — the type map the
// read-side payload decoder uses to restore native values from JSON (numbers
// via json.Number, timestamps from RFC 3339, []byte from base64) so the
// projected document carries the same value shapes the composer produces:
//
//   - the schema's own declared fields (typed via the anchored struct);
//   - every sibling's fields (same struct — a sibling partitions the row);
//   - the shared-base business fields (type-less base, typed via the ROLE's
//     struct through the resolved scan plan);
//   - the managed timestamp / DeletedAt columns (time.Time), own and base's;
//   - the ID column and the shared-base ParentID column (canonical uuid strings on
//     the wire and in the document alike).
//
// Type-less schemas (external sources) contribute nothing beyond ID/managed —
// they never ride the write-side payload.
func (s *TableSchema) PayloadColumnTypes() map[string]reflect.Type {
	out := map[string]reflect.Type{}
	stringT := reflect.TypeOf("")
	timeT := reflect.TypeOf(time.Time{})
	// A value-object field is carried in the payload as its UNDERLYING scalar
	// (the write path unwraps it), so the read-side decoder must coerce against
	// the underlying, not the named VO type — otherwise an int/time VO would miss
	// the decoder's exact-type/kind cases.
	resolveType := func(ft reflect.Type) reflect.Type {
		if _, u, ok := valueObjectField(ft); ok {
			return u
		}
		return ft
	}
	addFields := func(sc *TableSchema) {
		if sc == nil || s.typ == nil {
			return
		}
		for _, f := range sc.fields {
			if f.index >= 0 {
				out[f.column] = resolveType(s.typ.Field(f.index).Type)
			}
		}
	}
	addManaged := func(sc *TableSchema) {
		if sc == nil {
			return
		}
		if sc.deletedAt != "" {
			out[sc.deletedAt] = timeT
		}
		if sc.createdAt != "" {
			out[sc.createdAt] = timeT
		}
		if sc.updatedAt != "" {
			out[sc.updatedAt] = timeT
		}
	}
	addFields(s)
	for _, sib := range s.siblings {
		addFields(sib)
	}
	if s.idColumn != "" {
		out[s.idColumn] = stringT
	}
	addManaged(s)
	if l := s.sharedBaseLink; l != nil {
		out[l.parentIDColumn] = stringT
		if s.typ != nil {
			for col, idx := range l.scanByCol {
				out[col] = resolveType(s.typ.Field(idx).Type)
			}
		}
		addManaged(l.base)
		if rc := l.base.revisionCol; rc != "" {
			out[rc] = reflect.TypeOf(int64(0))
		}
	}
	// A child schema answers for its own columns (the caller walks children
	// through ChildSchemas); its ParentID column is uuid-shaped like the ID.
	if s.parentIDColumn != "" {
		out[s.parentIDColumn] = stringT
	}
	return out
}

// SharedBaseBusinessColumns returns, for a ROLE schema, the physical columns
// of its shared base's BUSINESS fields — the subset of the payload's flat
// scalars that belongs to the shared identity and must be revision-guarded on
// every read-model write (multi-writer data: any role of the identity writes
// them). Nil when the schema declares no shared base.
func (s *TableSchema) SharedBaseBusinessColumns() []string {
	if s == nil || s.sharedBaseLink == nil {
		return nil
	}
	return s.sharedBaseLink.scanCols
}
