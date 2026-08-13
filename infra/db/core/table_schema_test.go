package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// schemaSample is a type-anchored target for TableSchema construction tests —
// the exported fields exist so Field/ID declarations validate against it.
type schemaSample struct {
	ID      string
	Name    string
	Created string
	Updated string
	Removed string
}

// assertPanics runs fn and fails the test unless it panics.
func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, got none", name)
		}
	}()
	fn()
}

// TestTableSchema_ManagedColumnBijection exercises the order-independent
// collision enforcement over the full physical column set (ID + Field +
// DeletedAt/CreatedAt/UpdatedAt). Each managed setter and ID must reject a
// column already claimed by another slot, regardless of declaration order.
func TestTableSchema_ManagedColumnBijection(t *testing.T) {
	assertPanics(t, "CreatedAt vs UpdatedAt same column", func() {
		NewTableSchema[schemaSample]("t").CreatedAt("ts").UpdatedAt("ts")
	})
	assertPanics(t, "DeletedAt vs CreatedAt same column", func() {
		NewTableSchema[schemaSample]("t").DeletedAt("ts").CreatedAt("ts")
	})
	assertPanics(t, "Field then DeletedAt same column", func() {
		NewTableSchema[schemaSample]("t").Field("Name", "deleted_at").DeletedAt("deleted_at")
	})
	assertPanics(t, "DeletedAt then Field same column", func() {
		NewTableSchema[schemaSample]("t").DeletedAt("deleted_at").Field("Name", "deleted_at")
	})
	assertPanics(t, "Field then CreatedAt same column", func() {
		NewTableSchema[schemaSample]("t").Field("Created", "created_at").CreatedAt("created_at")
	})
	assertPanics(t, "ID column claimed by a field", func() {
		NewTableSchema[schemaSample]("t").Field("Name", "k").ID("k")
	})
	assertPanics(t, "CreatedAt collides with non-default ID", func() {
		NewTableSchema[schemaSample]("t").ID("pk").CreatedAt("pk")
	})
}

// TestTableSchema_FKColumnNotAlsoAField proves the boot guard that forbids
// mapping the aggregate-root ParentID column as a domain Field (either declaration
// order): the write cascade OWNS that column — insertChild sets it to the
// parent id — so a mapped field on it would be silently overwritten on every
// write. The shared-ID model (the ID column that IS the ParentID) stays legitimate,
// because the ID is never a mapped field.
func TestTableSchema_FKColumnNotAlsoAField(t *testing.T) {
	assertPanics(t, "ParentID then Field on the ParentID column", func() {
		NewTableSchema[schemaSample]("child").ID("id").ParentID("root_id").Field("Name", "root_id")
	})
	assertPanics(t, "Field then ParentID on the same column", func() {
		NewTableSchema[schemaSample]("child").ID("id").Field("Name", "root_id").ParentID("root_id")
	})
	// Shared-ID: the ID column doubles as the ParentID (the SharedBase pattern the user
	// relies on). Legitimate — the ID is not in byCol, so the guard must NOT fire.
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("shared-ID (ID == ParentID) must be allowed, got panic: %v", r)
			}
		}()
		s := NewTableSchema[schemaSample]("role").ID("id").ParentID("id")
		if s.IDColumn() != "id" || s.ParentIDColumn() != "id" {
			t.Fatalf("ID/ParentID = %q/%q want id/id", s.IDColumn(), s.ParentIDColumn())
		}
	}()
}

// TestTableSchema_ReservedGoNames_IDandFID proves neither the primary-key Go
// name ("ID") nor the foreign-key projection name ("ParentID") can be declared as a
// Field — "ID" is declared with ID(column), "ParentID" is exposed automatically.
func TestTableSchema_ReservedGoNames_IDandFID(t *testing.T) {
	assertPanics(t, "Field(ID) is reserved (the ID)", func() {
		NewTableSchema[schemaSample]("t").ID("id").Field("ID", "other")
	})
	assertPanics(t, "Field(ParentID) is reserved (the ParentID projection)", func() {
		NewTableSchema[schemaSample]("t").ID("id").Field("ParentID", "other")
	})
}

