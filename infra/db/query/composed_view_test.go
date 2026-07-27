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
		PK("id").
		Field("Code", "code").
		Field("MirrorID", "mirror_id").
		SoftDelete("deleted_at")
}

func cvPrimaryView() *ViewDefinition {
	return View("gadgets").Version(1).Schema(cvPrimarySchema())
}

func cvNotesSchema() *core.TableSchema {
	return core.NewTableSchema[Note]("gadget_notes").
		PK("id").
		Field("GadgetID", "gadget_id").
		Field("Text", "text").
		SoftDelete("deleted_at")
}

func cvNotesView() *ViewDefinition {
	return View("gadget_notes").Version(1).Schema(cvNotesSchema()).
		Indexes(Index("gadget_id")) // the LinkMany FK must be index-covered (boot-enforced)
}

func cvUpstreamSchema() *core.TableSchema {
	return core.NewExternalSchema("upstream_gadgets").PK("id").Field("Code", "code")
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
			name: "external leg without PK",
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
			name: "LinkMany FK without a covering index",
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
	// FK "id" is the primary's PK → the parent join value is the item's _id.
	if mirror.ParentKeyGoField != "_id" {
		t.Fatalf("PK-joined 1:1 link must read the parent key from _id, got %q", mirror.ParentKeyGoField)
	}
	if mirror.Node() == nil {
		t.Fatal("mirror link carries no translator node")
	}

	notes := links[1]
	if notes.GoSegment != "Notes" || !notes.Many || notes.External {
		t.Fatalf("unexpected notes link: %+v", notes)
	}
	if notes.Collection != "gadget_notes" || notes.FKColumn != "gadget_id" {
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
		t.Fatalf("expected the Go name of the primary FK column, got %q", links[0].ParentKeyGoField)
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
