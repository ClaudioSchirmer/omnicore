package query

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// ─── composed-view test fixtures ─────────────────────────────────────────────

type composedGadget struct{ ID, Code, MirrorID string }

// Note anchors the 1:N leg schema; the LinkMany segment is named "Notes".
type Note struct{ ID, GadgetID, Text string }

func cvPrimarySchema() *core.TableSchema {
	return core.NewTableSchema[composedGadget]("gadgets").
		ID("id").
		Field("Code", "code").
		Field("MirrorID", "mirror_id").
		SoftDelete("deleted_at")
}

func cvPrimaryView() *ViewDefinition {
	return View("gadgets").Version(1).Schema(cvPrimarySchema())
}

func cvNotesSchema() *core.TableSchema {
	return core.NewTableSchema[Note]("gadget_notes").
		ID("id").
		Field("GadgetID", "gadget_id").
		Field("Text", "text").
		SoftDelete("deleted_at")
}

func cvNotesView() *ViewDefinition {
	return View("gadget_notes").Version(1).Schema(cvNotesSchema()).
		Indexes(Index("gadget_id")) // the LinkMany ParentID must be index-covered (boot-enforced)
}

func cvUpstreamSchema() *core.TableSchema {
	return core.NewExternalSchema("upstream_gadgets").ID("id").Field("Code", "code")
}

func cvValidComposed() *ComposedViewDefinition {
	return ComposedView("gadgets_full").
		Primary(cvPrimaryView()).
		Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("id").
		LinkMany(JoinView(cvNotesView(), "Notes", "notes")).
		OrderBy("text").Desc().MaxLinkManyLimit(5).On("gadget_id")
}

func cvRegistered() []*ViewDefinition {
	return []*ViewDefinition{cvPrimaryView(), cvNotesView()}
}

func cvUpstreams() map[string]bool {
	return map[string]bool{"upstream_gadgets": true}
}

func validateOne(c *ComposedViewDefinition, views []*ViewDefinition, ups map[string]bool) error {
	return ValidateComposedViews([]*ComposedViewDefinition{c}, views, ups)
}

func wantProblem(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error containing %q, got nil", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("expected error to contain %q, got:\n%s", fragment, err.Error())
	}
}

func wantPanic(t *testing.T, fragment string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", fragment)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, fragment) {
			t.Fatalf("expected panic to contain %q, got: %v", fragment, r)
		}
	}()
	fn()
}

// ─── declaration-time panics ─────────────────────────────────────────────────

func TestComposedView_DeclarationPanics(t *testing.T) {
	wantPanic(t, ".Primary(nil)", func() { ComposedView("x").Primary(nil) })
	wantPanic(t, "declared twice", func() {
		ComposedView("x").Primary(cvPrimaryView()).Primary(cvPrimaryView())
	})
	wantPanic(t, ".Link(nil)", func() { ComposedView("x").Link(nil) })
	wantPanic(t, ".LinkMany(nil)", func() { ComposedView("x").LinkMany(nil) })
	wantPanic(t, "JoinView(nil)", func() { JoinView(nil, "G", "g") })
	wantPanic(t, "JoinUpstream(nil)", func() { JoinUpstream(nil, "G", "g") })
	wantPanic(t, "write-anchored", func() { JoinUpstream(cvNotesSchema(), "G", "g") })
	// goName / externalName are both mandatory on the leg constructors.
	wantPanic(t, "mandatory", func() { JoinUpstream(cvUpstreamSchema(), "G", "") })
	wantPanic(t, "mandatory", func() { JoinView(cvNotesView(), "", "g") })
}

// ─── boot validation ─────────────────────────────────────────────────────────

func TestValidateComposedViews_HappyPath(t *testing.T) {
	if err := validateOne(cvValidComposed(), cvRegistered(), cvUpstreams()); err != nil {
		t.Fatalf("expected valid composed view, got: %v", err)
	}
}

