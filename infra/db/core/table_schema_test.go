package core

import "testing"

// schemaSample is a type-anchored target for TableSchema construction tests —
// the exported fields exist so Field/PK declarations validate against it.
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
// collision enforcement over the full physical column set (PK + Field +
// SoftDelete/CreatedAt/UpdatedAt). Each managed setter and PK must reject a
// column already claimed by another slot, regardless of declaration order.
func TestTableSchema_ManagedColumnBijection(t *testing.T) {
	assertPanics(t, "CreatedAt vs UpdatedAt same column", func() {
		NewTableSchema[schemaSample]("t").CreatedAt("ts").UpdatedAt("ts")
	})
	assertPanics(t, "SoftDelete vs CreatedAt same column", func() {
		NewTableSchema[schemaSample]("t").SoftDelete("ts").CreatedAt("ts")
	})
	assertPanics(t, "Field then SoftDelete same column", func() {
		NewTableSchema[schemaSample]("t").Field("Name", "deleted_at").SoftDelete("deleted_at")
	})
	assertPanics(t, "SoftDelete then Field same column", func() {
		NewTableSchema[schemaSample]("t").SoftDelete("deleted_at").Field("Name", "deleted_at")
	})
	assertPanics(t, "Field then CreatedAt same column", func() {
		NewTableSchema[schemaSample]("t").Field("Created", "created_at").CreatedAt("created_at")
	})
	assertPanics(t, "PK column claimed by a field", func() {
		NewTableSchema[schemaSample]("t").Field("Name", "k").PK("k")
	})
	assertPanics(t, "CreatedAt collides with non-default PK", func() {
		NewTableSchema[schemaSample]("t").PK("pk").CreatedAt("pk")
	})
}

// TestTableSchema_GrandchildRejected proves ValidateChildDepth (run by
// WithSchema) panics when a declared aggregate child carries its own Child(...) —
// grandchildren are unsupported on the write side (root + one level).
func TestTableSchema_GrandchildRejected(t *testing.T) {
	grand := NewTableSchema[embedFixture]("grand").PK("id").FK("child_id")
	child := NewTableSchema[schemaSample]("child").PK("id").FK("root_id").Child(grand)
	root := NewTableSchema[schemaSample]("root").PK("id").Child(child)
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
	child := NewTableSchema[embedFixture]("child").PK("id").FK("root_id")
	NewTableSchema[schemaSample]("root").PK("id").Child(child).ValidateChildDepth()
}

// TestTableSchema_Sibling_HappyPath proves a declared sibling is recorded,
// exposed via Siblings()/IsSecondary(), and passes ValidateSiblings when its
// fields are disjoint from the owner's.
func TestTableSchema_Sibling_HappyPath(t *testing.T) {
	root := NewTableSchema[schemaSample]("root").
		PK("id").
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
// sibling owns no lifecycle (SoftDelete), no FK, no PK (it borrows the owner's),
// no children, no nested sibling; it must be over the same type, built with
// NewSiblingSchema, carry fields, and not collide table names. The kind-mismatch
// guards on Sibling()/Child() are covered too.
func TestTableSchema_Sibling_BootGuards(t *testing.T) {
	owner := func() *TableSchema { return NewTableSchema[schemaSample]("root").PK("id").Field("Name", "name") }

	assertPanics(t, "SoftDelete on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").Field("Removed", "removed").SoftDelete("del"))
	})
	assertPanics(t, "FK on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").FK("fk").Field("Removed", "removed"))
	})
	assertPanics(t, "PK on a sibling", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s").PK("id").Field("Removed", "removed"))
	})
	assertPanics(t, "sibling with no fields", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("s"))
	})
	assertPanics(t, "sibling over a different type", func() {
		owner().Sibling(NewSiblingSchema[embedFixture]("s").Field("ID", "other_id"))
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
			Child(NewTableSchema[embedFixture]("c").PK("id").FK("s_id"))
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
		NewTableSchema[schemaSample]("root").PK("id").Field("Name", "shared").
			Sibling(NewSiblingSchema[schemaSample]("sib").Field("Removed", "shared")).
			ValidateSiblings()
	})
	assertPanics(t, "Go field mapped by owner and sibling", func() {
		NewTableSchema[schemaSample]("root").PK("id").Field("Name", "name").
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
	child := NewTableSchema[schemaSample]("child").PK("id").FK("root_id").Field("Name", "name").
		Sibling(NewSiblingSchema[schemaSample]("child_sib").Field("Removed", "removed"))
	NewTableSchema[schemaSample]("root").PK("id").Child(child).ValidateSiblings()
}

// TestTableSchema_ChildSchemas proves ChildSchemas() returns every declared
// aggregate child ordered by table name (so the delete cascade emits
// deterministic SQL on any engine) and nil for a childless schema. The aggregate
// delete path relies on it to clear every declared child table by FK explicitly.
func TestTableSchema_ChildSchemas(t *testing.T) {
	if got := NewTableSchema[schemaSample]("root").PK("id").ChildSchemas(); got != nil {
		t.Errorf("childless schema: ChildSchemas() = %v, want nil", got)
	}
	alpha := NewTableSchema[embedFixture]("alpha").PK("id").FK("root_id")
	zebra := NewTableSchema[schemaSample]("zebra").PK("id").FK("root_id")
	// Declared zebra-then-alpha; ChildSchemas() must return them sorted by table.
	root := NewTableSchema[schemaSample]("root").PK("id").Child(zebra).Child(alpha)
	got := root.ChildSchemas()
	if len(got) != 2 {
		t.Fatalf("ChildSchemas() len = %d, want 2", len(got))
	}
	if got[0].Table() != "alpha" || got[1].Table() != "zebra" {
		t.Errorf("order = [%s %s], want [alpha zebra] (sorted by table name)", got[0].Table(), got[1].Table())
	}
}

// TestTableSchema_PKMandatory proves PK rejects an empty column — a
// single-column primary key is mandatory on every schema.
func TestTableSchema_PKMandatory(t *testing.T) {
	assertPanics(t, "empty PK column", func() {
		NewTableSchema[schemaSample]("t").PK("")
	})
}

// TestTableSchema_ChildRequiresPK proves an aggregate child registered without
// an explicit PK is rejected at Child() — there is no default primary key.
func TestTableSchema_ChildRequiresPK(t *testing.T) {
	assertPanics(t, "child without PK", func() {
		NewTableSchema[schemaSample]("root").PK("id").
			Child(NewTableSchema[embedFixture]("child").FK("root_id")) // no .PK
	})
}

// TestTableSchema_ChildRequiresFK proves an aggregate child registered without a
// foreign key is rejected at Child() — the persister injects the root id into
// the child FK on every write, so it cannot be empty.
func TestTableSchema_ChildRequiresFK(t *testing.T) {
	assertPanics(t, "child without FK", func() {
		NewTableSchema[schemaSample]("root").PK("id").
			Child(NewTableSchema[embedFixture]("child").PK("id")) // no .FK
	})
}

// TestTableSchema_ValidDeclarationDoesNotPanic confirms a well-formed schema —
// distinct columns across PK, fields, and all three managed slots — constructs
// cleanly. PK("id") matching the default column must not self-collide.
func TestTableSchema_ValidDeclarationDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("valid schema panicked: %v", r)
		}
	}()
	s := NewTableSchema[schemaSample]("t").
		PK("id").
		Field("Name", "name").
		Field("Created", "created").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	if got, _ := s.ColumnOf("Name"); got != "name" {
		t.Errorf("ColumnOf(Name) = %q, want name", got)
	}
}
