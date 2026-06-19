package infra

import (
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// TableSchema is the explicit, complete Go-field ↔ physical-column map for one
// table (a PG table and/or the source of a Mongo view). The framework does NOT
// guess: there is no PascalToSnake/PluralizeSnake column inference and no
// `transient` tag — every persisted field is declared here, Go field on one
// side, physical column on the other. The same TableSchema is attached to the
// PG repository (NewBaseAggregateRepository.WithSchema) AND to the read-side
// View (View.Schema), so write, criteria, scan, compose and read all translate
// from one declaration. Because the map is complete, both directions are
// lossless by construction — column → Go is a trivial inversion, no acronym
// ambiguity, no reflection at read time.
//
// Type-anchored: NewTableSchema[T] binds the schema to the Go type so the boot
// validates every declared field exists on T and resolves its field index once.
// External view sources (fwinfra.FromMongo, whose physical columns belong to an
// upstream service) declare a type-less schema via NewExternalSchema.
type TableSchema struct {
	table string
	typ   reflect.Type // nil for external (type-less) schemas

	pkGo     string
	pkColumn string
	pkIndex  int // reflect field index of the Go PK field; -1 when ID is not an
	// exported struct field (the aggregate root carries id privately in
	// BaseEntity and exposes it via GetID/SetID).

	fkColumn string // child only — FK column referencing the root

	fields []schemaField // non-PK persisted fields, in declaration order
	byGo   map[string]schemaField
	byCol  map[string]schemaField

	softDelete string // "" = disabled
	createdAt  string // "" = disabled (not stamped on insert)
	updatedAt  string // "" = disabled (not stamped on insert/update)

	children map[string]*TableSchema // aggregate child schemas, keyed by Go type name

	// goSegment is, for a child schema embedded in a view, the parent-side Go
	// field name of the collection (e.g. "Addresses"). Set via View embed
	// wiring; empty for a root schema.
	goSegment string
}

type schemaField struct {
	goName string
	column string
	index  int // reflect field index in the struct; -1 = not a struct field
}

const tableSchemaDefaultPK = "id"

// NewTableSchema starts a type-anchored schema for table over Go type T. Field
// declarations are validated against T at call time (panic on a missing or
// unexported field) — that is the enforcement that replaces convention.
func NewTableSchema[T any](table string) *TableSchema {
	t := reflect.TypeOf((*T)(nil)).Elem()
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("infra.NewTableSchema: %s is not a struct", t))
	}
	s := newSchema(table)
	s.typ = t
	s.pkIndex = exportedFieldIndex(t, "ID")
	return s
}

// NewExternalSchema starts a type-less schema for a Mongo view source whose
// physical columns belong to an upstream service (fwinfra.FromMongo). Field
// declarations carry logical Go names (consumed by the composite view's
// criteria/Response) mapped to the upstream's doc columns; there is no struct
// to validate against.
func NewExternalSchema(table string) *TableSchema {
	return newSchema(table)
}

func newSchema(table string) *TableSchema {
	return &TableSchema{
		table:    table,
		pkGo:     "ID",
		pkColumn: tableSchemaDefaultPK,
		pkIndex:  -1,
		byGo:     map[string]schemaField{},
		byCol:    map[string]schemaField{},
		children: map[string]*TableSchema{},
	}
}

// exportedFieldIndex returns the single-depth field index of an exported field
// named goName, or -1 when absent / unexported / promoted from an embed.
func exportedFieldIndex(t reflect.Type, goName string) int {
	f, ok := t.FieldByName(goName)
	if !ok || f.PkgPath != "" || len(f.Index) != 1 {
		return -1
	}
	return f.Index[0]
}

// PK declares the primary-key mapping. The Go side is the BaseEntity contract
// field "ID"; the column defaults to "id" and is overridden here. Single-column.
func (s *TableSchema) PK(goName, column string) *TableSchema {
	s.pkGo = goName
	s.pkColumn = column
	if s.typ != nil {
		s.pkIndex = exportedFieldIndex(s.typ, goName)
	}
	return s
}

// FK declares the child's foreign-key column referencing the root. Child only.
func (s *TableSchema) FK(column string) *TableSchema {
	s.fkColumn = column
	return s
}