func TestValidateComposedViews_Rejections(t *testing.T) {
	cases := []struct {
		name     string
		composed *ComposedViewDefinition
		views    []*ViewDefinition
		ups      map[string]bool
		fragment string
	}{
		{
			name:     "empty name",
			composed: ComposedView("").Primary(cvPrimaryView()),
			views:    cvRegistered(), ups: cvUpstreams(),
			fragment: "empty name",
		},
		{
			name:     "name collides with a view",
			composed: ComposedView("gadgets").Primary(cvPrimaryView()).LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On("gadget_id"),
			views:    cvRegistered(), ups: cvUpstreams(),
			fragment: "collides with a registered view",
		},
		{
			name:     "no primary",
			composed: ComposedView("gadgets_full"),
			views:    cvRegistered(), ups: cvUpstreams(),
			fragment: "no .Primary",
		},
		{
			name:     "primary not registered",
			composed: ComposedView("gadgets_full").Primary(View("ghosts").Version(1).Schema(rootSchema("ghosts"))).LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On("gadget_id"),
			views:    cvRegistered(), ups: cvUpstreams(),
			fragment: "is not registered",
		},
		{
			name:     "no links",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()),
			views:    cvRegistered(), ups: cvUpstreams(),
			fragment: "declares no .Link",
		},
		{
			name: "segment collides with a primary field",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				Link(JoinUpstream(cvUpstreamSchema(), "Code", "code")).On("id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "already produced by a primary root field",
		},
		{
			name: "duplicate link segment",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				Link(JoinUpstream(cvUpstreamSchema(), "Mirror", "a")).On("id").
				Link(JoinUpstream(cvUpstreamSchema(), "Mirror", "b")).On("id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "each segment has exactly one source",
		},
		{
			name: "internal leg not registered",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(View("ghosts").Version(1).Schema(cvNotesSchema()), "Ghosts", "ghosts")).On("gadget_id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "which is not registered",
		},
		{
			name: "external leg without subscription",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("id"),
			views: cvRegistered(), ups: map[string]bool{},
			fragment: "no UpstreamSubscription materializes it",
		},
		{
			name: "external leg without ID",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				Link(JoinUpstream(core.NewExternalSchema("upstream_gadgets").Field("Code", "code"), "UpstreamMirror", "upstreamMirror")).On("id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "declares no primary key",
		},
		{
			name: "empty join column",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On(""),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "empty join column",
		},
		{
			name: "1:N join column not on leg schema",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On("bogus_fk"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "does not exist on the leg schema",
		},
		{
			name: "1:1 join column not on primary schema",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("bogus_fk"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "does not exist on the primary schema",
		},
		{
			name: "OrderBy column not on leg schema",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(cvNotesView(), "Notes", "notes")).OrderBy("bogus").On("gadget_id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "OrderBy column \"bogus\" does not exist",
		},
		{
			name: "Desc without OrderBy",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(cvNotesView(), "Notes", "notes")).Desc().On("gadget_id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: ".Desc() without .OrderBy",
		},
		{
			name: "negative MaxLinkManyLimit",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(cvNotesView(), "Notes", "notes")).MaxLinkManyLimit(-1).On("gadget_id"),
			views: cvRegistered(), ups: cvUpstreams(),
			fragment: "negative MaxLinkManyLimit",
		},
		{
			name: "LinkMany ParentID without a covering index",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryView()).
				LinkMany(JoinView(
					View("gadget_notes").Version(1).Schema(cvNotesSchema()), "Notes", "notes",
				)).On("gadget_id"),
			views:    []*ViewDefinition{cvPrimaryView(), View("gadget_notes").Version(1).Schema(cvNotesSchema())},
			ups:      cvUpstreams(),
			fragment: "NO covering index",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantProblem(t, validateOne(tc.composed, tc.views, tc.ups), tc.fragment)
		})
	}
}

func TestValidateComposedViews_DuplicateComposedName(t *testing.T) {
	err := ValidateComposedViews(
		[]*ComposedViewDefinition{cvValidComposed(), cvValidComposed()},
		cvRegistered(), cvUpstreams())
	wantProblem(t, err, "declared more than once")
}

func TestValidateComposedViews_SegmentCollidesWithDerivedSegment(t *testing.T) {
	// The primary declares an external embed landing on Go segment "Mirror";
	// a link claiming the same segment is a boot error.
	primary := View("gadgets").Version(1).Schema(cvPrimarySchema()).
		Embed(extLeg("mirrors", "Mirror", "mirror")).On("id")
	composed := ComposedView("gadgets_full").Primary(primary).
		Link(JoinUpstream(cvUpstreamSchema(), "Mirror", "mirror2")).On("id")
	err := validateOne(composed, []*ViewDefinition{primary, cvNotesView()}, cvUpstreams())
	wantProblem(t, err, "already produced by a primary document segment")
}

// ─── Links() projection ──────────────────────────────────────────────────────

func TestComposedView_LinksProjection(t *testing.T) {
	links := cvValidComposed().Links()
	if len(links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(links))
	}

	mirror := links[0]
	if mirror.GoSegment != "UpstreamMirror" || mirror.DocField != "upstreamMirror" {
		t.Fatalf("unexpected mirror segment naming: %+v", mirror)
	}
	if mirror.Many || !mirror.External {
		t.Fatalf("mirror should be a 1:1 external link: %+v", mirror)
	}
	if mirror.Collection != "upstream_gadgets" {
		t.Fatalf("unexpected mirror collection %q", mirror.Collection)
	}
	// ParentID "id" is the primary's ID → the parent join value is the item's _id.
	if mirror.ParentKeyGoField != "_id" {
		t.Fatalf("ID-joined 1:1 link must read the parent key from _id, got %q", mirror.ParentKeyGoField)
	}
	if mirror.Node() == nil {
		t.Fatal("mirror link carries no translator node")
	}

	notes := links[1]
	if notes.GoSegment != "Notes" || !notes.Many || notes.External {
		t.Fatalf("unexpected notes link: %+v", notes)
	}
	if notes.Collection != "gadget_notes" || notes.ParentIDColumn != "gadget_id" {
		t.Fatalf("unexpected notes join: %+v", notes)
	}
	if notes.ParentKeyGoField != "_id" {
		t.Fatalf("a 1:N link always joins on the parent _id, got %q", notes.ParentKeyGoField)
	}
	if notes.OrderByColumn != "text" || !notes.OrderByDesc {
		t.Fatalf("unexpected notes order: %+v", notes)
	}
}