// TestTableSchema_FIDReadProjection proves the read-only "ParentID" projection of a
// schema's foreign key resolves both ways — for an aggregate child (s.parentIDColumn)
// AND a SharedBase role — and is inert on a schema that declares no ParentID.
func TestTableSchema_FIDReadProjection(t *testing.T) {
	child := NewTableSchema[embedFixture]("lines").ID("id").ParentID("order_id")
	if g, ok := child.GoNameForRead("order_id"); !ok || g != "ParentID" {
		t.Errorf("child GoNameForRead(order_id) = %q,%v want ParentID", g, ok)
	}
	if c, ok := child.ColumnForRead("ParentID"); !ok || c != "order_id" {
		t.Errorf("child ColumnForRead(ParentID) = %q,%v want order_id", c, ok)
	}

	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name")
	role := NewTableSchema[*revRoleEntity]("aluno").ID("id").SharedBase(base, "pessoa_id")
	if g, ok := role.GoNameForRead("pessoa_id"); !ok || g != "ParentID" {
		t.Errorf("role GoNameForRead(pessoa_id) = %q,%v want ParentID", g, ok)
	}
	if c, ok := role.ColumnForRead("ParentID"); !ok || c != "pessoa_id" {
		t.Errorf("role ColumnForRead(ParentID) = %q,%v want pessoa_id", c, ok)
	}

	root := NewTableSchema[schemaSample]("orders").ID("id").Field("Name", "name")
	if _, ok := root.ColumnForRead("ParentID"); ok {
		t.Errorf("schema without an ParentID: ColumnForRead(ParentID) must be false")
	}
}

// TestTableSchema_FKAndSharedBaseMutuallyExclusive proves a schema cannot be both
// an aggregate child (ParentID) and a SharedBase role — that would be two parents and
// an ambiguous ParentID — in either declaration order.
func TestTableSchema_FKAndSharedBaseMutuallyExclusive(t *testing.T) {
	base := func() *TableSchema {
		return NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("name")
	}
	assertPanics(t, "ParentID then SharedBase", func() {
		NewTableSchema[*revRoleEntity]("aluno").ID("id").ParentID("parent_id").SharedBase(base(), "pessoa_id")
	})
	assertPanics(t, "SharedBase then ParentID", func() {
		NewTableSchema[*revRoleEntity]("aluno").ID("id").SharedBase(base(), "pessoa_id").ParentID("parent_id")
	})
}

// TestTableSchema_SingleDeclaration proves every single-column slot rejects a
// SECOND declaration — otherwise the second call silently overwrites the first
// (ensureColumnFree deliberately skips a slot's own value, and some setters
// never checked at all).
func TestTableSchema_SingleDeclaration(t *testing.T) {
	assertPanics(t, "ID twice", func() {
		NewTableSchema[schemaSample]("t").ID("id").ID("other")
	})
	assertPanics(t, "ParentID twice", func() {
		NewTableSchema[embedFixture]("c").ID("id").ParentID("a").ParentID("b")
	})
	assertPanics(t, "DeletedAt twice", func() {
		NewTableSchema[schemaSample]("t").ID("id").DeletedAt("a").DeletedAt("b")
	})
	assertPanics(t, "CreatedAt twice", func() {
		NewTableSchema[schemaSample]("t").ID("id").CreatedAt("a").CreatedAt("b")
	})
	assertPanics(t, "UpdatedAt twice", func() {
		NewTableSchema[schemaSample]("t").ID("id").UpdatedAt("a").UpdatedAt("b")
	})
	assertPanics(t, "Revision twice", func() {
		NewTableSchema[schemaSample]("t").ID("id").Revision("a").Revision("b")
	})
	assertPanics(t, "NaturalID twice", func() {
		NewSharedBaseSchema("b").ID("id").Field("Name", "name").NaturalID("name").NaturalID("other")
	})
}

// TestTableSchema_DuplicateChildRejected proves a second aggregate child of the
// same Go type panics instead of silently overwriting the first — children are
// keyed by type name, so two collections of one type would drop one.
func TestTableSchema_DuplicateChildRejected(t *testing.T) {
	assertPanics(t, "two children of the same type", func() {
		c1 := NewTableSchema[embedFixture]("lines_a").ID("id").ParentID("root_id")
		c2 := NewTableSchema[embedFixture]("lines_b").ID("id").ParentID("root_id")
		NewTableSchema[schemaSample]("orders").ID("id").Child(c1).Child(c2)
	})
}

// collidingFixture is a DIFFERENT Go type that declares the SAME collection
// name as embedFixture — the one way two children can still land on one
// document segment now that the name is declared rather than derived.
type collidingFixture struct {
	ID   string
	Name string
}

