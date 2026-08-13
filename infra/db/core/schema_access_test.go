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
		ID("id").
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
		t.Error("HasPKDeclared() must be true after ID(...)")
	}
	if anchored.HasChildren() {
		t.Error("HasChildren() must be false for a flat schema")
	}
	if anchored.GoType() != reflect.TypeOf(labeledSample{}) {
		t.Errorf("GoType() = %v, want labeledSample", anchored.GoType())
	}
	if anchored.IDIndex() != 0 {
		t.Errorf("IDIndex() = %d, want 0 (exported ID field)", anchored.IDIndex())
	}

	ext := NewExternalSchema("upstream").Field("Name", "name")
	if !ext.IsExternal() {
		t.Error("NewExternalSchema must report IsExternal()")
	}
	if got := ext.TypeName(); got != "" {
		t.Errorf("external TypeName() = %q, want empty", got)
	}
	// A type-less source is never an aggregate child, so it occupies no
	// collection segment — the one case CollectionSegment() answers empty
	// instead of resolving a declaration.
	if got := ext.CollectionSegment(); got != "" {
		t.Errorf("external CollectionSegment() = %q, want empty", got)
	}
	if ext.HasPKDeclared() {
		t.Error("HasPKDeclared() must be false before ID(...)")
	}
	if ext.GoType() != nil {
		t.Error("external GoType() must be nil")
	}
	if ext.IDIndex() >= 0 {
		t.Errorf("external IDIndex() = %d, want < 0", ext.IDIndex())
	}

	withChild := NewTableSchema[schemaSample]("root").ID("id").
		Child(NewTableSchema[embedFixture]("child").ID("id").ParentID("root_id"))
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
	bare := NewTableSchema[labeledSample]("t").ID("id")
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
	s := NewTableSchema[schemaSample]("t").ID("id").Field("Name", "name")
	if got := s.LabelKeysByGoField(); len(got) != 0 {
		t.Errorf("LabelKeysByGoField() with no tags = %v, want empty", got)
	}
}

// roleAlpha / roleBeta are two roles of one shared identity: each carries the
// base's fields FLAT on its own struct, where the domain labels live. Alpha
// leaves Phone untagged and opts Email out; Beta labels Phone — so the
// first-anchor-wins resolution is observable across them.
type roleAlpha struct {
	Document string `labelKey:"alpha.document"`
	Name     string `labelKey:"alpha.name"`
	Email    string `labelKey:"-"`
	Phone    string
	Own      string `labelKey:"alpha.own"`
	Plain    string `labelKey:"alpha.plain"` // labeledSample leaves Plain untagged — the guard's probe
}

type roleBeta struct {
	Document string `labelKey:"beta.document"`
	Name     string `labelKey:"beta.name"`
	Phone    string `labelKey:"beta.phone"`
}

// unexportedAnchor pairs a lower-case logical field name (legal on a type-less
// schema) with an UNEXPORTED struct field: reflect surfaces it, but it is not
// part of the anchor's domain surface and must never supply a label.
type unexportedAnchor struct {
	note string `labelKey:"anchor.note"` //nolint:unused // read only via reflect in the guard test
}

// anchoredBase is the type-less shared base the two roles specialize. Only
// Document declares an inline label — the explicit declaration that must beat
// every anchor.
func anchoredBase() *TableSchema {
	return NewSharedBaseSchema("persons").
		ID("id").
		Field("Document", "document", "base.document").
		Field("Name", "name").
		Field("Email", "email").
		Field("Phone", "phone").
		NaturalID("document")
}

func TestLabelKeysByGoFieldAnchoredOn_RecoversTagsOffTheAnchor(t *testing.T) {
	base := anchoredBase()
	got := base.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(&roleAlpha{}))
	want := map[string]string{
		"Document": "base.document", // inline declaration beats the anchor tag
		"Name":     "alpha.name",    // recovered off the role struct
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("anchored labels = %v, want %v", got, want)
	}
	if _, has := got["Email"]; has {
		t.Error(`labelKey:"-" on the anchor must opt the field out`)
	}
	if _, has := got["Phone"]; has {
		t.Error("an untagged anchor field must not appear in the map")
	}
	// A value-type anchor resolves like its pointer, and a repeat call on the same
	// (schema, anchor) pair comes from the memo.
	again := base.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(roleAlpha{}))
	if !reflect.DeepEqual(again, want) {
		t.Errorf("value-type anchor = %v, want %v", again, want)
	}
	if cached := base.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(roleAlpha{})); !reflect.DeepEqual(cached, want) {
		t.Errorf("memoized anchored labels = %v, want %v", cached, want)
	}
}

func TestLabelKeysByGoFieldAnchoredOn_FirstAnchorDeclaringTheFieldWins(t *testing.T) {
	base := anchoredBase()
	got := base.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(&roleAlpha{}), reflect.TypeOf(&roleBeta{}))
	want := map[string]string{
		"Document": "base.document", // inline still wins over BOTH anchors
		"Name":     "alpha.name",    // first anchor declaring it
		"Phone":    "beta.phone",    // only the second anchor tags it
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("multi-anchor labels = %v, want %v", got, want)
	}
	// Order matters: Beta first flips the fields both roles tag.
	flipped := base.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(&roleBeta{}), reflect.TypeOf(&roleAlpha{}))
	if flipped["Name"] != "beta.name" {
		t.Errorf("Name = %q, want beta.name (first anchor in the new order)", flipped["Name"])
	}
	if _, has := flipped["Email"]; has {
		t.Error("a field no anchor declares must stay out of the map")
	}
}

func TestLabelKeysByGoFieldAnchoredOn_DegenerateAnchors(t *testing.T) {
	base := anchoredBase()
	// No anchor at all degrades to the schema's own inline labels.
	if got := base.LabelKeysByGoFieldAnchoredOn(); !reflect.DeepEqual(got, map[string]string{"Document": "base.document"}) {
		t.Errorf("no-anchor form = %v, want the inline map", got)
	}
	// A nil / non-struct anchor contributes nothing but must not panic.
	if got := base.LabelKeysByGoFieldAnchoredOn(nil, reflect.TypeOf("")); !reflect.DeepEqual(got, map[string]string{"Document": "base.document"}) {
		t.Errorf("degenerate anchors = %v, want the inline map only", got)
	}
	var nilSchema *TableSchema
	if got := nilSchema.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(&roleAlpha{})); got != nil {
		t.Errorf("nil schema = %v, want nil", got)
	}
	// An unexported anchor field is not domain surface — no label comes from it.
	notes := NewSharedBaseSchema("notes").ID("id").Field("note", "note")
	if got := notes.LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(unexportedAnchor{})); len(got) != 0 {
		t.Errorf("unexported anchor field = %v, want no label", got)
	}
	// A type-ANCHORED schema IGNORES the anchors entirely — its own struct is the
	// single source of its labels (which is why Field(...) boot-panics on an
	// inline label there). Name keeps its own tag, and Plain — untagged on the
	// schema's type but tagged on the anchor — stays unlabeled.
	anchored := labeledSchema().LabelKeysByGoFieldAnchoredOn(reflect.TypeOf(&roleAlpha{}))
	if anchored["Name"] != "person.name" {
		t.Errorf("type-anchored Name = %q, want person.name (own struct tag)", anchored["Name"])
	}
	if _, has := anchored["Plain"]; has {
		t.Errorf("an anchor must not inject a label into a type-anchored schema, got %v", anchored)
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
		t.Error("GoFieldValues must not include the ID")
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