func TestComposedView_LinkParentKeyFromNonPKColumn(t *testing.T) {
	composed := ComposedView("gadgets_full").Primary(cvPrimaryView()).
		Link(JoinUpstream(cvUpstreamSchema(), "UpstreamMirror", "upstreamMirror")).On("mirror_id")
	if err := validateOne(composed, cvRegistered(), cvUpstreams()); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	links := composed.Links()
	if links[0].ParentKeyGoField != "MirrorID" {
		t.Fatalf("expected the Go name of the primary ParentID column, got %q", links[0].ParentKeyGoField)
	}
}

// ─── ceilings ────────────────────────────────────────────────────────────────

func TestComposedLink_ResolveMaxLinkManyLimit(t *testing.T) {
	withCap := cvValidComposed().Links()[1] // MaxLinkManyLimit(5)
	if got := withCap.ResolveMaxLinkManyLimit(30); got != 5 {
		t.Fatalf("per-link value must win, got %d", got)
	}
	noCap := ComposedView("x").Primary(cvPrimaryView()).
		LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On("gadget_id").Links()[0]
	if got := noCap.ResolveMaxLinkManyLimit(30); got != 30 {
		t.Fatalf("yaml default must win when the link is silent, got %d", got)
	}
	if got := noCap.ResolveMaxLinkManyLimit(0); got != FrameworkDefaultMaxLinkManyLimit {
		t.Fatalf("framework default must close the cascade, got %d", got)
	}
}

// ─── export plan ─────────────────────────────────────────────────────────────