func (collidingFixture) CollectionName() string { return "EmbedFixtures" }

// TestTableSchema_CollidingCollectionNameRejected proves two children whose
// types declare the SAME CollectionName panic at declaration: each collection
// occupies its own document segment, so the second would overwrite the first.
// The type-name guard above cannot catch this — the types differ.
func TestTableSchema_CollidingCollectionNameRejected(t *testing.T) {
	assertPanics(t, "two children declaring the same collection name", func() {
		c1 := NewTableSchema[embedFixture]("lines_a").ID("id").ParentID("root_id")
		c2 := NewTableSchema[collidingFixture]("lines_b").ID("id").ParentID("root_id")
		NewTableSchema[schemaSample]("orders").ID("id").Child(c1).Child(c2)
	})
}

// TestTableSchema_ChildWithoutCollectionNameRejected proves a child type that
// declares no collection name is rejected where it is declared, at boot — the
// framework has no fallback derivation to fall back to.
type unnamedChildFixture struct {
	ID   string
	Name string
}

func TestTableSchema_ChildWithoutCollectionNameRejected(t *testing.T) {
	assertPanics(t, "child type declares no CollectionName", func() {
		child := NewTableSchema[unnamedChildFixture]("lines").ID("id").ParentID("root_id")
		NewTableSchema[schemaSample]("orders").ID("id").Child(child)
	})
}

// TestTableSchema_GrandchildRejected proves ValidateChildDepth (run by
// WithSchema) panics when a declared aggregate child carries its own Child(...) —
// grandchildren are unsupported on the write side (root + one level).
func TestTableSchema_GrandchildRejected(t *testing.T) {
	grand := NewTableSchema[embedFixture]("grand").ID("id").ParentID("child_id")
	child := NewTableSchema[schemaSample]("child").ID("id").ParentID("root_id").Child(grand)
	root := NewTableSchema[schemaSample]("root").ID("id").Child(child)
	assertPanics(t, "child declares its own Child(...)", func() {
		root.ValidateChildDepth()
	})
}

// TestTableSchema_OneLevelChildOK confirms a root with a single level of
// children (the supported depth) passes the depth check.
func TestTableSchema_OneLevelChildOK(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("one-level aggregate panicked: %v", r)
		}
	}()
	child := NewTableSchema[embedFixture]("child").ID("id").ParentID("root_id")
	NewTableSchema[schemaSample]("root").ID("id").Child(child).ValidateChildDepth()
}

// TestTableSchema_Sibling_HappyPath proves a declared sibling is recorded,
// exposed via Siblings()/IsSecondary(), and passes ValidateSiblings when its
// fields are disjoint from the owner's.
func TestTableSchema_Sibling_HappyPath(t *testing.T) {
	root := NewTableSchema[schemaSample]("root").
		ID("id").
		Field("Name", "name").
		Sibling(NewSiblingSchema[schemaSample]("sib").Field("Removed", "removed"))

	sibs := root.Siblings()
	if len(sibs) != 1 || sibs[0].Table() != "sib" {
		t.Fatalf("Siblings() = %v, want one sibling table \"sib\"", sibs)
	}
	if !sibs[0].IsSecondary() {
		t.Errorf("a NewSiblingSchema must report IsSecondary() == true")
	}
	if root.IsSecondary() {
		t.Errorf("an owner schema must report IsSecondary() == false")
	}
	root.ValidateSiblings() // must not panic — fields are disjoint
}

