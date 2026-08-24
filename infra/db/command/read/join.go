package read

import (
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// A JOIN here is a READ-ONLY traversal from an aggregate to another one,
// declared on the repository — never on the TableSchema and never on a view.
//
// The TableSchema declares the form of ONE entity: its columns, its id, its
// managed slots and its own closure (children, siblings, shared base). A foreign
// key to ANOTHER aggregate is already an ordinary column of that form; what a
// join adds is permission to REACH ACROSS it in a query. That is access, not
// storage, and the repository is where a developer would write the join by hand.
//
// Declaring it here rather than on the schema also keeps it invisible to the
// projection side: the TableSchema is shared with the Mongo composer, and a
// declaration one engine honors while the other silently ignores is the failure
// mode this framework spent a refactor removing.
//
// Read-only is STRUCTURAL, not a convention. TableSchema.WriteFields walks the
// schema's own fields, so a join field can never enter an INSERT or UPDATE; and
// the write repository holds a TableSchema with no loader in sight, so a write
// has no path to a join at all.

// JoinKind is how a declared traversal treats a joining row with no counterpart.
type JoinKind uint8

const (
	// JoinLeft PRESERVES the root: an aggregate with no match is still loaded and
	// the traversed fields read as NULL. It is what a load should use by default —
	// FindOne and FindAll exist to return THIS aggregate, and a missing relation is
	// not a reason to stop returning it.
	JoinLeft JoinKind = iota
	// JoinInner REQUIRES the match: the row with no counterpart is not returned —
	// the ROOT itself for a root join, and for a child join the CHILD ELEMENT
	// (the root still comes back, minus that element). Because the declaration
	// lives on the repository, that applies to EVERY read through this loader —
	// FindByID included, which is what the write-side Auto handlers load through.
	// It is therefore legal only over a NON-NULLABLE foreign key, where
	// referential integrity makes it equivalent to a left join on the same row
	// set; over a nullable one it would drop rows in silence.
	JoinInner
)

func (k JoinKind) String() string {
	if k == JoinInner {
		return "InnerJoin"
	}
	return "LeftJoin"
}

// JoinField is one column of the joined table mapped onto a Go field of the
// entity that declares the join — same (goField, column) shape, same order, as
// TableSchema.Field.
type JoinField struct {
	GoField string
	Column  string
}

// Join is one declared traversal. The predicate is always the joining table's
// FKColumn against the TARGET's declared id column: a foreign key points at a
// primary key, and the schema already names both, so nothing else has to be
// declared — and a traversal onto a NON-id column of the target is deliberately
// not expressible.
//
// A ROOT join is ALWAYS in the FROM, and its columns ride the root SELECT — so
// its values cost no second round trip, and the field is trustworthy: one that
// appeared only when some filter happened to mention it would leave the same
// entity populated on one call and blank on the next. The cost is a real join on
// every read through this loader, FindByID included; declare one because the
// aggregate genuinely reads that way, not "just in case".
//
// A CHILD join rides the child's own batched SELECT, on the same terms.
//
// Neither is gated on the archived state of the TARGET. A join answers "what is
// on the other side of this foreign key", and a soft-deleted counterpart is
// still what the key points at — the read scope governs the roots this loader
// returns, never the rows it reaches across into.
type Join struct {
	Kind JoinKind
	// Child is the aggregate child this join hangs off, or nil when it hangs off
	// the root. It must be one of the root schema's OWN children.
	Child *core.TableSchema
	// Target is the joined aggregate's schema.
	Target *core.TableSchema
	// FKColumn is the foreign key ON THE JOINING TABLE — the root's when Child is
	// nil, the child's otherwise.
	FKColumn string
	// Fields is what the traversal brings back. At least one is mandatory: a join
	// that maps no field reaches nothing.
	Fields []JoinField
}

// InnerJoin declares a traversal from the ROOT that requires the match. Its
// fields are addressable in a criteria (filter and order) AND loaded.
func InnerJoin(target *core.TableSchema) *JoinBinding {
	return newBinding(JoinInner, nil, target)
}

// LeftJoin declares a traversal from the ROOT that preserves the root. Its
// fields are addressable in a criteria (filter and order) AND loaded.
func LeftJoin(target *core.TableSchema) *JoinBinding {
	return newBinding(JoinLeft, nil, target)
}

// InnerJoinInChild declares a traversal from one of the root's OWN aggregate
// children. Child joins are LOAD-ONLY: filtering the root by a field of a 1:N
// child is a pushdown a single root SELECT cannot express, which is the boundary
// the relational read already refuses.
func InnerJoinInChild(child *core.TableSchema) *ChildJoinBinding {
	return &ChildJoinBinding{kind: JoinInner, child: child}
}

// LeftJoinInChild is the root-preserving twin of InnerJoinInChild.
func LeftJoinInChild(child *core.TableSchema) *ChildJoinBinding {
	return &ChildJoinBinding{kind: JoinLeft, child: child}
}

// ChildJoinBinding is the half-declared child join: the verb already named the
// CHILD, so To names the target. The two schemas sit behind different verbs on
// purpose — two positional *TableSchema arguments would be swappable without a
// compile error.
type ChildJoinBinding struct {
	kind  JoinKind
	child *core.TableSchema
}

// To names the aggregate this child traverses to.
func (b *ChildJoinBinding) To(target *core.TableSchema) *JoinBinding {
	if b.child == nil {
		panic(fmt.Sprintf("read.%sInChild(nil): the child schema is mandatory", b.kind))
	}
	return newBinding(b.kind, b.child, target)
}

// JoinBinding accumulates one declaration until WithJoins consumes it.
type JoinBinding struct{ j Join }

func newBinding(kind JoinKind, child, target *core.TableSchema) *JoinBinding {
	if target == nil {
		panic(fmt.Sprintf("read.%s(nil): the target schema is mandatory", kind))
	}
	return &JoinBinding{j: Join{Kind: kind, Child: child, Target: target}}
}

// On names the foreign key column on the JOINING table: the root's when the join
// hangs off the root, the child's when it hangs off a child.
func (b *JoinBinding) On(fkColumn string) *JoinBinding {
	if fkColumn == "" {
		panic(fmt.Sprintf("read.%s(%q).On(\"\"): the foreign key column is mandatory", b.j.Kind, b.j.Target.Table()))
	}
	b.j.FKColumn = fkColumn
	return b
}

// Field maps ONE column of the joined table onto a Go field of the entity that
// declares the join — the same (goField, column) order TableSchema.Field uses.
// The Go name is yours: the joined side's spelling never surfaces above infra.
//
// Call it once per column. A join that maps no field is rejected at construction:
// it would emit a SQL JOIN reaching nothing. So is a Go field that two mappings
// claim — here or across two traversals of the same owner — since both columns
// would scan into the one field and the second would win in silence.
func (b *JoinBinding) Field(goField, column string) *JoinBinding {
	if goField == "" || column == "" {
		panic(fmt.Sprintf(
			"read.%s(%q).Field(%q, %q): both the Go field and the column are mandatory",
			b.j.Kind, b.j.Target.Table(), goField, column))
	}
	b.j.Fields = append(b.j.Fields, JoinField{GoField: goField, Column: column})
	return b
}

// build finalizes the declaration, validating everything that can be known from
// the join alone. What needs the OWNING schema — the child membership, the FK
// column, the Go field's existence and nullability — is checked by validateJoins,
// which the loader runs once it has both.
func (b *JoinBinding) build() Join {
	if b.j.FKColumn == "" {
		panic(fmt.Sprintf(
			"read.%s(%q): no .On(...) — name the foreign key column on the joining table",
			b.j.Kind, b.j.Target.Table()))
	}
	if len(b.j.Fields) == 0 {
		panic(fmt.Sprintf(
			"read.%s(%q): no .Field(...) — a join that maps no column reaches nothing; "+
				"declare at least one Field(goField, column)",
			b.j.Kind, b.j.Target.Table()))
	}
	return b.j
}

// validateJoins asserts every declaration against the schema that owns it AND
// against the other declarations. It runs at construction — a violation panics
// there, never on the first request.
//
// It receives the WHOLE set, which is what WithJoins' once-only rule exists to
// guarantee: two of these checks are about a COLLISION between traversals — one
// foreign key reaches one table (the SQL alias is derived from it), one Go field
// receives one column (both would scan into the same address) — and neither can
// be answered from one declaration in isolation.
//
// root is the schema the repository declared; the owner's Go type is used to
// prove each mapped field exists and, for a left join, that it can hold the NULL
// a missing counterpart produces.
func validateJoins(contextName string, root *core.TableSchema, joins []Join) {
	fail := func(format string, args ...any) {
		panic(fmt.Sprintf("read.WithJoins[%s]: ", contextName) + fmt.Sprintf(format, args...))
	}
	if root == nil {
		fail("no schema declared — call WithSchema(...) before WithJoins(...)")
	}

	ownChildren := map[string]*core.TableSchema{}
	for _, ch := range root.ChildSchemas() {
		ownChildren[ch.Table()] = ch
	}
	// One join per foreign key per owner. The FK is what the SQL alias is derived
	// from — two traversals may reach the SAME table (bill_to and ship_to both to
	// customers), and the FK is what tells them apart. Two joins sharing one FK
	// would collide on that alias, and a column pointing at two tables is a
	// modelling mistake worth naming rather than aliasing around.
	fkSeen := map[string]string{}
	// One Go field per owner receives ONE column. Two traversals mapping the same
	// field is the quietest mistake in this file: the SELECT carries both columns
	// (joinSelectExprs), the scan builds two destinations at the SAME struct
	// address (joinScanTargetsFor), so the later column overwrites the earlier
	// one on every row — and a criteria naming that field binds to whichever join
	// was declared first, which may be the one whose value was overwritten. The
	// key is the OWNER's table because a root join and a child join land on
	// different structs: the same Go name there is two different fields.
	goSeen := map[string]string{}

	for _, j := range joins {
		owner := root
		if j.Child != nil {
			ch, ok := ownChildren[j.Child.Table()]
			if !ok {
				fail("%s in child %q: %q is not a child of %q — only a schema declared via "+
					"root.Child(...) can carry a join. A child of a SHARED BASE belongs to the base: "+
					"declare the join on the base's own repository.",
					j.Kind, j.Child.Table(), j.Child.Table(), root.Table())
			}
			owner = ch
		}

		if _, ok := owner.GoNameForRead(j.FKColumn); !ok {
			fail("%s(%q).On(%q): %q is not a column of %q",
				j.Kind, j.Target.Table(), j.FKColumn, j.FKColumn, owner.Table())
		}

		fkKey := owner.Table() + "." + j.FKColumn
		if prev, dup := fkSeen[fkKey]; dup {
			fail("%s(%q).On(%q): %q already carries the join to %q — one foreign key reaches "+
				"ONE table. Two traversals to the same table need two foreign keys (bill_to_id, "+
				"ship_to_id), which is also what tells their SQL aliases apart.",
				j.Kind, j.Target.Table(), j.FKColumn, fkKey, prev)
		}
		fkSeen[fkKey] = j.Target.Table()

		// An inner join drops roots with no counterpart. Over a NON-NULLABLE key
		// referential integrity means there is always one, so the choice is intent
		// and plan; over a nullable key it would silently drop aggregates — from
		// FindByID too, which the write-side handlers load through.
		if j.Kind == JoinInner {
			if fkGo, ok := owner.GoNameForRead(j.FKColumn); ok && owner.IDKindOf(fkGo) == core.IDPointer {
				fail("%s(%q).On(%q): the foreign key is nullable (%s is a *domain.ID), so an inner "+
					"join would silently drop every %s with no %s — use LeftJoin instead.",
					j.Kind, j.Target.Table(), j.FKColumn, fkGo, owner.Table(), j.Target.Table())
			}
		}

		ownerType := owner.GoType()
		for _, f := range j.Fields {
			if _, ok := j.Target.GoNameForRead(f.Column); !ok {
				fail("%s(%q).Field(%q, %q): %q is not a column of %q",
					j.Kind, j.Target.Table(), f.GoField, f.Column, f.Column, j.Target.Table())
			}
			if _, taken := owner.Resolve(f.GoField); taken {
				fail("%s(%q).Field(%q, ...): %q already resolves on %q — a join field must not "+
					"shadow the entity's own field, a sibling's or the shared base's",
					j.Kind, j.Target.Table(), f.GoField, f.GoField, owner.Table())
			}
			goKey := owner.Table() + "." + f.GoField
			if prev, dup := goSeen[goKey]; dup {
				fail("%s(%q).Field(%q, %q): %s.%s is already mapped by %s — one Go field receives "+
					"ONE column. Both would be selected and both would scan into the same field, so "+
					"the second silently overwrites the first on every row. Give the second traversal "+
					"its own field.",
					j.Kind, j.Target.Table(), f.GoField, f.Column, owner.Table(), f.GoField, prev)
			}
			goSeen[goKey] = fmt.Sprintf("the join to %q on column %q", j.Target.Table(), f.Column)
			validateJoinFieldType(fail, j, ownerType, f)
		}
	}
}

// validateJoinFieldType proves the mapped Go field exists on the owning struct,
// is exported, carries no DOMAIN type, and — for a left join — can hold the NULL
// a missing counterpart produces. A non-nullable field there would receive its
// zero value and report a blank name where the truth is "there is no
// counterpart".
func validateJoinFieldType(fail func(string, ...any), j Join, ownerType reflect.Type, f JoinField) {
	if ownerType == nil {
		return // type-less schema: nothing to prove the field against
	}
	sf, ok := ownerType.FieldByName(f.GoField)
	if !ok {
		fail("%s(%q).Field(%q, ...): %s has no field %q — the join lands its value there, "+
			"so the field must exist and be exported",
			j.Kind, j.Target.Table(), f.GoField, ownerType.Name(), f.GoField)
	}
	if sf.PkgPath != "" {
		fail("%s(%q).Field(%q, ...): %s.%s is unexported — the join cannot write to it",
			j.Kind, j.Target.Table(), f.GoField, ownerType.Name(), f.GoField)
	}
	// A join field may carry NO domain type — not an identity, not a value object
	// of any kind. The complaint comes before the nullability one because it is
	// the more fundamental of the two: a domain-typed field is wrong whichever
	// join declares it.
	if kind, remedy, isDomain := domainTypeOfJoinField(sf.Type); isDomain {
		fail("%s(%q).Field(%q, ...): %s.%s is %s, and a join field carries no domain type. "+
			"The value belongs to ANOTHER aggregate and arrives read-only: it is never written "+
			"through this entity and never validated by this domain, so a domain type here would "+
			"be an instance no rule ever approved. %s",
			j.Kind, j.Target.Table(), f.GoField, ownerType.Name(), f.GoField, kind, remedy)
	}
	// The joined aggregate declares this column as an IDENTITY. Since a join field
	// carries no domain type, the identity has exactly one shape here: the plain
	// string the framework decodes it into. Anything else would receive the
	// dialect's stored form — 16 raw bytes on three of the four engines — so the
	// declaration is checked rather than trusted.
	tgtGoField, tgtType, srcNullable := targetColumnNullability(j, f)
	if idKind := targetIDKindOf(j, f); idKind != core.IDNone {
		wantPtr := srcNullable || j.Kind == JoinLeft
		if !isJoinIDTextField(sf.Type, wantPtr) {
			want, why := "string", "an identity column is read as its canonical text"
			if wantPtr {
				want = "*string"
				why = "the column is nullable, so the absence must be representable"
				if idKind != core.IDPointer {
					why = "a left join produces NULL when there is no counterpart"
				}
			}
			fail("%s(%q).Field(%q, %q): %q is an identity column of %q, so %s.%s must be %s — %s. "+
				"It is %s. A join field carries no value object, domain.ID included, which is why an "+
				"identity arrives as text rather than as its domain type.",
				j.Kind, j.Target.Table(), f.GoField, f.Column, f.Column, j.Target.Table(),
				ownerType.Name(), f.GoField, want, why, sf.Type)
		}
	}
	if j.Kind == JoinLeft && !isNullableKind(sf.Type) {
		fail("%s(%q).Field(%q, ...): %s.%s is %s, which cannot hold the NULL a left join "+
			"produces when there is no counterpart — declare it as a pointer, or use InnerJoin",
			j.Kind, j.Target.Table(), f.GoField, ownerType.Name(), f.GoField, sf.Type)
	}
	// The column may be NULL on its OWN side, independently of the join kind: an
	// inner join proves the joined ROW exists, never that every column of it is
	// filled. The field that receives it must be able to say "absent", or the
	// first row carrying a NULL fails the read.
	if srcNullable && !isNullableKind(sf.Type) {
		fail("%s(%q).Field(%q, %q): %s.%s is %s and cannot hold NULL, but %q is nullable on %q%s "+
			"— the field receiving a nullable column must be a pointer.",
			j.Kind, j.Target.Table(), f.GoField, f.Column,
			ownerType.Name(), f.GoField, sf.Type,
			f.Column, j.Target.Table(), targetDeclaration(j, tgtGoField, tgtType))
	}
}

// targetDeclaration renders the joined side's own declaration for an error
// message — "(Campus.OwnerName is *string)" — or nothing when the column is not
// a field of the target's struct.
func targetDeclaration(j Join, goField string, typ reflect.Type) string {
	if typ == nil || j.Target == nil || j.Target.GoType() == nil {
		return ""
	}
	return fmt.Sprintf(" (%s.%s is %s)", j.Target.GoType().Name(), goField, typ)
}

// domainTypeOfJoinField names the domain typing of a join field's Go type and
// the remedy for it, or reports false for an ordinary one. The four kinds are
// separated because the fix differs: a value object of any kind is replaced by
// the scalar it is stored as, while an identity has no honest scalar on every
// engine and the message must say so rather than send the developer at a string
// that silently reads as bytes.
//
// domain.ID is checked FIRST because it satisfies the value-object contract
// (Value() plus IsValid) and would otherwise be named a scalar value object —
// the same special case core.valueObjectField makes for the same reason.
func domainTypeOfJoinField(t reflect.Type) (kind, remedy string, isDomain bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(domain.ID{}) {
		return "an identity (domain.ID)", idTextRemedy, true
	}
	zero := reflect.Zero(t).Interface()
	if domain.IsEnumValueObject(zero) {
		return "an enum value object", scalarRemedy, true
	}
	if !domain.IsValueObject(zero) {
		return "", "", false
	}
	// Value() is what tells a scalar-backed value object from a composite one:
	// a composite spans several columns and declares none.
	if _, hasValue := domain.ValueObjectValue(zero); hasValue {
		return "a scalar value object", scalarRemedy, true
	}
	return "a composite value object",
		"A composite value object spans SEVERAL columns and a join field maps exactly one, " +
			"so it has no form here at all. Map the parts the traversal actually needs, each as " +
			"its own scalar field.",
		true
}

const idTextRemedy = "Declare the field as a plain string: an identity column is read as its " +
	"canonical text, and the framework decodes the dialect's stored form (BINARY(16) on mysql and " +
	"sqlserver, RAW(16) on oracle) into it for you."

const scalarRemedy = "Declare the field as the scalar the column is stored as (string, int64, …) — " +
	"a join brings back the COLUMN, and the domain type is the owning aggregate's to reconstruct."

// targetIDKindOf reports how the JOINED aggregate types the column this field
// maps: IDValue for a domain.ID field of the target, IDPointer for a *domain.ID
// one, IDNone for an ordinary column. The target's schema is the declaration —
// the same source the mapped-field scan plan consults — so the two paths cannot
// disagree about what an identity is.
func targetIDKindOf(j Join, f JoinField) core.IDKind {
	if j.Target == nil {
		return core.IDNone
	}
	goName, ok := j.Target.GoNameForRead(f.Column)
	if !ok {
		return core.IDNone // already reported by the column check
	}
	return j.Target.IDKindOf(goName)
}

// targetColumnNullability reports whether the column this field maps is declared
// NULLABLE by the JOINED aggregate, along with that side's own Go field and type
// for the error message. The declaration is the target's Go type — a pointer
// field is the nullable one, the same rule the framework reads everywhere else —
// and it is reachable because the join declaration HOLDS the target's schema,
// which is type-anchored (`NewTableSchema[T]`). It is the aggregate's declared
// shape, not the database catalog: a column left NULL-able in DDL while the
// owning entity declares a non-pointer is that entity's own mis-declaration, and
// no join can see it.
//
// A column the target's struct does not expose — a managed slot, or a type-less
// external schema — answers NOT nullable except where the schema itself recorded
// the identity typing. The framework enforces only what it can point at.
func targetColumnNullability(j Join, f JoinField) (goField string, typ reflect.Type, nullable bool) {
	if j.Target == nil {
		return "", nil, false
	}
	goName, ok := j.Target.GoNameForRead(f.Column)
	if !ok {
		return "", nil, false // already reported by the column check
	}
	ty := j.Target.GoType()
	if ty == nil {
		return goName, nil, false
	}
	sf, ok := ty.FieldByName(goName)
	if !ok {
		return goName, nil, j.Target.IDKindOf(goName) == core.IDPointer
	}
	return goName, sf.Type, sf.Type.Kind() == reflect.Pointer
}

// isJoinIDTextField reports whether t is the string shape an identity column
// lands in: *string when the value may be absent, string when it may not.
func isJoinIDTextField(t reflect.Type, wantPtr bool) bool {
	if wantPtr {
		return t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.String && t.Elem().PkgPath() == ""
	}
	return t.Kind() == reflect.String && t.PkgPath() == ""
}

func isNullableKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	default:
		return false
	}
}
