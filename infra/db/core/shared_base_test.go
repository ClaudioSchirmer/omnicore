package core

import "testing"

// B1 coverage for the SharedBase (Modelagem 2) schema foundation: declaring a
// shared base + a role referencing it, the accessors, and every declaration-time
// guard.

func sharedBaseFixture() *TableSchema {
	return NewSharedBase("pessoa").
		PK("id").
		Field("Name", "name").
		Field("Created", "document"). // Created is a real schemaSample field; "document" is its column
		NaturalKey("document").
		OrphanPolicy(DeleteWhenUnreferenced)
}

func TestSharedBase_HappyPath(t *testing.T) {
	base := sharedBaseFixture()
	role := NewTableSchema[schemaSample]("aluno").
		PK("id").
		Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")

	if !base.IsSharedBase() {
		t.Error("NewSharedBase must report IsSharedBase() == true")
	}
	if base.NaturalKeyColumn() != "document" {
		t.Errorf("NaturalKeyColumn() = %q, want \"document\"", base.NaturalKeyColumn())
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
	base := sharedBaseFixture() // PK id, Name→name, Created→document, NaturalKey document
	role := NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
		SharedBase(base, "pessoa_id")

	if c, ok := role.ColumnForRead("Name"); !ok || c != "name" {
		t.Errorf("ColumnForRead(base field Name) = %q,%v — want \"name\",true", c, ok)
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
	role := func() *TableSchema { return NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula") }

	assertPanics(t, "NaturalKey on a non-shared-base", func() {
		NewTableSchema[schemaSample]("t").PK("id").NaturalKey("email")
	})
	assertPanics(t, "OrphanPolicy on a non-shared-base", func() {
		NewTableSchema[schemaSample]("t").PK("id").OrphanPolicy(KeepOrphan)
	})
	assertPanics(t, "SharedBase given a non-shared-base", func() {
		role().SharedBase(NewTableSchema[schemaSample]("x").PK("id"), "pessoa_id")
	})
	assertPanics(t, "SharedBase declared twice", func() {
		role().SharedBase(sharedBaseFixture(), "pessoa_id").SharedBase(sharedBaseFixture(), "outra_id")
	})
	assertPanics(t, "shared base without PK", func() {
		role().SharedBase(NewSharedBase("pessoa").Field("Name", "name").NaturalKey("name"), "pessoa_id")
	})
	assertPanics(t, "shared base without NaturalKey", func() {
		role().SharedBase(NewSharedBase("pessoa").PK("id").Field("Name", "name"), "pessoa_id")
	})
	assertPanics(t, "NaturalKey column not a declared field", func() {
		role().SharedBase(NewSharedBase("pessoa").PK("id").Field("Name", "name").NaturalKey("cpf"), "pessoa_id")
	})
	assertPanics(t, "FK column also a role field", func() {
		NewTableSchema[schemaSample]("aluno").PK("id").Field("Removed", "matricula").
			SharedBase(sharedBaseFixture(), "matricula")
	})
	assertPanics(t, "base field not on the role type", func() {
		// schemaSample has no "Unknown" field; a base mapping it must fail at attach.
		role().SharedBase(NewSharedBase("pessoa").PK("id").Field("Unknown", "u").NaturalKey("u"), "pessoa_id")
	})
	assertPanics(t, "empty FK column", func() {
		role().SharedBase(sharedBaseFixture(), "")
	})
	assertPanics(t, "a sibling cannot reference a shared base", func() {
		NewSiblingSchema[schemaSample]("usuario").Field("Removed", "matricula").
			SharedBase(sharedBaseFixture(), "pessoa_id")
	})
}