// TestTableSchema_Sibling_BootGuards locks every declaration-time trava: a
// sibling owns no lifecycle (DeletedAt), no ParentID, no ID (it borrows the owner's),
// no children, no nested sibling; it must be over the same type, built with
// NewSiblingSchema, carry fields, and not collide table names. The kind-mismatch
// guards on Sibling()/Child() are covered too.
// The identity/lifecycle declarations fail AT THE CALL on a sibling schema —
// before the attach — and the ID/ParentID messages teach the DDL contract: the
// sibling table's physical ID column carries the OWNER's ID column NAME
// (naming it "<owner>_id" is the classic first-write 500).
func TestTableSchema_Sibling_DeclarationGuardsTeachTheDDLContract(t *testing.T) {
	wantPanicContaining := func(name, needle string, fn func()) {
		t.Helper()
		defer func() {
			r := recover()
			if r == nil {
				t.Errorf("%s: expected panic, got none", name)
				return
			}
			if msg := fmt.Sprint(r); !strings.Contains(msg, needle) {
				t.Errorf("%s: panic message must carry %q, got %q", name, needle, msg)
			}
		}()
		fn()
	}
	wantPanicContaining("ID on a sibling", "SAME name", func() {
		NewSiblingSchema[schemaSample]("s").ID("rental_listing_id")
	})
	wantPanicContaining("ParentID on a sibling", "the shared ID IS the link", func() {
		NewSiblingSchema[schemaSample]("s").ParentID("owner_id")
	})
	wantPanicContaining("DeletedAt on a sibling", "no lifecycle of its own", func() {
		NewSiblingSchema[schemaSample]("s").DeletedAt("deleted_at")
	})
	wantPanicContaining("CreatedAt on a sibling", "owner's CreatedAt/UpdatedAt already date that row", func() {
		NewSiblingSchema[schemaSample]("s").CreatedAt("created_at")
	})
	wantPanicContaining("UpdatedAt on a sibling", "owner's CreatedAt/UpdatedAt already date that row", func() {
		NewSiblingSchema[schemaSample]("s").UpdatedAt("updated_at")
	})
	// Planted state (not reachable through the builder any more) still trips the
	// attach-time guard — defense in depth.
	assertPanics(t, "planted managed timestamp on attach", func() {
		sib := NewSiblingSchema[schemaSample]("s").Field("Name", "name")
		sib.createdAt = "created_at"
		NewTableSchema[schemaSample]("root").ID("id").Sibling(sib)
	})
}

func TestTableSchema_Sibling_BootGuards(t *testing.T) {
	owner := func() *TableSchema { return NewTableSchema[schemaSample]("root").ID("id").Field("Name", "name") }

	assertPanics(t, "DeletedAt on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").Field("Removed", "removed").DeletedAt("del"))
	})
	assertPanics(t, "ParentID on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").ParentID("fk").Field("Removed", "removed"))
	})
	assertPanics(t, "ID on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").ID("id").Field("Removed", "removed"))
	})
	assertPanics(t, "sibling with no fields", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s"))
	})
	assertPanics(t, "sibling over a different type", func() {
		owner().Sibling(NewSiblingSchema[otherFixture]("s").Field("Tag", "tag"))
	})
	assertPanics(t, "non-sibling passed to Sibling()", func() {
		owner().Sibling(NewTableSchema[schemaSample]("s").Field("Removed", "removed"))
	})
	assertPanics(t, "sibling of a sibling", func() {
		NewSiblingSchema[schemaSample]("s1").Field("Name", "name").
			Sibling(NewSiblingSchema[schemaSample]("s2").Field("Removed", "removed"))
	})
	assertPanics(t, "sibling cannot own a child", func() {
		NewSiblingSchema[schemaSample]("s").Field("Name", "name").
			Child(NewTableSchema[embedFixture]("c").ID("id").ParentID("s_id"))
	})
	assertPanics(t, "secondary schema passed to Child()", func() {
		owner().Child(NewSiblingSchema[schemaSample]("s").Field("Removed", "removed"))
	})
	assertPanics(t, "duplicate sibling table name", func() {
		owner().
			Sibling(NewSiblingSchema[schemaSample]("dup").Field("Removed", "removed")).
			Sibling(NewSiblingSchema[schemaSample]("dup").Field("Created", "created"))
	})
}

// TestTableSchema_ValidateSiblings_Overlap proves the order-independent
// partition check at WithSchema: a column or Go field mapped by both the owner
// and a sibling is a boot panic.
func TestTableSchema_ValidateSiblings_Overlap(t *testing.T) {
	assertPanics(t, "column mapped by owner and sibling", func() {
		NewTableSchema[schemaSample]("root").ID("id").Field("Name", "shared").
			Sibling(NewSiblingSchema[schemaSample]("sib").Field("Removed", "shared")).
			ValidateSiblings()
	})
	assertPanics(t, "Go field mapped by owner and sibling", func() {
		NewTableSchema[schemaSample]("root").ID("id").Field("Name", "name").
			Sibling(NewSiblingSchema[schemaSample]("sib").Field("Name", "name2")).
			ValidateSiblings()
	})
}

