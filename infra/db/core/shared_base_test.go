package core

import "testing"

// B1 coverage for the SharedBase (Modelagem 2) schema foundation: declaring a
// shared base + a role referencing it, the accessors, and every declaration-time
// guard.

func sharedBaseFixture() *TableSchema {
	return NewSharedBaseSchema("pessoa").Revision("revision").
		ID("id").
		Field("Name", "name").
		Field("Created", "document"). // Created is a real schemaSample field; "document" is its column
		NaturalID("document").
		OrphanPolicy(DeleteWhenUnreferenced)
}

func TestSharedBase_HappyPath(t *testing.T) {
	base := sharedBaseFixture()
	role := NewTableSchema[schemaSample]("aluno").
		ID("id").
		Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")

	if !base.IsSharedBase() {
		t.Error("NewSharedBaseSchema must report IsSharedBase() == true")
	}
	if base.NaturalIDColumn() != "document" {
		t.Errorf("NaturalIDColumn() = %q, want \"document\"", base.NaturalIDColumn())
	}
	if base.OrphanPolicyValue() != DeleteWhenUnreferenced {
		t.Errorf("OrphanPolicyValue() = %v, want DeleteWhenUnreferenced", base.OrphanPolicyValue())
	}
	gotBase, fk, ok := role.SharedBaseRef()
	if !ok || gotBase != base || fk != "pessoa_id" {
		t.Errorf("SharedBaseRef() = %v,%q,%v — want base,\"pessoa_id\",true", gotBase, fk, ok)
	}
	if role.IsSharedBase() {
		t.Error("a role must not report IsSharedBase()")
	}
}

// The role's read-path translators resolve shared-base fields as root-level Go
// fields (the base is merged FLAT into the role doc), and the scan plan maps the
// base columns to the role's struct field indices.
func TestSharedBase_ReadTranslationAndScanPlan(t *testing.T) {
	base := sharedBaseFixture() // ID id, Name→name, Created→document, NaturalID document
	role := NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")

	if c, ok := resolvedColumn(role, "Name"); !ok || c != "name" {
		t.Errorf("Resolve(base field Name) = %q,%v — want \"name\",true", c, ok)
	}
	if g, ok := role.GoNameForRead("name"); !ok || g != "Name" {
		t.Errorf("GoNameForRead(base column name) = %q,%v — want \"Name\",true", g, ok)
	}
	cols, byCol, ok := role.SharedBaseScanPlan()
	if !ok || len(cols) != 2 {
		t.Fatalf("SharedBaseScanPlan() = %v,%v,%v — want 2 cols", cols, byCol, ok)
	}
	if _, has := byCol["name"]; !has {
		t.Errorf("scan plan must map the base column \"name\" to a role field index: %v", byCol)
	}
	if _, has := byCol["document"]; !has {
		t.Errorf("scan plan must map the base column \"document\": %v", byCol)
	}
}

func TestSharedBase_BootGuards(t *testing.T) {
	role := func() *TableSchema {
		return NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula")
	}

	assertPanics(t, "NaturalID on a non-shared-base", func() {
		NewTableSchema[schemaSample]("t").ID("id").NaturalID("email")
	})
	assertPanics(t, "OrphanPolicy on a non-shared-base", func() {
		NewTableSchema[schemaSample]("t").ID("id").OrphanPolicy(KeepOrphan)
	})
	assertPanics(t, "SharedBase given a non-shared-base", func() {
		role().SharedBase(NewTableSchema[schemaSample]("x").ID("id"), "pessoa_id")
	})
	assertPanics(t, "SharedBase declared twice", func() {
		role().SharedBase(sharedBaseFixture(), "pessoa_id").SharedBase(sharedBaseFixture(), "outra_id")
	})
	assertPanics(t, "shared base without ID", func() {
		role().SharedBase(NewSharedBaseSchema("pessoa").Revision("revision").Field("Name", "name").NaturalID("name"), "pessoa_id")
	})
	assertPanics(t, "shared base without NaturalID", func() {
		role().SharedBase(NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name"), "pessoa_id")
	})
	assertPanics(t, "NaturalID column not a declared field", func() {
		role().SharedBase(NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Name", "name").NaturalID("cpf"), "pessoa_id")
	})
	assertPanics(t, "ParentID column also a role field", func() {
		NewTableSchema[schemaSample]("aluno").ID("id").Field("Removed", "matricula").
			SharedBase(sharedBaseFixture(), "matricula")
	})
	assertPanics(t, "base field not on the role type", func() {
		// schemaSample has no "Unknown" field; a base mapping it must fail at attach.
		role().SharedBase(NewSharedBaseSchema("pessoa").Revision("revision").ID("id").Field("Unknown", "u").NaturalID("u"), "pessoa_id")
	})
	assertPanics(t, "empty ParentID column", func() {
		role().SharedBase(sharedBaseFixture(), "")
	})
	assertPanics(t, "a sibling cannot reference a shared base", func() {
		NewSiblingSchema[schemaSample]("usuario").Field("Removed", "matricula").
			SharedBase(sharedBaseFixture(), "pessoa_id")
	})
}
