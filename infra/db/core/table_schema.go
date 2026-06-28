package core

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
// External view sources (fwinfra.FromSchema over a type-less NewExternalSchema,
// whose physical columns belong to an upstream service) carry no Go struct.
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

	// labelKey is the header catalog key declared inline on the schema. It is
	// EXTERNAL-ONLY (NewExternalSchema): a type-less schema has no Go struct to
	// carry a labelKey:"…" tag, so the "mini-domain" declares the label here.
	// On a type-anchored schema the struct tag is the single source (declaring
	// a schema-level label there is a boot panic — never two ways to express
	// one domain concept). "" when none.
	labelKey string
}

// NewTableSchema starts a type-anchored schema for table over Go type T. Field
// declarations are validated against T at call time (panic on a missing or
// unexported field) — that is the enforcement that replaces convention. The
// primary key column is NOT assumed: the developer must declare it via
// PK(col) (the Go side is fixed to the Entity contract's "ID").
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
	return s
}

// NewExternalSchema starts a type-less schema for a Mongo view source whose
// physical columns belong to an upstream service (consumed via fwinfra.FromSchema). Field
// declarations carry logical Go names (consumed by the composite view's
// criteria/Response) mapped to the upstream's doc columns; there is no struct
// to validate against.
func NewExternalSchema(table string) *TableSchema {
	return newSchema(table)
}

func newSchema(table string) *TableSchema {
	return &TableSchema{
		table: table,
		// No PK default — pkGo/pkColumn stay empty until PK(...) is declared
		// (the developer must declare the column; the Go side is the Entity
		// contract's "ID", never guessed from the column name).
		pkIndex:  -1,
		byGo:     map[string]schemaField{},
		byCol:    map[string]schemaField{},
		children: map[string]*TableSchema{},
	}
}

// hasPKDeclared reports whether PK(...) was explicitly declared. There is NO
// default primary key — a schema without an explicit PK is a boot failure at
// every consumer checkpoint (WithSchema, Child, ValidateViewSchemas).
func (s *TableSchema) hasPKDeclared() bool { return s.pkColumn != "" }

// exportedFieldIndex returns the single-depth field index of an exported field
// named goName, or -1 when absent / unexported / promoted from an embed.
func exportedFieldIndex(t reflect.Type, goName string) int {
	f, ok := t.FieldByName(goName)
	if !ok || f.PkgPath != "" || len(f.Index) != 1 {
		return -1
	}
	return f.Index[0]
}

// ensureColumnFree panics when column is already claimed by the PK, a mapped
// field, or another managed column — enforcing the bijection over the full
// physical column set (PK + every Field + soft-delete/created/updated)
// regardless of the order PK/Field/SoftDelete/CreatedAt/UpdatedAt are declared.
// `self` names the slot being (re)assigned so reassigning it to the same column
// is not flagged as a self-collision. Field() runs its own equivalent check at
// declaration time; this covers every managed setter + PK so a collision is a
// boot panic no matter which side is declared second.
func (s *TableSchema) ensureColumnFree(column, self string) {
	if column == "" {
		return
	}
	collide := func(role string) {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): column %q claimed by both %s and %s — the map must be a bijection",
			s.table, column, role, self,
		))
	}
	if self != "PK" && column == s.pkColumn {
		collide("PK")
	}
	if _, dup := s.byCol[column]; dup {
		collide("a mapped field")
	}
	if self != "SoftDelete" && column == s.softDelete {
		collide("SoftDelete")
	}
	if self != "CreatedAt" && column == s.createdAt {
		collide("CreatedAt")
	}
	if self != "UpdatedAt" && column == s.updatedAt {
		collide("UpdatedAt")
	}
}

// pkGoField is the fixed Go-side name of every primary key. Identity is locked
// by the domain.Entity/BaseEntity contract — an aggregate root carries it
// privately in BaseEntity and exposes it via GetID/SetID, while an
// AggregateValueObject exposes the exported field "ID" — so the Go side is
// never a free parameter. Only the physical column varies.
const pkGoField = "ID"