// TestTableSchema_ChildWithSibling proves the one allowed recursive width: an
// aggregate child may carry its own sibling, and ValidateSiblings on the root
// validates the child's partition too.
func TestTableSchema_ChildWithSibling(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("child-with-sibling panicked: %v", r)
		}
	}()
	child := NewTableSchema[schemaSample]("child").ID("id").ParentID("root_id").Field("Name", "name").
		Sibling(NewSiblingSchema[schemaSample]("child_sib").Field("Removed", "removed"))
	NewTableSchema[schemaSample]("root").ID("id").Child(child).ValidateSiblings()
}

// TestTableSchema_ReadTranslationIncludesSiblings proves the read-path Go↔column
// translators resolve sibling fields as root-level fields (the doc is flat), in
// both directions — so the Mongo reader can filter/sort/project on a sibling
// field and ToGoDoc keeps a merged sibling column.
func TestTableSchema_ReadTranslationIncludesSiblings(t *testing.T) {
	root := NewTableSchema[schemaSample]("root").ID("id").Field("Name", "name").
		Sibling(NewSiblingSchema[schemaSample]("ext").Field("Removed", "removed"))
	if c, ok := root.ColumnForRead("Removed"); !ok || c != "removed" {
		t.Errorf("ColumnForRead(sibling field) = %q,%v — want \"removed\",true", c, ok)
	}
	if g, ok := root.GoNameForRead("removed"); !ok || g != "Removed" {
		t.Errorf("GoNameForRead(sibling column) = %q,%v — want \"Removed\",true", g, ok)
	}
}

// TestTableSchema_ChildSchemas proves ChildSchemas() returns every declared
// aggregate child ordered by table name (so the delete cascade emits
// deterministic SQL on any engine) and nil for a childless schema. The aggregate
// delete path relies on it to clear every declared child table by ParentID explicitly.
func TestTableSchema_ChildSchemas(t *testing.T) {
	if got := NewTableSchema[schemaSample]("root").ID("id").ChildSchemas(); got != nil {
		t.Errorf("childless schema: ChildSchemas() = %v, want nil", got)
	}
	alpha := NewTableSchema[embedFixture]("alpha").ID("id").ParentID("root_id")
	zebra := NewTableSchema[schemaSample]("zebra").ID("id").ParentID("root_id")
	// Declared zebra-then-alpha; ChildSchemas() must return them sorted by table.
	root := NewTableSchema[schemaSample]("root").ID("id").Child(zebra).Child(alpha)
	got := root.ChildSchemas()
	if len(got) != 2 {
		t.Fatalf("ChildSchemas() len = %d, want 2", len(got))
	}
	if got[0].Table() != "alpha" || got[1].Table() != "zebra" {
		t.Errorf("order = [%s %s], want [alpha zebra] (sorted by table name)", got[0].Table(), got[1].Table())
	}
}

// TestTableSchema_PKMandatory proves ID rejects an empty column — a
// single-column primary key is mandatory on every schema.
func TestTableSchema_PKMandatory(t *testing.T) {
	assertPanics(t, "empty ID column", func() {
		NewTableSchema[schemaSample]("t").ID("")
	})
}

// TestTableSchema_ChildRequiresPK proves an aggregate child registered without
// an explicit ID is rejected at Child() — there is no default primary key.
func TestTableSchema_ChildRequiresPK(t *testing.T) {
	assertPanics(t, "child without ID", func() {
		NewTableSchema[schemaSample]("root").ID("id").
			Child(NewTableSchema[embedFixture]("child").ParentID("root_id")) // no .ID
	})
}

// TestTableSchema_ChildRequiresFK proves an aggregate child registered without a
// foreign key is rejected at Child() — the persister injects the root id into
// the child ParentID on every write, so it cannot be empty.
func TestTableSchema_ChildRequiresFK(t *testing.T) {
	assertPanics(t, "child without ParentID", func() {
		NewTableSchema[schemaSample]("root").ID("id").
			Child(NewTableSchema[embedFixture]("child").ID("id")) // no .ParentID
	})
}

// TestTableSchema_ValidDeclarationDoesNotPanic confirms a well-formed schema —
// distinct columns across ID, fields, and all three managed slots — constructs
// cleanly. ID("id") matching the default column must not self-collide.
func TestTableSchema_ValidDeclarationDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("valid schema panicked: %v", r)
		}
	}()
	s := NewTableSchema[schemaSample]("t").
		ID("id").
		Field("Name", "name").
		Field("Created", "created").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	if got, _ := s.ColumnOf("Name"); got != "name" {
		t.Errorf("ColumnOf(Name) = %q, want name", got)
	}
}

