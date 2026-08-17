package core

import (
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Composite value objects — the schema-side decomposition.
//
// A value object whose value spans MORE THAN ONE field (Address{Street, City},
// Money{Amount, Currency}, Period{From, To}) cannot be a single column. The
// TableSchema is the only place that knows it is stored across N of them: the
// domain declares a plain struct that owns its rule (IsValid) and nothing else,
// and every consumer downstream — criteria, scan, audit, the Mongo projection,
// the read DTO, every surface — sees the parts as if they had been flattened
// onto the entity by hand. The composite exists in the domain and evaporates
// here.
//
// It is declared the way every other sub-object of a schema is: a constructor
// builds the whole thing with its own Field chain, and the owner attaches it —
// exactly like Sibling(NewSiblingSchema[T](…).Field(…)).
//
//	NewTableSchema[Person]("persons").
//	    ID("id").
//	    Field("Name", "name").
//	    Composite(core.NewCompositeValueObject[vos.Address]().
//	        Field("Street",  "street").
//	        Field("City",    "city").
//	        Field("ZipCode", "zip_code")).
//	    CreatedAt("created_at")

// CompositeValueObject is the declaration of ONE composite value object's
// decomposition: which of its fields are persisted, into which columns, and
// under which logical name each is exposed. Build it with
// NewCompositeValueObject and hand it to TableSchema.Composite.
type CompositeValueObject struct {
	typ   reflect.Type
	parts []compositePartDecl
}

type compositePartDecl struct {
	voField string // the field's name INSIDE the value object
	column  string
	exposed string // the logical name everything downstream sees (As, or voField)
}

// NewCompositeValueObject starts the decomposition of the composite value
// object VO — a value object with no Value() of its own, whose value therefore
// occupies more than one column. The type is a parameter rather than a value,
// so the declaration is compile-time checked and reads as the concept it names.
//
// The kind is verified HERE, at the call site that names the type: passing a
// scalar or enum value object (one that declares Value(), and therefore
// occupies exactly one column) fails pointing at this line, not later at the
// schema that attached it.
func NewCompositeValueObject[VO any]() *CompositeValueObject {
	t := reflect.TypeOf((*VO)(nil)).Elem()
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	mustBeCompositeValueObject(t)
	return &CompositeValueObject{typ: t}
}

// Field declares one field of the value object ↔ its column. goName is the
// field's name INSIDE the value object; it is also the logical name the part is
// exposed under to everything downstream (criteria, audit, the projection, the
// read DTO) unless As(...) renames it.
func (c *CompositeValueObject) Field(goName, column string) *CompositeValueObject {
	if exportedFieldIndex(c.typ, goName) < 0 {
		panic(fmt.Sprintf(
			"infra.CompositeValueObject(%s): %q is not an exported single-depth field of the value object — "+
				"a part names a field of %s itself.", c.typ, goName, c.typ))
	}
	for _, p := range c.parts {
		if p.voField == goName {
			panic(fmt.Sprintf("infra.CompositeValueObject(%s): field %q declared twice", c.typ, goName))
		}
		if p.column == column {
			panic(fmt.Sprintf(
				"infra.CompositeValueObject(%s): column %q claimed by more than one field — the map must be a bijection",
				c.typ, column))
		}
	}
	c.parts = append(c.parts, compositePartDecl{voField: goName, column: column, exposed: goName})
	return c
}

// As renames the part declared immediately before it — the logical name the
// part is EXPOSED under, everywhere outside the domain. The default is the
// part's own name inside the value object, which reads right when the value
// object is specific (Address{Street, City} → street, city) and wrong when it is
// generic: Money{Amount, Currency} on a Salary field would expose "Amount", and
// an entity carrying both a Money and a Discount would collide on it with no
// way out, since a part's name belongs to the value object and not to the
// consumer.
//
// The alias moves the exposed name ONLY. The labelKey still comes from the tag
// inside the value object: the value object owns its own vocabulary.
func (c *CompositeValueObject) As(exposedName string) *CompositeValueObject {
	if len(c.parts) == 0 {
		panic(fmt.Sprintf(
			"infra.CompositeValueObject(%s): As(%q) does not follow a Field(...) — an alias renames the part "+
				"declared immediately before it.", c.typ, exposedName))
	}
	if exposedName == "" {
		panic(fmt.Sprintf("infra.CompositeValueObject(%s): As(\"\") — an alias needs a name.", c.typ))
	}
	last := len(c.parts) - 1
	for i, p := range c.parts {
		if i != last && p.exposed == exposedName {
			panic(fmt.Sprintf("infra.CompositeValueObject(%s): part %q declared twice", c.typ, exposedName))
		}
	}
	c.parts[last].exposed = exposedName
	return c
}

// compositeDecl is one ATTACHED decomposition as the schema keeps it: the value
// object's type and the path to the entity field holding it. The cross-schema
// once-rule checkpoint reads this.
type compositeDecl struct {
	typ    reflect.Type // the value object's type (never a pointer)
	prefix FieldPath    // path to the entity field; empty on a type-less schema
}

// Composite attaches a composite value object's decomposition to this schema.
// The entity field is located BY TYPE — the framework finds the field of the
// anchored struct typed vos.Address (or *vos.Address, the optional form) — and
// each declared part becomes an ordinary mapped field under its exposed logical
// name, which is what makes the composite invisible to every consumer
// downstream.
func (s *TableSchema) Composite(c *CompositeValueObject) *TableSchema {
	if c == nil {
		panic(fmt.Sprintf("infra.TableSchema(%s): Composite(nil) — build the declaration with core.NewCompositeValueObject[VO]().", s.table))
	}
	if len(c.parts) == 0 {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): the composite value object %s declares no Field(...) — a decomposition with no "+
				"parts persists nothing. Declare the parts, or drop the call.", s.table, c.typ))
	}
	// A shared base is type-less too, but it IS a local materialization: its parts
	// resolve against each role's struct at .SharedBase(...) time, exactly as its
	// scalar fields already do. Only the upstream-owned external schema is out.
	if s.typ == nil && !s.isSharedBase {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): Composite(%s) on an external schema — NewExternalSchema describes an "+
				"UPSTREAM service's columns, not a local materialization, and has no Go struct to decompose against. "+
				"Declare the upstream's columns as plain Fields.", s.table, c.typ))
	}
	// The once rule: each composite value object type appears EXACTLY ONCE in an
	// entity's schema graph. This is the same-schema half (the cross-schema half —
	// root vs sibling vs shared base — is checked at the boot checkpoint, so
	// declaration order cannot matter).
	for _, existing := range s.composites {
		if existing.typ == c.typ {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): the composite value object %s is decomposed twice on this schema — each "+
					"composite type is declared exactly ONCE per entity (its parts all live in one table). Merge the "+
					"two Composite(...) declarations into one.", s.table, c.typ))
		}
	}
	decl := &compositeDecl{typ: c.typ}
	if s.typ != nil {
		decl.prefix = FieldPath{s.mustLocateCompositeField(c.typ)}
	}
	for _, p := range c.parts {
		s.mustClaimNames(p.exposed, p.column, "value-object part")
		fd := schemaField{
			goName:      p.exposed,
			column:      p.column,
			voType:      c.typ,
			voFieldName: p.voField,
		}
		if decl.prefix.resolved() {
			fd.path = append(append(FieldPath{}, decl.prefix...), exportedFieldIndex(c.typ, p.voField))
		}
		fd.applyTyping(s.table, p.exposed, c.typ.Field(exportedFieldIndex(c.typ, p.voField)).Type)
		s.appendField(fd)
	}
	s.composites = append(s.composites, decl)
	return s
}

