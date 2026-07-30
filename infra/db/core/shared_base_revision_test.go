package core

import (
	"strings"
	"testing"
)

// The Revision surface: mandatory on every attached shared base, SharedBase-only,
// reserved-namespace-guarded, part of the equivalence contract.

type revRoleEntity struct {
	ID       string
	Name     string
	Document string
	Extra    string
}

func revBase() *TableSchema {
	return NewSharedBaseSchema("pessoa").
		ID("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalID("document")
}

func mustPanicContains(t *testing.T, substr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic mentioning %q", substr)
		}
		if msg, ok := r.(string); !ok || !strings.Contains(msg, substr) {
			t.Fatalf("panic = %v, want it to mention %q", r, substr)
		}
	}()
	fn()
}

func TestRevision_MandatoryOnAttach(t *testing.T) {
	mustPanicContains(t, "declares no Revision", func() {
		NewTableSchema[*revRoleEntity]("aluno").ID("id").SharedBase(revBase(), "pessoa_id")
	})
}

func TestRevision_RootOnly(t *testing.T) {
	// Generalized in 4b-3: any ROOT schema declares it; a sibling or child
	// (owner-guarded rows) must not.
	mustPanicContains(t, "ENTITY schema", func() {
		NewSiblingSchema[*revRoleEntity]("aluno_extra").Revision("revision")
	})
	mustPanicContains(t, "ENTITY schema", func() {
		NewTableSchema[*revRoleEntity]("aluno_children").ParentID("aluno_id").Revision("revision")
	})
	if got := NewTableSchema[*revRoleEntity]("aluno").ID("id").Revision("revision").RevisionColumn(); got != "revision" {
		t.Fatalf("a plain root schema must accept Revision, got %q", got)
	}
}

func TestRevision_EmptyAndReservedRejected(t *testing.T) {
	mustPanicContains(t, "non-empty column", func() {
		revBase().Revision("")
	})
	mustPanicContains(t, "reserved", func() {
		revBase().Revision("_revision")
	})
}

func TestRevision_AccessorAndFieldCollision(t *testing.T) {
	base := revBase().Revision("revision")
	if base.RevisionColumn() != "revision" {
		t.Fatalf("RevisionColumn = %q, want revision", base.RevisionColumn())
	}
	mustPanicContains(t, "collides", func() {
		base.Field("Extra", "revision")
	})
}

func TestRevision_EquivalenceDivergence(t *testing.T) {
	a := revBase().Revision("revision")
	b := revBase().Revision("rev")
	mustPanicContains(t, "Revision column", func() {
		AssertSharedBaseEquivalent(a, b)
	})
}

func TestReservedColumnPrefix_RejectedEverywhere(t *testing.T) {
	mustPanicContains(t, "reserved", func() {
		NewTableSchema[*revRoleEntity]("t").ID("_id")
	})
	mustPanicContains(t, "reserved", func() {
		NewTableSchema[*revRoleEntity]("t").ID("id").Field("Name", "_name")
	})
	mustPanicContains(t, "reserved", func() {
		NewTableSchema[*revRoleEntity]("t").ID("id").DeletedAt("_deleted_at")
	})
}

func TestPayloadColumnTypes_CoversAllScalarSources(t *testing.T) {
	base := revBase().Revision("brev")
	role := NewTableSchema[*revRoleEntity]("aluno").ID("id").Revision("revision").
		Field("Extra", "extra").DeletedAt("deleted_at").
		SharedBase(base, "pessoa_id")
	types := role.PayloadColumnTypes()
	for _, col := range []string{"id", "extra", "deleted_at", "pessoa_id", "name", "document", "brev"} {
		if _, ok := types[col]; !ok {
			t.Errorf("PayloadColumnTypes must cover %q, got %v", col, types)
		}
	}
	if got := role.SharedBaseBusinessColumns(); len(got) != 2 {
		t.Errorf("SharedBaseBusinessColumns = %v, want the base's business fields", got)
	}
	if NewTableSchema[*revRoleEntity]("solo").ID("id").SharedBaseBusinessColumns() != nil {
		t.Error("a schema without a shared base answers nil")
	}
}

// The declaration-order hole: Revision declared BEFORE ParentID slips past Revision's
// own guard — Child() closes it at attach time.
func TestRevision_ChildAttachRejectsOwnToken(t *testing.T) {
	sneaky := NewTableSchema[*revRoleEntity]("kids").ID("id").Revision("revision")
	sneaky.ParentID("parent_id").Field("Name", "name")
	mustPanicContains(t, "guarded by its owner's revision", func() {
		NewTableSchema[*revRoleEntity]("parents").ID("id").Revision("revision").Child(sneaky)
	})
}
