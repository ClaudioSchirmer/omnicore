package query

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Fixtures for the JoinView Fields allowlist: a source view with two business
// fields, a DeletedAt column and one native child, so entries of every kind
// (business field, managed slot, top-level segment) are exercised.

type fieldsSrcRoot struct {
	ID    string
	Name  string
	Email string
}

type fieldsSrcNote struct {
	ID   string
	Note string
}

func fieldsSrcNoteSchema() *core.TableSchema {
	return core.NewTableSchema[fieldsSrcNote]("fields_src_notes").
		ID("id").ParentID("src_id").Field("Note", "note")
}

func fieldsSourceView() *ViewDefinition {
	return View("fields_src").Version(3).
		Schema(core.NewTableSchema[fieldsSrcRoot]("fields_src_tbl").
			ID("id").
			Field("Name", "name").
			Field("Email", "mail").
			DeletedAt("removed_on").
			Child(fieldsSrcNoteSchema()))
}

// fieldsEmbedder wires a 1:1 embed of the source with the given leg, index
// included (the ripple's reverse-scan requirement).
func fieldsEmbedder(leg *Leg) *ViewDefinition {
	return View("fields_dep").Version(1).Schema(rootSchema("fields_dep_tbl")).
		Embed(leg).On("src_ref").
		Indexes(Index("src_ref"))
}

func noteSegment() string { return childDocSegment(fieldsSrcNoteSchema()) }

// ─── declaration panics ──────────────────────────────────────────────────────

func TestLegFields_DeclarationPanics(t *testing.T) {
	cases := map[string]func(){
		"empty list":   func() { JoinView(fieldsSourceView(), "S", "s").Fields() },
		"empty entry":  func() { JoinView(fieldsSourceView(), "S", "s").Fields("") },
		"duplicate":    func() { JoinView(fieldsSourceView(), "S", "s").Fields("Name", "Name") },
		"reserved _id": func() { JoinView(fieldsSourceView(), "S", "s").Fields("_id") },
		"reserved _revision": func() {
			JoinView(fieldsSourceView(), "S", "s").Fields("Name", "_revision")
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s must panic at declaration", name)
				}
			}()
			fn()
		})
	}
}

// ─── boot validation ─────────────────────────────────────────────────────────

func TestLegFields_JoinUpstreamLegRejected(t *testing.T) {
	leg := extLeg("upstream_things", "Thing", "thing").Fields("name")
	v := fieldsEmbedder(leg)
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "JoinUpstream leg") {
		t.Fatalf("Fields on a JoinUpstream leg must be a boot error, got: %v", err)
	}
}

func TestLegFields_UnknownEntryRejected(t *testing.T) {
	src := fieldsSourceView()
	leg := JoinView(src, "Src", "src").Fields("Nope")
	v := fieldsEmbedder(leg)
	err := ValidateViewSchemas([]*ViewDefinition{src, v})
	if err == nil || !strings.Contains(err.Error(), `Fields entry "Nope"`) {
		t.Fatalf("an unknown Fields entry must be a boot error, got: %v", err)
	}
}

func TestLegFields_ValidEntriesPass(t *testing.T) {
	src := fieldsSourceView()
	// A business field by Go name, the fixed managed name, and a top-level
	// segment (the source's native child) — every entry kind at once.
	leg := JoinView(src, "Src", "src").Fields("Name", "DeletedAt", noteSegment())
	v := fieldsEmbedder(leg)
	if err := ValidateViewSchemas([]*ViewDefinition{src, v}); err != nil {
		t.Fatalf("valid Fields entries must pass boot validation, got: %v", err)
	}
}

func TestComposedView_RejectsFieldsLeg(t *testing.T) {
	src := fieldsSourceView()
	primary := View("fields_primary").Version(1).Schema(rootSchema("fields_primary_tbl"))
	c := ComposedView("fields_composed").
		Primary(primary).
		Link(JoinView(src, "Src", "src").Fields("Name")).On("id")
	err := ValidateComposedViews([]*ComposedViewDefinition{c}, []*ViewDefinition{src, primary}, nil)
	if err == nil || !strings.Contains(err.Error(), "Fields is available only") {
		t.Fatalf("a Fields-bearing leg on a ComposedView must be a boot error, got: %v", err)
	}
}