// --- ValidateOldCloneSafety fixtures ------------------------------------------

// oldCloneSkipTag persists a field excluded from JSON — the shape the guard
// rejects (the field would vanish from the domain.Old ghost).
type oldCloneSkipTag struct {
	ID   string
	Name string `json:"-"`
}

// oldCloneRenameTag persists a field with a RENAMING json tag — harmless (the
// clone round-trip marshals and unmarshals the same type, so renames are
// symmetric) and must pass.
type oldCloneRenameTag struct {
	ID   string
	Name string `json:"nome"`
}

// oldCloneSiblingTag partitions its fields across owner + sibling; the
// sibling-mapped field carries the poisonous tag.
type oldCloneSiblingTag struct {
	ID    string
	Name  string
	Notes string `json:"-"`
}

// oldCloneRoleTag is a shared-base ROLE whose base-mapped field carries the
// poisonous tag (the type-less base resolves its fields on the role's type).
type oldCloneRoleTag struct {
	ID         string
	RoleField  string
	PersonName string `json:"-"`
}

// oldCloneMarshaler / oldCloneUnmarshaler implement one half of a custom JSON
// codec each — either alone takes over the clone round-trip and must be
// rejected.
type oldCloneMarshaler struct {
	ID   string
	Name string
}

func (m oldCloneMarshaler) MarshalJSON() ([]byte, error) { return json.Marshal(m.Name) }

type oldCloneUnmarshaler struct {
	ID   string
	Name string
}

func (m *oldCloneUnmarshaler) UnmarshalJSON([]byte) error { return nil }

// TestTableSchema_ValidateOldCloneSafety proves the boot guard over the
// domain.Old JSON round-trip: a persisted field tagged `json:"-"` (declared on
// the root, on a sibling partition, or mapped through a shared base) and a
// custom json.Marshaler/json.Unmarshaler on the entity type are boot panics;
// renaming tags, type-less schemas and aggregate children (cloned by value,
// never through JSON) pass.
func TestTableSchema_ValidateOldCloneSafety(t *testing.T) {
	assertPanics(t, "root field tagged json:\"-\"", func() {
		NewTableSchema[oldCloneSkipTag]("t").ID("id").Field("Name", "name").
			ValidateOldCloneSafety()
	})
	assertPanics(t, "sibling field tagged json:\"-\"", func() {
		NewTableSchema[oldCloneSiblingTag]("t").ID("id").Field("Name", "name").
			Sibling(NewSiblingSchema[oldCloneSiblingTag]("t_notes").Field("Notes", "notes")).
			ValidateOldCloneSafety()
	})
	assertPanics(t, "shared-base field tagged json:\"-\"", func() {
		base := NewSharedBaseSchema("people").Revision("revision").ID("id").
			Field("PersonName", "person_name").NaturalID("person_name")
		NewTableSchema[oldCloneRoleTag]("alunos").ID("id").Field("RoleField", "role_field").
			SharedBase(base, "person_id").
			ValidateOldCloneSafety()
	})
	assertPanics(t, "entity implements json.Marshaler", func() {
		NewTableSchema[oldCloneMarshaler]("t").ID("id").Field("Name", "name").
			ValidateOldCloneSafety()
	})
	assertPanics(t, "entity implements json.Unmarshaler", func() {
		NewTableSchema[oldCloneUnmarshaler]("t").ID("id").Field("Name", "name").
			ValidateOldCloneSafety()
	})

	// Tolerated shapes — any panic here fails the test.
	NewTableSchema[oldCloneRenameTag]("t").ID("id").Field("Name", "name").
		ValidateOldCloneSafety()
	NewExternalSchema("upstream").Field("Name", "name").
		ValidateOldCloneSafety()
	// An aggregate CHILD with the tag is exempt: children reach the Old ghost by
	// value copy of the aggregate map, never through the JSON round-trip.
	NewTableSchema[schemaSample]("root").ID("id").Field("Name", "name").
		Child(NewTableSchema[oldCloneSkipTag]("child").ID("id").ParentID("root_id").Field("Name", "name")).
		ValidateOldCloneSafety()
}

func (schemaSample) CollectionName() string { return "SchemaSamples" }

func (oldCloneSkipTag) CollectionName() string { return "OldCloneSkipTags" }
