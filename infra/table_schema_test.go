package infra

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
		NewTableSchema[schemaSample]("t").Field("Name", "k").PK("ID", "k")
	})
	assertPanics(t, "CreatedAt collides with non-default PK", func() {
		NewTableSchema[schemaSample]("t").PK("ID", "pk").CreatedAt("pk")
	})
}

// TestTableSchema_ValidDeclarationDoesNotPanic confirms a well-formed schema —
// distinct columns across PK, fields, and all three managed slots — constructs
// cleanly. PK("ID","id") matching the default column must not self-collide.
func TestTableSchema_ValidDeclarationDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("valid schema panicked: %v", r)
		}
	}()
	s := NewTableSchema[schemaSample]("t").
		PK("ID", "id").
		Field("Name", "name").
		Field("Created", "created").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
	if got, _ := s.ColumnOf("Name"); got != "name" {
		t.Errorf("ColumnOf(Name) = %q, want name", got)
	}
}