// ─── trim set + trim ─────────────────────────────────────────────────────────

func TestEmbedTrimSet_TranslatesAndForces(t *testing.T) {
	src := fieldsSourceView()
	e := embedDef{
		leg:     JoinView(src, "Src", "src").Fields("Name", noteSegment()),
		many:    true,
		joinCol: "src_ref",
		orderBy: "mail",
	}
	set := embedTrimSet(e)
	for _, want := range []string{"name", "id", "src_ref", "mail", noteSegment()} {
		if _, ok := set[want]; !ok {
			t.Errorf("trim set must contain %q, got %v", want, set)
		}
	}
	if _, ok := set["removed_on"]; ok {
		t.Errorf("DeletedAt column not declared must NOT be in the trim set: %v", set)
	}
}

func TestTrimToFields_KeepsReservedAndAllowed(t *testing.T) {
	src := fieldsSourceView()
	e := embedDef{leg: JoinView(src, "Src", "src").Fields("Name")}
	doc := Document{
		"_id": "x", "_revision": int64(7),
		"name": "Ana", "mail": "a@b", "removed_on": "2026-01-01", noteSegment(): []any{},
	}
	got := trimToFields(doc, embedTrimSet(e))
	if got["_id"] != "x" || got["_revision"] != int64(7) || got["name"] != "Ana" {
		t.Errorf("reserved + allowed fields must survive, got %v", got)
	}
	if _, has := got["mail"]; has {
		t.Errorf("capped field must be trimmed, got %v", got)
	}
	if _, has := got["removed_on"]; has {
		t.Errorf("capped DeletedAt column must be trimmed, got %v", got)
	}
	if _, has := got[noteSegment()]; has {
		t.Errorf("unlisted segment must be cut whole, got %v", got)
	}
	if _, mutated := doc["name"]; !mutated || len(doc) != 6 {
		t.Errorf("input document must not be mutated, got %v", doc)
	}
}

func TestTrimToFields_NilSetPassthrough(t *testing.T) {
	doc := Document{"a": 1}
	if got := trimToFields(doc, nil); len(got) != 1 {
		t.Errorf("nil allowset must return the document untouched, got %v", got)
	}
}

func TestSurgicalElement_Trims(t *testing.T) {
	src := fieldsSourceView()
	e := embedDef{leg: JoinView(src, "Src", "src").Fields("Name"), joinCol: "src_ref"}
	elem := surgicalElement(e, "id-1", Document{"name": "Ana", "mail": "a@b", "removed_on": "x"})
	if elem["_id"] != "id-1" || elem["name"] != "Ana" {
		t.Errorf("element must carry _id + allowed fields, got %v", elem)
	}
	if _, has := elem["mail"]; has {
		t.Errorf("capped field must not reach the surgical element: %v", elem)
	}
}

// ─── hash ────────────────────────────────────────────────────────────────────

func TestRebuildHash_FieldsConditional(t *testing.T) {
	build := func(fields ...string) *ViewDefinition {
		src := fieldsSourceView()
		leg := JoinView(src, "Src", "src")
		if len(fields) > 0 {
			leg = leg.Fields(fields...)
		}
		return fieldsEmbedder(leg)
	}
	plain, plain2 := build(), build()
	if plain.RebuildHash() != plain2.RebuildHash() {
		t.Fatalf("identical declarations must hash identically")
	}
	withFields := build("Name")
	if withFields.RebuildHash() == plain.RebuildHash() {
		t.Errorf("declaring Fields must move the rebuild hash")
	}
	other := build("Name", "Email")
	if other.RebuildHash() == withFields.RebuildHash() {
		t.Errorf("changing Fields must move the rebuild hash")
	}
}

// ─── read side ───────────────────────────────────────────────────────────────

func TestLegViewNode_FieldsGateDeletedAt(t *testing.T) {
	src := fieldsSourceView()
	capped := legViewNode(JoinView(src, "Src", "src").Fields("Name"))
	if _, ok := capped.DeletedAtColumn(); ok {
		t.Errorf("a capped DeletedAt column must report NO archived gate (the archive switch)")
	}
	kept := legViewNode(JoinView(src, "Src", "src").Fields("Name", "DeletedAt"))
	if col, ok := kept.DeletedAtColumn(); !ok || col != "removed_on" {
		t.Errorf("DeletedAt listed → the gate stays on the physical column, got %q/%v", col, ok)
	}
}

