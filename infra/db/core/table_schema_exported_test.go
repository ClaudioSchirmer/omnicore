package core

import (
	"reflect"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Coverage for the exported write-path accessors (the out-of-package engine
// surface), BoolColumns, ScanPlan's struct-field branches, FieldResolver,
// ValidateAnchored, and the remaining Sibling boot guards.

// boolSample carries a bool and a *bool so BoolColumns has both flavors to
// detect, plus a non-bool field it must skip.
type boolSample struct {
	ID       string
	Active   bool
	Verified *bool
	Name     string
}

func TestTableSchema_FKColumn(t *testing.T) {
	child := NewTableSchema[embedFixture]("child").ID("id").ParentID("root_id")
	if got := child.ParentIDColumn(); got != "root_id" {
		t.Errorf("ParentIDColumn() = %q, want root_id", got)
	}
	if got := covFullSchema().ParentIDColumn(); got != "" {
		t.Errorf("root ParentIDColumn() = %q, want empty", got)
	}
}

func TestTableSchema_ExportedWriteAccessors(t *testing.T) {
	child := NewTableSchema[embedFixture]("child").ID("id").ParentID("root_id")
	s := covFullSchema().Child(child) // ID id, Name→name, all three managed columns

	got := s.WriteFields(&schemaSample{Name: "bob"})
	if len(got) != 1 || got["name"] != "bob" {
		t.Errorf("WriteFields() = %v, want map[name:bob]", got)
	}
	insert := s.InsertNowColumns()
	if !reflect.DeepEqual(insert, []string{"created_at", "updated_at"}) {
		t.Errorf("InsertNowColumns() = %v, want [created_at updated_at]", insert)
	}
	update := s.UpdateNowColumns()
	if !reflect.DeepEqual(update, []string{"updated_at"}) {
		t.Errorf("UpdateNowColumns() = %v, want [updated_at]", update)
	}
	if col, ok := s.DeletedAtColumn(); !ok || col != "deleted_at" {
		t.Errorf("DeletedAtColumn() = (%q,%v), want (deleted_at,true)", col, ok)
	}
	if got := s.ChildSchema("embedFixture"); got != child {
		t.Errorf("ChildSchema(embedFixture) = %v, want the declared child", got)
	}
	if got := s.ChildSchema("Nope"); got != nil {
		t.Errorf("ChildSchema(unknown) = %v, want nil", got)
	}
}

func TestTableSchema_Siblings_NilAndEmpty(t *testing.T) {
	var nilSchema *TableSchema
	if got := nilSchema.Siblings(); got != nil {
		t.Errorf("nil schema Siblings() = %v, want nil", got)
	}
	if got := covFullSchema().Siblings(); got != nil {
		t.Errorf("siblingless Siblings() = %v, want nil", got)
	}
}

func TestTableSchema_BoolColumns(t *testing.T) {
	s := NewTableSchema[boolSample]("t").
		ID("id").
		Field("Active", "active").
		Field("Verified", "verified").
		Field("Name", "name")
	got := s.BoolColumns()
	want := map[string]bool{"active": true, "verified": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BoolColumns() = %v, want %v", got, want)
	}
	// Type-less schema: no Go struct to reflect → nil.
	ext := NewExternalSchema("upstream").Field("Active", "active")
	if got := ext.BoolColumns(); got != nil {
		t.Errorf("external BoolColumns() = %v, want nil", got)
	}
	// A field without a struct index is skipped (defense-in-depth: the public
	// builder never yields index<0 on a type-anchored schema, so it is planted
	// directly here).
	s.fields = append(s.fields, schemaField{goName: "Ghost", column: "ghost"})
	if got := s.BoolColumns(); !reflect.DeepEqual(got, want) {
		t.Errorf("BoolColumns() with unindexed field = %v, want %v", got, want)
	}
}

func TestTableSchema_ScanPlan_ExportedPK(t *testing.T) {
	// schemaSample's ID is an exported struct field, so idIndex >= 0 and the ID
	// column leads the scan plan (the aggregate-child shape).
	s := NewTableSchema[schemaSample]("t").ID("id").Field("Name", "name")
	cols, byCol := s.ScanPlan()
	if !reflect.DeepEqual(cols, []string{"id", "name"}) {
		t.Fatalf("ScanPlan cols = %v, want [id name]", cols)
	}
	if !byCol["id"].equal(FieldPath{0}) || !byCol["name"].equal(FieldPath{1}) {
		t.Errorf("ScanPlan byCol = %v, want id→{0} name→{1}", byCol)
	}
}

func TestTableSchema_ValidateAnchored(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("anchored schema panicked: %v", r)
		}
	}()
	covFullSchema().ValidateAnchored() // type-anchored → no panic
	assertPanics(t, "type-less schema as a write-backed root", func() {
		NewExternalSchema("upstream").ID("id").Field("Name", "name").ValidateAnchored()
	})
}

func TestValidateModes_DeletedAtEnabledAllowsArchive(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ValidateModes with DeletedAt panicked: %v", r)
		}
	}()
	covFullSchema().ValidateModes([]domain.EntityMode{domain.ModeArchive, domain.ModeUnarchive})
}

// The remaining Sibling boot guards: a nil / type-less sibling, a sibling whose
// table duplicates the owner's, and the defense-in-depth guards over a sibling
// carrying children or nested siblings (unreachable through the public builder —
// Child()/Sibling() on a secondary panic first — so the state is planted
// directly on the in-package struct).
func TestTableSchema_Sibling_RemainingBootGuards(t *testing.T) {
	owner := func() *TableSchema { return NewTableSchema[schemaSample]("root").ID("id").Field("Name", "name") }

	assertPanics(t, "nil sibling", func() {
		owner().Sibling(nil)
	})
	assertPanics(t, "type-less sibling", func() {
		owner().Sibling(NewExternalSchema("s"))
	})
	assertPanics(t, "sibling table duplicates the owner table", func() {
		owner().Sibling(NewSiblingSchema[schemaSample]("root").Field("Removed", "removed"))
	})
	assertPanics(t, "sibling carrying a child", func() {
		sib := NewSiblingSchema[schemaSample]("s").Field("Removed", "removed")
		sib.children["embedFixture"] = NewTableSchema[embedFixture]("c").ID("id").ParentID("s_id")
		owner().Sibling(sib)
	})
	assertPanics(t, "sibling carrying a nested sibling", func() {
		sib := NewSiblingSchema[schemaSample]("s").Field("Removed", "removed")
		sib.siblings = append(sib.siblings, NewSiblingSchema[schemaSample]("s2").Field("Created", "created"))
		owner().Sibling(sib)
	})
}