// Field declares one persisted Go field ↔ column pair. The field must exist and
// be exported on T (panic otherwise); the column must be unique within the
// schema (bijection).
func (s *TableSchema) Field(goName, column string) *TableSchema {
	idx := -1
	if s.typ != nil {
		idx = exportedFieldIndex(s.typ, goName)
		if idx < 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): %q is not an exported single-depth field of %s — only real persisted fields can be mapped",
				s.table, goName, s.typ.Name(),
			))
		}
	}
	if _, dup := s.byGo[goName]; dup {
		panic(fmt.Sprintf("infra.TableSchema(%s): field %q declared twice", s.table, goName))
	}
	if _, dup := s.byCol[column]; dup {
		panic(fmt.Sprintf("infra.TableSchema(%s): column %q claimed by more than one field — the map must be a bijection", s.table, column))
	}
	if column == s.pkColumn || column == s.softDelete || column == s.createdAt || column == s.updatedAt {
		panic(fmt.Sprintf("infra.TableSchema(%s): field column %q collides with a PK/managed column", s.table, column))
	}
	fd := schemaField{goName: goName, column: column, index: idx}
	s.fields = append(s.fields, fd)
	s.byGo[goName] = fd
	s.byCol[column] = fd
	return s
}

// SoftDelete enables the soft-delete predicate on col (read scope-gate +
// archive/unarchive SQL). Omitting it disables soft-delete: Archive/Unarchive
// are unavailable and the read gate is never applied.
func (s *TableSchema) SoftDelete(col string) *TableSchema { s.softDelete = col; return s }

// CreatedAt enables a framework-stamped created_at column: the framework writes
// col = NOW() on INSERT (it never relies on a DB DEFAULT it does not own).
func (s *TableSchema) CreatedAt(col string) *TableSchema { s.createdAt = col; return s }

// UpdatedAt enables a framework-stamped updated_at column: col = NOW() on
// INSERT and UPDATE.
func (s *TableSchema) UpdatedAt(col string) *TableSchema { s.updatedAt = col; return s }

// Child registers an aggregate child's schema, keyed by the child Go type name.
func (s *TableSchema) Child(child *TableSchema) *TableSchema {
	if child == nil || child.typ == nil {
		panic(fmt.Sprintf("infra.TableSchema(%s): Child requires a type-anchored schema", s.table))
	}
	s.children[child.typ.Name()] = child
	return s
}

// --- resolution -------------------------------------------------------------

func (s *TableSchema) Table() string    { return s.table }
func (s *TableSchema) PKColumn() string { return s.pkColumn }
func (s *TableSchema) FKColumn() string { return s.fkColumn }

// ColumnOf returns the physical column for a Go field name (PK included).
func (s *TableSchema) ColumnOf(goName string) (string, bool) {
	if goName == s.pkGo {
		return s.pkColumn, true
	}
	f, ok := s.byGo[goName]
	return f.column, ok
}

// GoOf returns the Go field name for a physical column (PK included). The
// inverse of ColumnOf — lossless because the map is complete.
func (s *TableSchema) GoOf(column string) (string, bool) {
	if column == s.pkColumn {
		return s.pkGo, true
	}
	f, ok := s.byCol[column]
	return f.goName, ok
}

// goNameForRead returns the logical Go field name for a physical column on the
// read path, including the managed columns under fixed logical names
// (created_at → "CreatedAt", updated_at → "UpdatedAt", soft-delete → "DeletedAt")
// so a view can project them to the wire without a domain Go field. Returns
// ok=false for a column the schema does not own (e.g. _id, foreign keys).
func (s *TableSchema) goNameForRead(column string) (string, bool) {
	if g, ok := s.GoOf(column); ok {
		return g, true
	}
	switch column {
	case s.createdAt:
		if s.createdAt != "" {
			return "CreatedAt", true
		}
	case s.updatedAt:
		if s.updatedAt != "" {
			return "UpdatedAt", true
		}
	case s.softDelete:
		if s.softDelete != "" {
			return "DeletedAt", true
		}
	}
	return "", false
}