// PK declares the primary-key column — mandatory, no default. The Go side is
// fixed to the BaseEntity/AVO field "ID" (the Entity contract locks identity),
// so only the physical column is declared here — it is what varies (id,
// person_pk, an upstream schema's own name). Single-column. Empty column is
// rejected.
func (s *TableSchema) PK(column string) *TableSchema {
	if column == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): PK requires a non-empty column — "+
				"a single-column primary key is mandatory on every schema",
			s.table,
		))
	}
	s.ensureColumnFree(column, "PK")
	s.pkGo = pkGoField
	s.pkColumn = column
	if s.typ != nil {
		s.pkIndex = exportedFieldIndex(s.typ, pkGoField)
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
//
// The optional trailing labelKey declares the field's header catalog key inline
// on the schema. It is EXTERNAL-ONLY: passing a non-empty labelKey on a
// type-anchored schema (NewTableSchema[T]) is a boot panic, because that schema
// already declares the label via the field's labelKey:"…" struct tag — there is
// never two ways to express one domain concept. A type-less external schema
// (NewExternalSchema) has no struct to read, so the schema-level labelKey is the
// only place it can live (the "mini-domain" for upstream columns). At most one
// labelKey may be passed.
func (s *TableSchema) Field(goName, column string, labelKey ...string) *TableSchema {
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
	lk := ""
	switch len(labelKey) {
	case 0:
	case 1:
		lk = labelKey[0]
	default:
		panic(fmt.Sprintf("infra.TableSchema(%s): field %q accepts at most one labelKey", s.table, goName))
	}
	if lk != "" && s.typ != nil {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): schema-level labelKey on field %q is external-only; a type-anchored schema declares the header via the `labelKey:\"…\"` struct tag, not here",
			s.table, goName,
		))
	}
	fd := schemaField{goName: goName, column: column, index: idx, labelKey: lk}
	s.fields = append(s.fields, fd)
	s.byGo[goName] = fd
	s.byCol[column] = fd
	return s
}

// SoftDelete enables the soft-delete predicate on col (read scope-gate +
// archive/unarchive SQL). Omitting it disables soft-delete: Archive/Unarchive
// are unavailable and the read gate is never applied.
func (s *TableSchema) SoftDelete(col string) *TableSchema {
	s.ensureColumnFree(col, "SoftDelete")
	s.softDelete = col
	return s
}

// CreatedAt enables a framework-stamped created_at column: the framework writes
// col = NOW() on INSERT (it never relies on a DB DEFAULT it does not own).
func (s *TableSchema) CreatedAt(col string) *TableSchema {
	s.ensureColumnFree(col, "CreatedAt")
	s.createdAt = col
	return s
}

// UpdatedAt enables a framework-stamped updated_at column: col = NOW() on
// INSERT and UPDATE.
func (s *TableSchema) UpdatedAt(col string) *TableSchema {
	s.ensureColumnFree(col, "UpdatedAt")
	s.updatedAt = col
	return s
}

// Child registers an aggregate child's schema, keyed by the child Go type name.
// An aggregate child MUST declare its foreign key to the root via .FK(col) — the
// persister injects the root id into that column on every child write — so a
// child without an FK is rejected here at construction.
func (s *TableSchema) Child(child *TableSchema) *TableSchema {
	if child == nil || child.typ == nil {
		panic(fmt.Sprintf("infra.TableSchema(%s): Child requires a type-anchored schema", s.table))
	}
	if !child.hasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q declares no primary key — declare .PK(column) "+
				"(there is no default; every schema must declare its PK)",
			s.table, child.typ.Name(),
		))
	}
	if child.fkColumn == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): aggregate child %q declares no foreign key — declare .FK(col) on its "+
				"schema; the persister injects the root id into that column on every child write",
			s.table, child.typ.Name(),
		))
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

// isExternal reports whether the schema is type-less (built via
// NewExternalSchema, no Go struct anchor). A view embed's source kind is derived
// from this: an external schema describes an upstream Mongo collection
// (FromSchema → isMongo), a type-anchored schema a local PG table.
func (s *TableSchema) isExternal() bool { return s.typ == nil }

