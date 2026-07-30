package core

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// TableSchema resolution-helper + modeName branches. Relocated from the former
// infra-root coverage grab-bag once table_schema.go moved to package db (these
// exercise unexported methods, so they must live alongside the code).

// covFullSchema returns a type-anchored schema exercising the ID + a mapped field
// + all three managed columns, reused across the resolution-helper tests.
func covFullSchema() *TableSchema {
	return NewTableSchema[schemaSample]("t").
		ID("id").
		Field("Name", "name").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		DeletedAt("deleted_at")
}

func TestTableSchema_PKAndManagedColumns(t *testing.T) {
	s := covFullSchema()
	if s.IDColumn() != "id" {
		t.Errorf("IDColumn = %q, want id", s.IDColumn())
	}
	if c, ok := s.createdAtColumn(); !ok || c != "created_at" {
		t.Errorf("createdAtColumn = (%q,%v), want (created_at,true)", c, ok)
	}
	if c, ok := s.updatedAtColumn(); !ok || c != "updated_at" {
		t.Errorf("updatedAtColumn = (%q,%v), want (updated_at,true)", c, ok)
	}
}

func TestTableSchema_NowColumns(t *testing.T) {
	s := covFullSchema()
	insert := s.insertNowColumns()
	if len(insert) != 2 || insert[0] != "created_at" || insert[1] != "updated_at" {
		t.Errorf("insertNowColumns = %v, want [created_at updated_at]", insert)
	}
	update := s.updateNowColumns()
	if len(update) != 1 || update[0] != "updated_at" {
		t.Errorf("updateNowColumns = %v, want [updated_at]", update)
	}
}

func TestTableSchema_NowColumns_WhenManagedAbsent(t *testing.T) {
	s := NewTableSchema[schemaSample]("t").ID("id").Field("Name", "name")
	if got := s.insertNowColumns(); got != nil {
		t.Errorf("insertNowColumns with no managed cols = %v, want nil", got)
	}
	if got := s.updateNowColumns(); got != nil {
		t.Errorf("updateNowColumns with no managed cols = %v, want nil", got)
	}
	if _, ok := s.createdAtColumn(); ok {
		t.Error("createdAtColumn should be disabled")
	}
	if _, ok := s.updatedAtColumn(); ok {
		t.Error("updatedAtColumn should be disabled")
	}
}

func TestTableSchema_WriteFields_ExcludesPKAndManaged(t *testing.T) {
	s := covFullSchema()
	got := s.writeFields(&schemaSample{ID: "x", Name: "bob", Created: "c", Updated: "u", Removed: "r"})
	if len(got) != 1 {
		t.Fatalf("writeFields = %v, want only the mapped Name column", got)
	}
	if got["name"] != "bob" {
		t.Errorf("writeFields[name] = %v, want bob", got["name"])
	}
	if _, present := got["id"]; present {
		t.Error("writeFields must exclude the ID column")
	}
	if _, present := got["created_at"]; present {
		t.Error("writeFields must exclude managed columns")
	}
}

func TestTableSchema_WriteFields_AcceptsValue(t *testing.T) {
	// Non-pointer entity exercises the deref loop's pass-through branch.
	s := covFullSchema()
	got := s.writeFields(schemaSample{Name: "alice"})
	if got["name"] != "alice" {
		t.Errorf("writeFields[name] = %v, want alice", got["name"])
	}
}

func TestTableSchema_GoNameForRead(t *testing.T) {
	s := covFullSchema()
	cases := []struct {
		col   string
		want  string
		found bool
	}{
		{"name", "Name", true},
		{"id", "ID", true},
		{"created_at", "CreatedAt", true},
		{"updated_at", "UpdatedAt", true},
		{"deleted_at", "DeletedAt", true},
		{"unknown_col", "", false},
	}
	for _, c := range cases {
		got, ok := s.goNameForRead(c.col)
		if got != c.want || ok != c.found {
			t.Errorf("goNameForRead(%q) = (%q,%v), want (%q,%v)", c.col, got, ok, c.want, c.found)
		}
	}
}

func TestTableSchema_ColumnForRead(t *testing.T) {
	s := covFullSchema()
	cases := []struct {
		goName string
		want   string
		found  bool
	}{
		{"Name", "name", true},
		{"ID", "id", true},
		{"CreatedAt", "created_at", true},
		{"UpdatedAt", "updated_at", true},
		{"DeletedAt", "deleted_at", true},
		{"Unknown", "", false},
	}
	for _, c := range cases {
		got, ok := s.columnForRead(c.goName)
		if got != c.want || ok != c.found {
			t.Errorf("columnForRead(%q) = (%q,%v), want (%q,%v)", c.goName, got, ok, c.want, c.found)
		}
	}
}

func TestTableSchema_ReadHelpers_ManagedAbsentMissing(t *testing.T) {
	// Without managed columns, the fixed logical names resolve to ok=false.
	s := NewTableSchema[schemaSample]("t").ID("id").Field("Name", "name")
	for _, col := range []string{"created_at", "updated_at", "deleted_at"} {
		if _, ok := s.goNameForRead(col); ok {
			t.Errorf("goNameForRead(%q) should be false when managed columns are absent", col)
		}
	}
	for _, name := range []string{"CreatedAt", "UpdatedAt", "DeletedAt"} {
		if _, ok := s.columnForRead(name); ok {
			t.Errorf("columnForRead(%q) should be false when managed columns are absent", name)
		}
	}
}

func TestTableSchema_ChildSchema_NilReceiverAndUnknown(t *testing.T) {
	var nilSchema *TableSchema
	if nilSchema.childSchema("anything") != nil {
		t.Error("childSchema on nil receiver must return nil")
	}
	s := covFullSchema()
	if s.childSchema("Nonexistent") != nil {
		t.Error("childSchema for an undeclared type must return nil")
	}
}

func TestTableSchema_TypeName(t *testing.T) {
	if got := covFullSchema().typeName(); got != "schemaSample" {
		t.Errorf("typeName = %q, want schemaSample", got)
	}
	ext := NewExternalSchema("users").ID("id").Field("Name", "name")
	if got := ext.typeName(); got != "" {
		t.Errorf("external typeName = %q, want empty", got)
	}
	if !ext.isExternal() {
		t.Error("NewExternalSchema must report isExternal = true")
	}
	if covFullSchema().isExternal() {
		t.Error("type-anchored schema must report isExternal = false")
	}
}

func TestModeName(t *testing.T) {
	if got := modeName(domain.ModeArchive); got != "ModeArchive" {
		t.Errorf("modeName(ModeArchive) = %q", got)
	}
	if got := modeName(domain.ModeUnarchive); got != "ModeUnarchive" {
		t.Errorf("modeName(ModeUnarchive) = %q", got)
	}
}