func TestComposedView_ExportPlan(t *testing.T) {
	plan := cvValidComposed().ExportPlan()
	if plan == nil || plan.Root == nil {
		t.Fatal("nil export plan")
	}
	var mirror, notes bool
	for _, child := range plan.Root.Children {
		switch child.GoSegment {
		case "UpstreamMirror":
			mirror = true
			if child.WireSegment != "upstreamMirror" {
				t.Fatalf("unexpected mirror wire segment %q", child.WireSegment)
			}
			if len(child.Columns) == 0 {
				t.Fatal("mirror branch carries no columns")
			}
		case "Notes":
			notes = true
			if child.WireSegment != "notes" {
				t.Fatalf("unexpected notes wire segment %q", child.WireSegment)
			}
		}
	}
	if !mirror || !notes {
		t.Fatalf("export plan is missing leg branches (mirror=%v notes=%v)", mirror, notes)
	}
	// Wire→Go paths must resolve through the leg branches like any embed.
	paths := plan.WireToGoPaths()
	if paths["notes.text"] != "Notes.Text" {
		t.Fatalf("expected notes.text → Notes.Text, got %q", paths["notes.text"])
	}
	if paths["upstreamMirror.code"] != "UpstreamMirror.Code" {
		t.Fatalf("expected upstreamMirror.code → UpstreamMirror.Code, got %q", paths["upstreamMirror.code"])
	}
}

func TestComposedView_ResolveMaxExportRowsDelegatesToPrimary(t *testing.T) {
	primary := cvPrimaryView().MaxExportRows(7)
	composed := ComposedView("gadgets_full").Primary(primary).
		LinkMany(JoinView(cvNotesView(), "Notes", "notes")).On("gadget_id")
	if got := composed.ResolveMaxExportRows(999); got != 7 {
		t.Fatalf("expected the primary's export ceiling, got %d", got)
	}
}

// ─── name accessor ───────────────────────────────────────────────────────────

func TestComposedView_Accessors(t *testing.T) {
	c := cvValidComposed()
	if c.Name() != "gadgets_full" {
		t.Fatalf("unexpected name %q", c.Name())
	}
	if c.PrimaryView() == nil || c.PrimaryView().Name() != "gadgets" {
		t.Fatal("unexpected primary view")
	}
}

// ─── LinkInChild ─────────────────────────────────────────────────────────────

type cvLine struct{ ID, GadgetID, ItemID string }

func cvLineSchema() *core.TableSchema {
	return core.NewTableSchema[cvLine]("gadget_lines").
		ID("id").ParentID("gadget_id").
		Field("ItemID", "item_id")
}

func cvPrimaryWithChildSchema() *core.TableSchema {
	return core.NewTableSchema[composedGadget]("gadgets").
		ID("id").Field("Code", "code").Field("MirrorID", "mirror_id").
		SoftDelete("deleted_at").
		Child(cvLineSchema())
}

func cvPrimaryWithChild() *ViewDefinition {
	return View("gadgets").Version(1).Schema(cvPrimaryWithChildSchema())
}

// cvValidInChild enriches each gadget_lines element with its upstream item (1:1,
// keyed by the element's item_id → upstream_gadgets._id). No covering index is
// required (LinkInChild is read-time, no ripple).
func cvValidInChild() *ComposedViewDefinition {
	return ComposedView("gadgets_full").
		Primary(cvPrimaryWithChild()).
		LinkInChild(cvLineSchema(), JoinUpstream(cvUpstreamSchema(), "Item", "item")).On("item_id")
}

func TestComposedView_LinkInChild_DeclarationPanics(t *testing.T) {
	wantPanic(t, ".LinkInChild(nil leg)", func() { ComposedView("x").LinkInChild(cvLineSchema(), nil) })
	wantPanic(t, ".LinkInChild(nil childSchema)", func() {
		ComposedView("x").LinkInChild(nil, JoinUpstream(cvUpstreamSchema(), "Item", "item"))
	})
}

func TestValidateComposedViews_LinkInChild_HappyPath_External(t *testing.T) {
	// No Indexes(...) declared — proves LinkInChild needs no covering index.
	if err := validateOne(cvValidInChild(), []*ViewDefinition{cvPrimaryWithChild()}, cvUpstreams()); err != nil {
		t.Fatalf("valid external LinkInChild must pass, got: %v", err)
	}
}

