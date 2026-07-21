package core

import "testing"

// Remaining SharedBase foundation coverage: the NaturalKey empty-column guard,
// the ReferencingRoles registry read (with its lazy soft-delete resolution),
// the SharedBaseScanPlan absent branch, and the child-name divergence axis of
// AssertSharedBaseEquivalent.

func TestSharedBase_NaturalKeyRequiresColumn(t *testing.T) {
	assertPanics(t, "empty NaturalKey column", func() {
		NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("")
	})
}

func TestSharedBase_ReferencingRoles(t *testing.T) {
	var nilSchema *TableSchema
	if got := nilSchema.ReferencingRoles(); got != nil {
		t.Errorf("nil schema ReferencingRoles() = %v, want nil", got)
	}
	base := NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name")
	if got := base.ReferencingRoles(); got != nil {
		t.Errorf("unreferenced base ReferencingRoles() = %v, want nil", got)
	}

	// SoftDelete declared AFTER SharedBase — the registry stores the schema
	// pointer, so the role's soft-delete column must still resolve (lazily).
	NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
		SharedBase(base, "pessoa_id").
		SoftDelete("deleted_at")
	NewTableSchema[schemaSample]("professor").PK("id").Field("Removed", "siape").
		SharedBase(base, "pessoa_fk") // no soft-delete on this role

	got := base.ReferencingRoles()
	if len(got) != 2 {
		t.Fatalf("ReferencingRoles() = %v, want 2 roles", got)
	}
	if got[0] != (RoleRef{Table: "aluno", FKColumn: "pessoa_id", SoftDeleteCol: "deleted_at"}) {
		t.Errorf("role[0] = %+v, want aluno/pessoa_id/deleted_at (lazy soft-delete)", got[0])
	}
	if got[1] != (RoleRef{Table: "professor", FKColumn: "pessoa_fk", SoftDeleteCol: ""}) {
		t.Errorf("role[1] = %+v, want professor/pessoa_fk/<none>", got[1])
	}
}

func TestSharedBase_ScanPlanAbsent(t *testing.T) {
	var nilSchema *TableSchema
	if _, _, ok := nilSchema.SharedBaseScanPlan(); ok {
		t.Error("nil schema SharedBaseScanPlan() must be ok=false")
	}
	noLink := NewTableSchema[schemaSample]("aluno").PK("id").Field("Name", "name")
	if cols, byCol, ok := noLink.SharedBaseScanPlan(); ok || cols != nil || byCol != nil {
		t.Errorf("SharedBaseScanPlan() without a base = (%v,%v,%v), want (nil,nil,false)", cols, byCol, ok)
	}
}

func TestAssertSharedBaseEquivalent_ChildNameDiverges(t *testing.T) {
	// Same child COUNT, different child type names — the absent-child axis.
	declare := func() *TableSchema {
		return NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name")
	}
	a := declare().Child(NewTableSchema[embedFixture]("docs").PK("id").FK("pessoa_id"))
	b := declare().Child(NewTableSchema[schemaSample]("docs").PK("id").FK("pessoa_id"))
	assertPanics(t, "native child present in a but absent in b", func() {
		AssertSharedBaseEquivalent(a, b)
	})
}
