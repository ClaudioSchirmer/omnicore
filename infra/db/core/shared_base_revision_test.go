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
	return NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Document", "document").
		NaturalKey("document")
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
		NewTableSchema[*revRoleEntity]("aluno").PK("id").SharedBase(revBase(), "pessoa_id")
	})
}

func TestRevision_SharedBaseOnly(t *testing.T) {
	mustPanicContains(t, "applies only to a SharedBase", func() {
		NewTableSchema[*revRoleEntity]("aluno").Revision("revision")
	})
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
		NewTableSchema[*revRoleEntity]("t").PK("_id")
	})
	mustPanicContains(t, "reserved", func() {
		NewTableSchema[*revRoleEntity]("t").PK("id").Field("Name", "_name")
	})
	mustPanicContains(t, "reserved", func() {
		NewTableSchema[*revRoleEntity]("t").PK("id").SoftDelete("_deleted_at")
	})
}
