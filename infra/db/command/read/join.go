package read

import (
	"fmt"
	"reflect"

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

// JoinKind is how a declared traversal treats a root with no counterpart.
type JoinKind uint8

const (
	// JoinLeft PRESERVES the root: an aggregate with no match is still loaded and
	// the traversed fields read as NULL. It is what a load should use by default —
	// FindOne and FindAll exist to return THIS aggregate, and a missing relation is
	// not a reason to stop returning it.
	JoinLeft JoinKind = iota
	// JoinInner REQUIRES the match: a root with no counterpart is not returned.
	// Because the declaration lives on the repository, that applies to EVERY read
	// through this loader — FindByID included, which is what the write-side Auto
	// handlers load through. It is therefore legal only over a NON-NULLABLE foreign
	// key, where referential integrity makes it equivalent to a left join on the
	// root set; over a nullable one it would drop aggregates in silence.
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

// Join is one declared traversal. It is INERT until a criteria names one of its
// fields (root joins) or the aggregate is loaded (child joins): no SQL JOIN is
// emitted for a traversal nobody used, so declaring one costs nothing per read.
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
// it would emit a SQL JOIN reaching nothing.
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

// validateJoins asserts every declaration against the schema that owns it. It
// runs at construction — a violation panics there, never on the first request.
//
// root is the schema the repository declared; goType is the root's Go type, used
// to prove each mapped field exists and, for a left join, that it can hold the
// NULL a missing counterpart produces.
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
			validateJoinFieldType(fail, j, ownerType, f)
		}
	}
}

// validateJoinFieldType proves the mapped Go field exists on the owning struct,
// is exported, and — for a left join — can hold the NULL a missing counterpart
// produces. A non-nullable field there would receive its zero value and report a
// blank name where the truth is "there is no counterpart".
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
	if j.Kind == JoinLeft && !isNullableKind(sf.Type) {
		fail("%s(%q).Field(%q, ...): %s.%s is %s, which cannot hold the NULL a left join "+
			"produces when there is no counterpart — declare it as a pointer, or use InnerJoin",
			j.Kind, j.Target.Table(), f.GoField, ownerType.Name(), f.GoField, sf.Type)
	}
}

func isNullableKind(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
		return true
	default:
		return false
	}
}
