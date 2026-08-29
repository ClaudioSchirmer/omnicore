package core

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The Direct schema: one table, no aggregate. These tests pin what it accepts,
// what it refuses at DECLARATION time (not on the first request), and the one
// extra rule it puts on the anchored struct.

type directJobRow struct {
	ID       domain.ID
	Status   string
	RunAt    string
	ParentID domain.ID
}

type directNoIDRow struct{ Status string }

type directWrongIDRow struct {
	ID     string
	Status string
}

func directJobSchema() *TableSchema {
	return NewDirectSchema[directJobRow]("job_queue").
		ID("id").
		Field("Status", "status").
		Field("RunAt", "run_at").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

func TestDirectSchema_IsTheSameDescriptor(t *testing.T) {
	s := directJobSchema()
	if !s.IsDirect() {
		t.Fatal("NewDirectSchema must mark the schema Direct")
	}
	if s.Table() != "job_queue" || s.IDColumn() != "id" {
		t.Fatalf("table/id = %q/%q", s.Table(), s.IDColumn())
	}
	// It resolves through the SAME surface every other schema does: its own
	// fields, the id, and the managed slots.
	for _, name := range []string{"ID", "Status", "RunAt", "CreatedAt", "UpdatedAt", "DeletedAt"} {
		if _, ok := s.Resolve(name); !ok {
			t.Errorf("Resolve(%q) failed — a Direct schema resolves like any other", name)
		}
	}
	// And the id is a scannable column, because the row declares it as a field.
	cols, byCol := s.ScanPlan()
	if len(cols) == 0 || cols[0] != "id" {
		t.Fatalf("ScanPlan cols = %v, want the id first", cols)
	}
	if _, ok := byCol["id"]; !ok {
		t.Error("the id column must map to the row's ID field")
	}
}

func TestDirectSchema_ResolvesParentID(t *testing.T) {
	// The child-table case: a Direct schema over an aggregate's child table
	// filters on ParentID with nothing extra declared.
	s := NewDirectSchema[directJobRow]("user_phones").ID("id").ParentID("user_id").Field("Status", "status")
	rf, ok := s.Resolve("ParentID")
	if !ok || rf.Column != "user_id" {
		t.Fatalf("Resolve(ParentID) = %+v, %v", rf, ok)
	}
}

func TestDirectSchema_RefusesVerticalComposition(t *testing.T) {
	child := NewTableSchema[directJobRow]("phones").ID("id").ParentID("job_id").Field("Status", "status")
	mustPanicWith(t, "Child composes tables DOWNWARD", func() {
		directJobSchema().Child(child)
	})
	mustPanicWith(t, "Sibling composes tables DOWNWARD", func() {
		directJobSchema().Sibling(NewSiblingSchema[directJobRow]("job_extra").Field("Status", "status"))
	})
	base := NewSharedBaseSchema("pessoa").Revision("revision").ID("id").
		Field("Name", "name").NaturalID("name")
	mustPanicWith(t, "SharedBase composes tables DOWNWARD", func() {
		directJobSchema().SharedBase(base, "pessoa_id")
	})
}

func TestDirectSchema_RequiresAnIDFieldOnTheRow(t *testing.T) {
	mustPanicWith(t, "it has no exported ID field", func() {
		NewDirectSchema[directNoIDRow]("t").ID("id")
	})
	mustPanicWith(t, "its ID field is string, not domain.ID", func() {
		NewDirectSchema[directWrongIDRow]("t").ID("id")
	})
}

// An ordinary (non-Direct) schema is unaffected by any of the above: the
// aggregate path keeps declaring children, siblings and a shared base, and an
// entity root still carries its id privately.
func TestDirectSchema_LeavesTheAggregatePathAlone(t *testing.T) {
	s := NewTableSchema[directNoIDRow]("things").ID("id").Field("Status", "status")
	if s.IsDirect() {
		t.Fatal("a plain TableSchema must not be Direct")
	}
	if _, ok := s.Resolve("Status"); !ok {
		t.Error("a plain schema still resolves its fields")
	}
}