// typeName returns the schema's Go type name ("Address"), or "" for a type-less
// external schema. A local view embed derives its Go segment from this; an
// external embed has none, so it must declare the segment via .As(...).
func (s *TableSchema) typeName() string {
	if s.typ == nil {
		return ""
	}
	return s.typ.Name()
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

// Exported write-path accessors. A relational engine living in its own package
// (the MySQL engine under its build tag) builds INSERT/UPDATE statements from a
// TableSchema; these thin wrappers expose the column → value map and the managed
// NOW() columns + soft-delete column it needs, without widening the surface the
// in-package Postgres path consumes (which keeps using the unexported forms).

// WriteFields is the exported form of writeFields — the column → value map an
// engine binds for INSERT/UPDATE (PK and managed NOW() columns excluded).
func (s *TableSchema) WriteFields(e any) domain.Fields { return s.writeFields(e) }

// InsertNowColumns is the exported form of insertNowColumns.
func (s *TableSchema) InsertNowColumns() []string { return s.insertNowColumns() }

// UpdateNowColumns is the exported form of updateNowColumns.
func (s *TableSchema) UpdateNowColumns() []string { return s.updateNowColumns() }

// SoftDeleteColumn is the exported form of softDeleteColumn — the soft-delete
// column and whether it was declared (engines gate archive/unarchive on it).
func (s *TableSchema) SoftDeleteColumn() (string, bool) { return s.softDeleteColumn() }

// ChildSchema is the exported form of childSchema — the declared child schema for
// an aggregate child type name (nil when undeclared). An out-of-package engine's
// aggregate persister resolves child tables/columns/FK through it.
func (s *TableSchema) ChildSchema(typeName string) *TableSchema { return s.childSchema(typeName) }

// BoolColumns returns the set of physical columns whose mapped Go field is a bool
// (or *bool). The composer uses it to restore type fidelity on a backend that
// cannot carry a native boolean: MySQL stores BOOL/BOOLEAN as TINYINT(1) and the
// dynamic read yields int64(0/1), so a bool field would compose into Mongo as a
// number. Empty for an external/type-less schema (no Go struct to reflect).
func (s *TableSchema) BoolColumns() map[string]bool {
	if s.typ == nil {
		return nil
	}
	out := make(map[string]bool)
	for _, f := range s.fields {
		if f.index < 0 {
			continue
		}
		ft := s.typ.Field(f.index).Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Bool {
			out[f.column] = true
		}
	}
	return out
}

// ScanPlan returns the SELECT columns (in field order) + a column → reflect
// field-index map for the scanner. The PK column is included only when the PK is
// an exported struct field (aggregate child); for the root (pkIndex < 0) the PK
// is the leading key handled by ScanLeadingKey, not a struct field.
func (s *TableSchema) ScanPlan() (cols []string, byCol map[string]int) {
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

// FieldResolver maps a Go field name to its SQL column. The criteria translator
// builds one from the entity's TableSchema (TableSchema.FieldResolver); ok=false
// for an unknown / non-persisted field → the translator fails fast (developer
// bug). The type lives here (the schema foundation) rather than with the
// translator so both the schema and the translator can name it without a cycle.
type FieldResolver func(goField string) (column string, ok bool)

// FieldResolver returns a criteria field resolver (Go field → column) backed by
// the schema. Unknown fields resolve to ok=false (the translator rejects them).
func (s *TableSchema) FieldResolver() FieldResolver {
	return func(goField string) (string, bool) {
		return s.ColumnOf(goField)
	}
}

// ValidateChildDepth panics when any declared aggregate child carries its own
// Child(...) — i.e. a grandchild. Aggregate persistence is root + exactly one
// level of children (insertChildren/applyChildChanges/the cascade all iterate
// root.AllAggregateItems() once and never recurse into a child's children), so a
// grandchild declared on the schema would silently never persist. Turn that
// no-op into a loud boot failure that names the alternative.
func (s *TableSchema) ValidateChildDepth() {
	for _, child := range s.children {
		if len(child.children) > 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): aggregate child %q declares its own Child(...) — "+
					"grandchildren are NOT supported by aggregate persistence (an aggregate is its "+
					"root plus exactly one level of children, persisted in a single transaction). "+
					"Model the sub-collection as a SEPARATE aggregate with its own root table + FK.",
				s.table, child.typ.Name(),
			))
		}
	}
}

// ValidateModes panics when the entity declares an archive verb but soft-delete
// is disabled — turning a runtime SQL error into a loud boot failure.
func (s *TableSchema) ValidateModes(modes []domain.EntityMode) {
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