func TestValidateComposedViews_LinkInChild_HappyPath_InternalView(t *testing.T) {
	// A JoinView leg is accepted (unlike EmbedInChild, which is external-only).
	composed := ComposedView("gadgets_full").
		Primary(cvPrimaryWithChild()).
		LinkInChild(cvLineSchema(), JoinView(cvNotesView(), "Line", "line")).On("item_id")
	views := []*ViewDefinition{cvPrimaryWithChild(), cvNotesView()}
	if err := validateOne(composed, views, cvUpstreams()); err != nil {
		t.Fatalf("valid internal-view LinkInChild must pass, got: %v", err)
	}
}

func TestValidateComposedViews_LinkInChild_Rejections(t *testing.T) {
	notAChild := core.NewTableSchema[cvLine]("random_lines").ID("id").ParentID("gadget_id").Field("ItemID", "item_id")
	cases := []struct {
		name     string
		composed *ComposedViewDefinition
		fragment string
	}{
		{
			name: "not a native child of the primary",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryWithChild()).
				LinkInChild(notAChild, JoinUpstream(cvUpstreamSchema(), "Item", "item")).On("item_id"),
			fragment: "NOT a native child of the primary",
		},
		{
			name: "empty join column",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryWithChild()).
				LinkInChild(cvLineSchema(), JoinUpstream(cvUpstreamSchema(), "Item", "item")).On(""),
			fragment: "empty join column",
		},
		{
			name: "join column not on the child schema",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryWithChild()).
				LinkInChild(cvLineSchema(), JoinUpstream(cvUpstreamSchema(), "Item", "item")).On("bogus"),
			fragment: "does not exist on the child schema",
		},
		{
			name: "leg Go segment collides with a child field",
			composed: ComposedView("gadgets_full").Primary(cvPrimaryWithChild()).
				LinkInChild(cvLineSchema(), JoinUpstream(cvUpstreamSchema(), "ItemID", "item")).On("item_id"),
			fragment: "already carries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantProblem(t, validateOne(tc.composed, []*ViewDefinition{cvPrimaryWithChild()}, cvUpstreams()), tc.fragment)
		})
	}
}

func TestComposedView_LinkInChild_LinksProjection(t *testing.T) {
	links := cvValidInChild().Links()
	if len(links) != 1 {
		t.Fatalf("expected 1 link, got %d", len(links))
	}
	l := links[0]
	if !l.InChild() {
		t.Fatal("link must report InChild() == true")
	}
	if l.ChildSegment != childDocSegment(cvLineSchema()) {
		t.Errorf("ChildSegment = %q, want %q", l.ChildSegment, childDocSegment(cvLineSchema()))
	}
	if l.FKGoField != "ItemID" {
		t.Errorf("FKGoField = %q, want ItemID (Go name of item_id)", l.FKGoField)
	}
	if l.GoSegment != "Item" || l.DocField != "item" {
		t.Errorf("segment naming = %q/%q, want Item/item", l.GoSegment, l.DocField)
	}
	if l.Many {
		t.Error("LinkInChild must be 1:1 (Many == false)")
	}
	if !l.External {
		t.Error("expected an external (JoinUpstream) leg")
	}
}

// ExternalLegs is the boot guards' pre-validation accessor: it must select
// ONLY the JoinUpstream legs (external schemas) and be safe before
// ValidateComposedViews.
func TestComposedView_ExternalLegs(t *testing.T) {
	prodSrc := View("cv_products").Version(1).Schema(composerRootSchema())
	cv := ComposedView("cv_full").
		Primary(View("cv_primary").Version(1).Schema(composerRootSchema())).
		Link(extLeg("upstream_mirror", "Mirror", "mirror")).On("mirror_id").
		Link(JoinView(prodSrc, "Product", "product")).On("product_id")

	legs := cv.ExternalLegs()
	if len(legs) != 1 {
		t.Fatalf("only the JoinUpstream leg is external, got %d", len(legs))
	}
	if legs[0].Collection() != "upstream_mirror" {
		t.Errorf("wrong leg selected: %q", legs[0].Collection())
	}
}