// columnForRead returns the physical column for a logical Go field name on the
// read path — the forward inverse of goNameForRead. Covers mapped fields and
// the PK (via ColumnOf) plus the three managed columns under their fixed
// logical names (CreatedAt, UpdatedAt, DeletedAt), so a view can sort/project/
// filter on a managed column by its Go name symmetrically with the read-back.
// Returns ok=false for a name the schema does not own.
func (s *TableSchema) columnForRead(goName string) (string, bool) {
	if c, ok := s.ColumnOf(goName); ok {
		return c, true
	}
	switch goName {
	case "CreatedAt":
		if s.createdAt != "" {
			return s.createdAt, true
		}
	case "UpdatedAt":
		if s.updatedAt != "" {
			return s.updatedAt, true
		}
	case "DeletedAt":
		if s.softDelete != "" {
			return s.softDelete, true
		}
	}
	return "", false
}

func (s *TableSchema) softDeleteColumn() (string, bool) { return s.softDelete, s.softDelete != "" }
func (s *TableSchema) createdAtColumn() (string, bool)  { return s.createdAt, s.createdAt != "" }
func (s *TableSchema) updatedAtColumn() (string, bool)  { return s.updatedAt, s.updatedAt != "" }

func (s *TableSchema) childSchema(typeName string) *TableSchema {
	if s == nil {
		return nil
	}
	return s.children[typeName]
}

// insertNowColumns returns the managed columns stamped NOW() on INSERT —
// created_at then updated_at, each when enabled.
func (s *TableSchema) insertNowColumns() []string {
	var out []string
	if c, ok := s.createdAtColumn(); ok {
		out = append(out, c)
	}
	if c, ok := s.updatedAtColumn(); ok {
		out = append(out, c)
	}
	return out
}

// updateNowColumns returns the managed columns stamped NOW() on UPDATE.
func (s *TableSchema) updateNowColumns() []string {
	if c, ok := s.updatedAtColumn(); ok {
		return []string{c}
	}
	return nil
}

// writeFields returns the column → value map the INSERT/UPDATE binds, by reading
// each declared field's value via its resolved index. The PK is excluded
// (DB-generated + separate WHERE); managed NOW() columns are appended by the
// statement builders, not here.
func (s *TableSchema) writeFields(e any) domain.Fields {
	v := reflect.ValueOf(e)
	for v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	out := make(domain.Fields, len(s.fields))
	for _, f := range s.fields {
		if f.index < 0 {
			continue
		}
		out[f.column] = v.Field(f.index).Interface()
	}
	return out
}

// scanPlan returns the SELECT columns (in field order) + a column → reflect
// field-index map for the scanner. The PK column is included only when the PK is
// an exported struct field (aggregate child); for the root (pkIndex < 0) the PK
// is the leading key handled by scanLeadingKey, not a struct field.
func (s *TableSchema) scanPlan() (cols []string, byCol map[string]int) {
	byCol = make(map[string]int, len(s.fields)+1)
	if s.pkIndex >= 0 {
		cols = append(cols, s.pkColumn)
		byCol[s.pkColumn] = s.pkIndex
	}
	for _, f := range s.fields {
		if f.index < 0 {
			continue
		}
		cols = append(cols, f.column)
		byCol[f.column] = f.index
	}
	return cols, byCol
}

// fieldResolverFromSchema returns a criteria field resolver (Go field → column)
// backed by the schema. Unknown fields resolve to ok=false (the translator
// rejects them).
func (s *TableSchema) fieldResolver() fieldResolver {
	return func(goField string) (string, bool) {
		return s.ColumnOf(goField)
	}
}

// validateModes panics when the entity declares an archive verb but soft-delete
// is disabled — turning a runtime SQL error into a loud boot failure.
func (s *TableSchema) validateModes(modes []domain.EntityMode) {
	if _, ok := s.softDeleteColumn(); ok {
		return
	}
	for _, m := range modes {
		if m == domain.ModeArchive || m == domain.ModeUnarchive {
			name := s.table
			if s.typ != nil {
				name = s.typ.Name()
			}
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): entity declares %s in Modes() but SoftDelete is not enabled — "+
					"declare SoftDelete(col) or drop the mode",
				name, modeName(m),
			))
		}
	}
}

func modeName(m domain.EntityMode) string {
	if m == domain.ModeArchive {
		return "ModeArchive"
	}
	return "ModeUnarchive"
}
