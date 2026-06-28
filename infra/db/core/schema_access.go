package core

import (
	"reflect"
	"sync"
)

// Exported accessors over TableSchema's internals, for the framework's Mongo
// read-side layer (view / view_export / view_mongo_spec in package infra), which
// consumes the schema cross-package now that TableSchema lives here.

// IsExternal reports whether the schema is a type-less external source
// (NewExternalSchema) — i.e. an upstream Mongo collection embed, not a local
// relational table.
func (s *TableSchema) IsExternal() bool { return s.isExternal() }

// TypeName is the Go type name the schema is anchored on ("" for an external
// schema). Used to derive a local embed's parent-side document segment.
func (s *TableSchema) TypeName() string { return s.typeName() }

// HasPKDeclared reports whether a PK column was declared (every local schema must
// declare one; the read-side boot guard checks it).
func (s *TableSchema) HasPKDeclared() bool { return s.hasPKDeclared() }

// HasChildren reports whether the schema declares any aggregate child schemas.
func (s *TableSchema) HasChildren() bool { return len(s.children) > 0 }

// GoFields returns the Go field names of the mapped (non-PK, non-managed)
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

// PKIndex is the reflect struct-field index of the PK on the schema's Go type,
// or < 0 when there is no struct-field PK (e.g. an external schema). The
// aggregate loader uses it to decode a child's own PK column on scan.
func (s *TableSchema) PKIndex() int { return s.pkIndex }

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
	for v.Kind() == reflect.Ptr {
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
		out[f.goName] = v.Field(f.index).Interface()
	}
	return out
}