func TestLegViewNode_FieldsGateColumnPath(t *testing.T) {
	src := fieldsSourceView()
	node := legViewNode(JoinView(src, "Src", "src").Fields("Name"))
	if p, ok := node.ColumnPath([]string{"Name"}); !ok || p[0] != "name" {
		t.Errorf("declared entry must translate, got %v/%v", p, ok)
	}
	if _, ok := node.ColumnPath([]string{"ID"}); !ok {
		t.Errorf("ID is always materialized, so it must always translate")
	}
	if _, ok := node.ColumnPath([]string{"Email"}); ok {
		t.Errorf("a capped field must be UNKNOWN on the restricted node (the wire's 400)")
	}
	if _, ok := node.ColumnPath([]string{noteSegment(), "Note"}); ok {
		t.Errorf("an unlisted segment must be cut from translation too")
	}
	admitted := legViewNode(JoinView(src, "Src", "src").Fields("Name", noteSegment()))
	if p, ok := admitted.ColumnPath([]string{noteSegment(), "Note"}); !ok || p[1] != "note" {
		t.Errorf("an admitted segment translates whole, got %v/%v", p, ok)
	}
}

func TestChildDeletedAtPaths_CappedSegmentContributesNothing(t *testing.T) {
	src := fieldsSourceView()
	capped := fieldsEmbedder(JoinView(src, "Src", "src").Fields("Name")).BuildViewNode()
	if paths := capped.ChildDeletedAtPaths(); len(paths) != 0 {
		t.Errorf("a capped segment has no archived rule — no auto-include path, got %v", paths)
	}
	kept := fieldsEmbedder(JoinView(src, "Src", "src").Fields("Name", "DeletedAt")).BuildViewNode()
	if paths := kept.ChildDeletedAtPaths(); paths["src"] != "removed_on" {
		t.Errorf("DeletedAt listed → the segment contributes its strip path, got %v", paths)
	}
}

func TestStripArchivedChildren_CappedSegmentNeverStripped(t *testing.T) {
	src := fieldsSourceView()
	capped := fieldsEmbedder(JoinView(src, "Src", "src").Fields("Name")).BuildViewNode()
	// Even a segment that (hypothetically) carried the archived stamp is left
	// alone: the rule is the DECLARATION, and this leg declares no archive rule.
	doc := map[string]any{"src": map[string]any{"_id": "s1", "name": "Ana", "removed_on": "2026-01-01"}}
	capped.StripArchivedChildren(doc)
	if doc["src"] == nil {
		t.Fatalf("capped segment must never be stripped")
	}
	kept := fieldsEmbedder(JoinView(src, "Src", "src").Fields("Name", "DeletedAt")).BuildViewNode()
	doc2 := map[string]any{"src": map[string]any{"_id": "s1", "name": "Ana", "removed_on": "2026-01-01"}}
	kept.StripArchivedChildren(doc2)
	if doc2["src"] != nil {
		t.Fatalf("DeletedAt listed → the archived segment hides on a default read, got %v", doc2["src"])
	}
}

// ─── export ──────────────────────────────────────────────────────────────────

func TestExportPlan_FieldsRestrictBranch(t *testing.T) {
	src := fieldsSourceView()
	v := fieldsEmbedder(JoinView(src, "Src", "src").Fields("Name"))
	plan := v.ExportPlan()
	var branch bool
	for _, ch := range plan.Root.Children {
		if ch.GoSegment != "Src" {
			continue
		}
		branch = true
		for _, c := range ch.Columns {
			if c.GoField != "Name" {
				t.Errorf("capped column %q must not be advertised by the export", c.GoField)
			}
		}
		if len(ch.Children) != 0 {
			t.Errorf("unlisted segments must be cut from the export branch, got %v", ch.Children)
		}
	}
	if !branch {
		t.Fatalf("embed branch missing from export plan")
	}
}
