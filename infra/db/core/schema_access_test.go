package core

import (
	"reflect"
	"sort"
	"testing"
)

// Coverage for the exported schema accessors in schema_access.go — the pure
// reflection surface the Mongo read-side layer and the audit builder consume.
// All in-memory, no database.

// labeledSample is a type-anchored target carrying labelKey struct tags in every
// flavor labelKeysByGoField distinguishes: a real key, the explicit "-" opt-out,
// and no tag at all.
type labeledSample struct {
	ID    string
	Name  string `labelKey:"person.name"`
	Email string `labelKey:"-"`
	Plain string
}

func labeledSchema() *TableSchema {
	return NewTableSchema[labeledSample]("people").
		PK("id").
		Field("Name", "name").
		Field("Email", "email").
		Field("Plain", "plain").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

func TestSchemaAccess_KindAccessors(t *testing.T) {
	anchored := labeledSchema()
	if anchored.IsExternal() {
		t.Error("a type-anchored schema must not report IsExternal()")
	}
	if got := anchored.TypeName(); got != "labeledSample" {
		t.Errorf("TypeName() = %q, want labeledSample", got)
	}
	if !anchored.HasPKDeclared() {
		t.Error("HasPKDeclared() must be true after PK(...)")
	}
	if anchored.HasChildren() {
		t.Error("HasChildren() must be false for a flat schema")
	}
	if anchored.GoType() != reflect.TypeOf(labeledSample{}) {
		t.Errorf("GoType() = %v, want labeledSample", anchored.GoType())
	}
	if anchored.PKIndex() != 0 {
		t.Errorf("PKIndex() = %d, want 0 (exported ID field)", anchored.PKIndex())
	}

	ext := NewExternalSchema("upstream").Field("Name", "name")
	if !ext.IsExternal() {
		t.Error("NewExternalSchema must report IsExternal()")
	}
	if got := ext.TypeName(); got != "" {
		t.Errorf("external TypeName() = %q, want empty", got)
	}
	if ext.HasPKDeclared() {
		t.Error("HasPKDeclared() must be false before PK(...)")
	}
	if ext.GoType() != nil {
		t.Error("external GoType() must be nil")
	}
	if ext.PKIndex() >= 0 {
		t.Errorf("external PKIndex() = %d, want < 0", ext.PKIndex())
	}

	withChild := NewTableSchema[schemaSample]("root").PK("id").
		Child(NewTableSchema[embedFixture]("child").PK("id").FK("root_id"))
	if !withChild.HasChildren() {
		t.Error("HasChildren() must be true once a child is declared")
	}
}

func TestSchemaAccess_GoFieldsAndMappedColumns(t *testing.T) {
	s := labeledSchema()
	got := s.GoFields()
	want := []string{"Name", "Email", "Plain"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoFields() = %v, want %v (declaration order)", got, want)
	}
	cols := s.MappedColumns()
	sort.Strings(cols)
	wantCols := []string{"email", "name", "plain"}
	if !reflect.DeepEqual(cols, wantCols) {
		t.Errorf("MappedColumns() = %v, want %v", cols, wantCols)
	}
}

func TestSchemaAccess_ManagedTimestampColumns(t *testing.T) {
	s := labeledSchema()
	if got := s.CreatedAtColumn(); got != "created_at" {
		t.Errorf("CreatedAtColumn() = %q, want created_at", got)
	}
	if got := s.UpdatedAtColumn(); got != "updated_at" {
		t.Errorf("UpdatedAtColumn() = %q, want updated_at", got)
	}
	bare := NewTableSchema[labeledSample]("t").PK("id")
	if bare.CreatedAtColumn() != "" || bare.UpdatedAtColumn() != "" {
		t.Error("undeclared managed columns must read as empty")
	}
}

func TestLabelKeysByGoField_StructTags(t *testing.T) {
	s := labeledSchema()
	got := s.LabelKeysByGoField()
	if len(got) != 1 || got["Name"] != "person.name" {
		t.Errorf("LabelKeysByGoField() = %v, want map[Name:person.name]", got)
	}
	if _, has := got["Email"]; has {
		t.Error("labelKey:\"-\" must opt the field out of the catalog map")
	}
	if _, has := got["Plain"]; has {
		t.Error("a field without a labelKey tag must not appear in the map")
	}
	// Second call returns the memoized map (the cache-hit branch).
	again := s.LabelKeysByGoField()
	if !reflect.DeepEqual(again, got) {
		t.Errorf("cached LabelKeysByGoField() = %v, want %v", again, got)
	}
}

func TestLabelKeysByGoField_ExternalInlineKey(t *testing.T) {
	// A type-less schema has no struct tags; the inline schema-level labelKey is
	// the single source.
	ext := NewExternalSchema("upstream").
		Field("Name", "name", "upstream.name").
		Field("Plain", "plain")
	got := ext.LabelKeysByGoField()
	if len(got) != 1 || got["Name"] != "upstream.name" {
		t.Errorf("external LabelKeysByGoField() = %v, want map[Name:upstream.name]", got)
	}
}

func TestLabelKeysByGoField_NilAndEmpty(t *testing.T) {
	if got := labelKeysByGoField(nil); got != nil {
		t.Errorf("labelKeysByGoField(nil) = %v, want nil", got)
	}
	// No field carries any labelKey → the memoized map is empty (nil).
	s := NewTableSchema[schemaSample]("t").PK("id").Field("Name", "name")
	if got := s.LabelKeysByGoField(); len(got) != 0 {
		t.Errorf("LabelKeysByGoField() with no tags = %v, want empty", got)
	}
}

func TestGoFieldValues_ReadsByGoName(t *testing.T) {
	s := labeledSchema()
	e := &labeledSample{ID: "1", Name: "bob", Email: "b@x", Plain: "p"}
	got := s.GoFieldValues(e)
	want := map[string]any{"Name": "bob", "Email": "b@x", "Plain": "p"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GoFieldValues() = %v, want %v", got, want)
	}
	if _, has := got["ID"]; has {
		t.Error("GoFieldValues must not include the PK")
	}
	// A non-pointer entity exercises the deref loop's pass-through.
	byValue := s.GoFieldValues(labeledSample{Name: "alice"})
	if byValue["Name"] != "alice" {
		t.Errorf("GoFieldValues(value) [Name] = %v, want alice", byValue["Name"])
	}
}

func TestGoFieldValues_DegenerateInputs(t *testing.T) {
	var nilSchema *TableSchema
	if got := nilSchema.GoFieldValues(&labeledSample{}); len(got) != 0 {
		t.Errorf("nil schema GoFieldValues() = %v, want empty map", got)
	}
	s := labeledSchema()
	if got := s.GoFieldValues(42); len(got) != 0 {
		t.Errorf("non-struct GoFieldValues() = %v, want empty map", got)
	}
	// External fields carry index < 0 and are skipped.
	ext := NewExternalSchema("upstream").Field("Name", "name")
	if got := ext.GoFieldValues(struct{ Name string }{Name: "x"}); len(got) != 0 {
		t.Errorf("external GoFieldValues() = %v, want empty map", got)
	}
}