// mustBeCompositeValueObject rejects every type that is not a composite value
// object, naming the correct declaration for the ones that are value objects of
// the OTHER kind. The discriminator is the presence of Value(): a value object
// that yields a scalar occupies exactly one column and is declared with Field;
// one that yields none spans several and is decomposed.
func mustBeCompositeValueObject(t reflect.Type) {
	zero := reflect.Zero(t).Interface()
	// The scalar/enum verdict comes FIRST: it is the informative one, and every
	// value object of those kinds is a named type over a scalar, so the struct
	// check below would otherwise answer with a message about the wrong problem.
	if _, hasValue := domain.ValueObjectValue(zero); hasValue {
		kind := "SCALAR"
		if domain.IsEnumValueObject(zero) {
			kind = "ENUM"
		}
		panic(fmt.Sprintf(
			"infra.NewCompositeValueObject[%s]: %s is a %s value object (it declares Value(), so it occupies exactly "+
				"one column). Declare it with Field(\"<field>\", \"<column>\"). A composite value object declares NO "+
				"Value(); expose a canonical rendering under any other name (String(), Format()).",
			t, t, kind))
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf(
			"infra.NewCompositeValueObject[%s]: a composite value object is a STRUCT whose fields are the parts; "+
				"%s is %s.", t, t, t.Kind()))
	}
	if !domain.IsValueObject(zero) {
		hint := ""
		if domain.IsValueObject(reflect.New(t).Interface()) {
			hint = " (it is declared on *" + t.Name() + "; the framework validates the VALUE, so IsValid needs a value receiver)"
		}
		panic(fmt.Sprintf(
			"infra.NewCompositeValueObject[%s]: %s is not a value object: it declares no "+
				"IsValid(fieldName string, ctx *domain.NotificationContext) bool%s. A composite value object owns its "+
				"rule like any other; decomposition is not a way to flatten an arbitrary struct.",
			t, t, hint))
	}
}

// locateCompositeField finds the exported field of t typed voType (or *voType).
// It reports the index and, when a SECOND field of that type exists, its name —
// the ambiguity that makes resolution-by-type impossible.
func locateCompositeField(t, voType reflect.Type) (idx int, dup string) {
	ptr := reflect.PointerTo(voType)
	idx = -1
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() || f.Anonymous {
			continue
		}
		if f.Type != voType && f.Type != ptr {
			continue
		}
		if idx >= 0 {
			return idx, f.Name
		}
		idx = i
	}
	return idx, ""
}

// compositeOrigin renders a field's value-object provenance for a divergence
// message ("-" when the field is not a composite part).
func compositeOrigin(f schemaField) string {
	if f.voType == nil {
		return "-"
	}
	return f.voType.String() + "." + f.voFieldName
}

// mustLocateCompositeField finds the exported field of the anchored struct
// typed voType (or *voType) and returns its index. Absence and ambiguity are
// both boot failures: with resolution by type there is nothing to pick from
// when an entity carries two fields of one composite type, which is the
// declaration-time half of the once rule.
func (s *TableSchema) mustLocateCompositeField(voType reflect.Type) int {
	found, dup := locateCompositeField(s.typ, voType)
	if dup != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s carries more than one field of the composite value object %s (%q and %q) — "+
				"a composite is resolved BY TYPE, so there is nothing to pick from. Each composite type appears "+
				"exactly once per entity; model the second occurrence as its own value-object type.",
			s.table, s.typ.Name(), voType, s.typ.Field(found).Name, dup))
	}
	if found < 0 {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s has no exported field typed %s (or *%s) — Composite(...) resolves the "+
				"entity field BY TYPE.", s.table, s.typ.Name(), voType, voType))
	}
	return found
}

// mustNotCompositeField rejects a composite value object declared as a plain
// Field: its value spans more than one column, so the mapping cannot exist. The
// message teaches the decomposition rather than letting the closed-set guard
// report an unsupported type, which says nothing about the fix.
func mustNotCompositeField(table, goName string, ft reflect.Type) {
	t := ft
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == idType {
		return
	}
	zero := reflect.Zero(t).Interface()
	if _, hasValue := domain.ValueObjectValue(zero); hasValue {
		return // a scalar/enum value object over a struct underlying — Field is right
	}
	if !domain.IsValueObject(zero) {
		return // not a value object at all — the closed-set guard owns the message
	}
	panic(fmt.Sprintf(
		"infra.TableSchema(%s): field %q is typed %s, a COMPOSITE value object (it declares IsValid but no Value(), "+
			"so its value spans more than one column). Decompose it:\n"+
			"    Composite(core.NewCompositeValueObject[%s]().\n"+
			"        Field(\"<field>\", \"<column>\").\n"+
			"        Field(\"<field>\", \"<column>\"))",
		table, goName, t, t))
}

// resolveBaseFieldPath resolves one SHARED-BASE column against this role's Go
// type. The base is type-less — it is shared by several roles — so its fields
// are addressed by name here, at .SharedBase(...) time, which is also where a
// role that forgot one is rejected.
//
// A composite's part cannot be resolved by its exposed name (that name lives on
// the value object, not on the role), so it is resolved by PROVENANCE: find the
// role's field typed like the value object, then the part inside it. The role
// must therefore carry the composite field itself — the exact analogue of
// "a role must carry every shared-base field".
func (s *TableSchema) resolveBaseFieldPath(base *TableSchema, f schemaField) FieldPath {
	if f.voType == nil {
		idx := exportedFieldIndex(s.typ, f.goName)
		if idx < 0 {
			panic(fmt.Sprintf(
				"infra.TableSchema(%s): shared base %q field %q is not an exported field of %s — a role must "+
					"carry every shared-base field.", s.table, base.table, f.goName, s.typ.Name()))
		}
		return FieldPath{idx}
	}
	voIdx, dup := locateCompositeField(s.typ, f.voType)
	if dup != "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): %s carries more than one field of the composite value object %s (%q and %q), "+
				"which shared base %q decomposes — a composite is resolved BY TYPE, so there is nothing to pick from.",
			s.table, s.typ.Name(), f.voType, s.typ.Field(voIdx).Name, dup, base.table))
	}
	if voIdx < 0 {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q decomposes the composite value object %s, but %s has no exported "+
				"field of that type — a role must carry every shared-base field, the composite included.",
			s.table, base.table, f.voType, s.typ.Name()))
	}
	partIdx := exportedFieldIndex(f.voType, f.voFieldName)
	if partIdx < 0 {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): shared base %q maps %s.%s, which is not an exported field of that value object.",
			s.table, base.table, f.voType, f.voFieldName))
	}
	return FieldPath{voIdx, partIdx}
}
