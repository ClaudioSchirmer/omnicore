package query

import (
	"github.com/ClaudioSchirmer/omnicore/domain"

	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// ─── test embed-source helpers ───────────────────────────────────────────────
//
// extLeg builds an external JoinUpstream leg (a type-less NewExternalSchema with a
// ID) for embed/link tests, carrying the mandatory Go and doc segment names. The
// join column is named at the call site via .On(...). extLegNoPK omits the ID for
// the "embed source without ID" boot-guard test.
type embedFixture struct{ ID string }

func extLeg(table, goName, ext string) *Leg {
	return JoinUpstream(core.NewExternalSchema(table).ID("id"), goName, ext)
}

func extLegNoPK(table, goName, ext string) *Leg {
	return JoinUpstream(core.NewExternalSchema(table), goName, ext)
}

// rootSchema is a minimal type-anchored schema for a composing test view's root
// (ID + DeletedAt). The composer only reads ID + DeletedAt from the root
// schema; row columns are read generically, so the dummy type suffices.
func rootSchema(table string) *core.TableSchema {
	return core.NewTableSchema[embedFixture](table).ID("id").DeletedAt("deleted_at")
}

// ─── own children on a schema project automatically (read side) ──────────────

// TestJoinUpstream_RejectsAnchoredSchema proves the canonical split: an embed leg
// composes only EXTERNAL data, so JoinUpstream over a write-anchored (type-anchored)
// schema panics at declaration — a local aggregate's own data projects automatically
// from the TableSchema, never through an embed. (An internal view is joined at read
// time with query.ComposedView / JoinView, not embedded.)
func TestJoinUpstream_RejectsAnchoredSchema(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected JoinUpstream over a write-anchored schema to panic")
		}
		if !strings.Contains(fmt.Sprint(r), "write-anchored") {
			t.Errorf("panic should name the external-only rule, got: %v", r)
		}
	}()
	JoinUpstream(core.NewTableSchema[embedFixture]("addresses").ID("id"), "Addresses", "addresses")
}

// TestValidateViewSchemas_RootSchemaWithChildrenOK confirms the view ROOT schema
// may carry Child(...) — its own children auto-project, no embed needed.
func TestValidateViewSchemas_RootSchemaWithChildrenOK(t *testing.T) {
	rootWithChild := core.NewTableSchema[embedFixture]("users").ID("id").DeletedAt("deleted_at").
		Child(core.NewTableSchema[schemaSample]("addresses").ID("id").ParentID("user_id"))
	v := View("users").Version(1).
		Schema(rootWithChild)

	if err := ValidateViewSchemas([]*ViewDefinition{v}); err != nil {
		t.Fatalf("root schema with one-level children must pass view validation, got: %v", err)
	}
}

// TestValidateViewSchemas_RejectsSegmentCollision proves the collision guard: a
// legal EXTERNAL embed whose .As segment collides with an auto-projected own-child
// segment is a boot error — one segment, one source.
func TestValidateViewSchemas_RejectsSegmentCollision(t *testing.T) {
	child := core.NewTableSchema[schemaSample]("addresses").ID("id").ParentID("user_id")
	seg := childDocSegment(child) // the auto own-child doc segment
	rootWithChild := core.NewTableSchema[embedFixture]("users").ID("id").DeletedAt("deleted_at").
		Child(child)
	// A legal external embed whose Go segment collides with the own-child segment.
	v := View("users").Version(1).
		Schema(rootWithChild).
		EmbedMany(extLeg("ext_coll", seg, "ext")).On("user_id")

	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil {
		t.Fatal("expected a collision error for an external embed segment clashing with an auto own-child")
	}
	if !strings.Contains(err.Error(), "exactly one source") {
		t.Errorf("error should name the one-source-per-segment rule, got: %v", err)
	}
}

// ─── mandatory ID on every view schema (read side) ───────────────────────────

// TestValidateViewSchemas_RejectsRootWithoutPK proves a view root schema with no
// explicit ID is a fatal view-validation error.
func TestValidateViewSchemas_RejectsRootWithoutPK(t *testing.T) {
	v := View("users").Version(1).
		Schema(core.NewTableSchema[embedFixture]("users").DeletedAt("deleted_at")). // no .ID
		EmbedMany(extLeg("addresses", "Addresses", "addresses")).On("user_id")
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected root-without-ID error, got %v", err)
	}
}

// TestValidateViewSchemas_RejectsEmbedSourceWithoutPK proves an embed source
// with no explicit ID is a fatal view-validation error.
func TestValidateViewSchemas_RejectsEmbedSourceWithoutPK(t *testing.T) {
	v := View("users").Version(1).
		Schema(rootSchema("users")).
		EmbedMany(extLegNoPK("addresses", "Addresses", "addresses")).On("user_id")
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("expected embed-source-without-ID error, got %v", err)
	}
}

// ─── mandatory join column on embeds (read side) ─────────────────────────────

// TestValidateViewSchemas_RejectsEmbedManyWithEmptyOn proves an EmbedMany whose
// .On names an empty join column is a fatal view-validation error — the composer
// joins the child rows on it.
func TestValidateViewSchemas_RejectsEmbedManyWithEmptyOn(t *testing.T) {
	v := View("users").Version(1).
		Schema(rootSchema("users")).
		EmbedMany(extLeg("addresses", "Addresses", "addresses")).On("")
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "empty join column") {
		t.Errorf("expected EmbedMany-with-empty-.On error, got %v", err)
	}
}

// TestValidateViewSchemas_RejectsOneToOneEmbedWithEmptyOn proves a one-to-one
// Embed whose .On names an empty join column is a fatal view-validation error —
// it joins on the parent's ParentID column, which must be named.
func TestValidateViewSchemas_RejectsOneToOneEmbedWithEmptyOn(t *testing.T) {
	v := View("orders").Version(1).
		Schema(rootSchema("orders")).
		Embed(extLeg("buyer", "Buyer", "buyer")).On("")
	err := ValidateViewSchemas([]*ViewDefinition{v})
	if err == nil || !strings.Contains(err.Error(), "empty join column") {
		t.Errorf("expected one-to-one-Embed-with-empty-.On error, got %v", err)
	}
}

// ─── DeleteOnArchive opt-in ──────────────────────────────────────────────────

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Flat(t *testing.T) {
	v := View("things")
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on a flat view")
	}
}

func TestViewDefinition_DeleteOnArchiveDefaultFalse_Aggregate(t *testing.T) {
	v := View("users").
		EmbedMany(extLeg("addresses", "Addresses", "addresses")).On("user_id")
	if v.DeletesOnArchive() {
		t.Fatal("DeletesOnArchive() must default to false on an aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

func TestViewDefinition_DeleteOnArchiveBuilder_Flat(t *testing.T) {
	v := View("things").DeleteOnArchive().Schema(rootSchema("things"))
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after .DeleteOnArchive() builder")
	}
	if v.RootTable() != "things" {
		t.Errorf("chaining broken: RootTable = %q, want %q", v.RootTable(), "things")
	}
}

func TestViewDefinition_DeleteOnArchiveBuilder_Aggregate(t *testing.T) {
	v := View("users").DeleteOnArchive().
		EmbedMany(extLeg("addresses", "Addresses", "addresses")).On("user_id")
	if !v.DeletesOnArchive() {
		t.Fatal("expected DeletesOnArchive() = true after builder on aggregate view")
	}
	if len(v.Embeds()) != 1 {
		t.Fatalf("aggregate view should carry 1 embed, got %d", len(v.Embeds()))
	}
}

// TestLeg_Accessors covers the surviving leg accessors the embed boot guards use:
// SchemaDef returns the external schema; IsMongo/Collection/Table describe it.
func TestLeg_Accessors(t *testing.T) {
	ext := core.NewExternalSchema("users").ID("id")
	leg := JoinUpstream(ext, "Buyer", "buyer")
	if leg.SchemaDef() != ext {
		t.Error("SchemaDef() must return the schema JoinUpstream was built with")
	}
	if !leg.IsMongo() {
		t.Error("JoinUpstream(core.NewExternalSchema(...)) must be a Mongo leg")
	}
	if leg.Collection() != "users" || leg.Table() != "users" {
		t.Errorf("Collection()/Table() = %q/%q, want users (from schema)", leg.Collection(), leg.Table())
	}
}

// ─── Schema-driven view translation tree ─────────────────────────────────────

type vsRoot struct {
	Email string
}

type vsChild struct {
	ID      string
	ZipCode string
}

func (v vsChild) GetID() domain.ID { return domain.NewID(v.ID) }

// TestViewNode_OwnChildPathResolves proves the translator registers a root's own
// aggregate children (no embed declared), so a filter/sort on an own-child field
// resolves via ColumnPath and the read-back nests the collection under its Go
// segment — the translator half of Phase-1 own-child projection.
func TestViewNode_OwnChildPathResolves(t *testing.T) {
	childSchema := core.NewTableSchema[csComposeVO]("lines").ID("id").ParentID("order_id").Field("Label", "label")
	rootWithChild := core.NewTableSchema[*builderTestEntity]("orders").
		ID("id").Field("Name", "name").DeletedAt("deleted_at").
		Child(childSchema)
	node := View("orders").Schema(rootWithChild).BuildViewNode()

	seg := childDocSegment(childSchema)
	if col, ok := node.ColumnPath([]string{seg, "Label"}); !ok || len(col) != 2 || col[0] != seg || col[1] != "label" {
		t.Errorf("%s.Label → %v,%v want [%s label]", seg, col, ok, seg)
	}
	doc := map[string]any{
		"id":   "o1",
		"name": "first",
		seg:    []any{map[string]any{"id": "l1", "order_id": "o1", "label": "L1"}},
	}
	got := node.ToGoDoc(doc)
	rows, ok := got[seg].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("read-back %s = %v", seg, got[seg])
	}
	if row, _ := rows[0].(map[string]any); row["Label"] != "L1" {
		t.Errorf("own-child read-back Label = %v want L1 (row=%v)", row["Label"], rows[0])
	}
}

func TestViewNode_TranslatesGoPathToColumnAndBack(t *testing.T) {
	rootSchema := core.NewTableSchema[vsRoot]("people").
		ID("person_pk").
		Field("Email", "mail").
		DeletedAt("removed_at").
		CreatedAt("created_at")
	childSchema := core.NewExternalSchema("tags").
		ID("tag_pk").
		Field("ZipCode", "zip")

	v := View("people").Schema(rootSchema).
		EmbedMany(JoinUpstream(childSchema, "Addresses", "addresses")).On("person_ref")

	node := v.BuildViewNode()

	// Go path → column path
	if col, ok := node.ColumnPath([]string{"Email"}); !ok || col[0] != "mail" {
		t.Errorf("Email → %v,%v want [mail]", col, ok)
	}
	if col, ok := node.ColumnPath([]string{"Addresses", "ZipCode"}); !ok || col[0] != "addresses" || col[1] != "zip" {
		t.Errorf("Addresses.ZipCode → %v,%v want [addresses zip]", col, ok)
	}
	if sd, ok := node.DeletedAtColumn(); !ok || sd != "removed_at" {
		t.Errorf("DeletedAt = %q,%v want removed_at", sd, ok)
	}
	// Managed columns translate forward (Go logical name → column) symmetrically
	// with the read-back, so a typed Response can sort/project on them.
	if col, ok := node.ColumnPath([]string{"CreatedAt"}); !ok || col[0] != "created_at" {
		t.Errorf("CreatedAt → %v,%v want [created_at]", col, ok)
	}
	if col, ok := node.ColumnPath([]string{"DeletedAt"}); !ok || col[0] != "removed_at" {
		t.Errorf("DeletedAt → %v,%v want [removed_at]", col, ok)
	}

	// column doc → Go doc (read-back), recursive into the embed
	doc := map[string]any{
		"person_pk":  "p1",
		"mail":       "a@x.test",
		"created_at": "2026-06-19T00:00:00Z",
		"addresses":  []any{map[string]any{"tag_pk": "t1", "zip": "10001", "person_ref": "p1"}},
	}
	got := node.ToGoDoc(doc)
	if got["Email"] != "a@x.test" {
		t.Errorf("read-back Email = %v", got["Email"])
	}
	if got["CreatedAt"] != "2026-06-19T00:00:00Z" {
		t.Errorf("read-back CreatedAt = %v", got["CreatedAt"])
	}
	if got["ID"] != "p1" {
		t.Errorf("read-back ID = %v want p1", got["ID"])
	}
	addrs, ok := got["Addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("read-back Addresses = %v", got["Addresses"])
	}
	child := addrs[0].(map[string]any)
	if child["ZipCode"] != "10001" || child["ID"] != "t1" {
		t.Errorf("read-back child = %v", child)
	}
}

// TestViewNode_IDAndParentID_AllLevels covers the id AND parentId translation at
// every distinct node kind — root, own child, and an external upstream mirror
// embed — in BOTH directions: ColumnPath (which drives a filter / sort / ?fields=
// at that level) and ToGoDoc (the wire output). A regular schema's id/parentId
// resolve to their PHYSICAL columns (so the projection includes/excludes them
// like any field); a mirror's id resolves to `_id` (its only carrier).
func TestViewNode_IDAndParentID_AllLevels(t *testing.T) {
	line := core.NewTableSchema[csComposeVO]("lines").
		ID("id").ParentID("order_id").Field("Label", "label")
	item := core.NewExternalSchema("items"). // upstream mirror: id lives only in `_id`
							ID("item_id").Field("Label", "label")
	root := core.NewTableSchema[vsRoot]("orders").
		ID("order_pk").Field("Email", "mail").Child(line)
	node := View("orders").Schema(root).
		EmbedMany(JoinUpstream(item, "Items", "items")).On("order_ref").
		BuildViewNode()
	seg := childDocSegment(line)

	// ColumnPath — the single translator every filter / sort / ?fields= goes
	// through, so if it resolves right, all three operations respect the field.
	for _, c := range []struct {
		name string
		in   []string
		want []string
	}{
		{"root id → physical pk", []string{"ID"}, []string{"order_pk"}},
		{"own-child id → physical pk", []string{seg, "ID"}, []string{seg, "id"}},
		{"own-child parentId → physical fk", []string{seg, "ParentID"}, []string{seg, "order_id"}},
		{"mirror id → _id", []string{"Items", "ID"}, []string{"items", "_id"}},
	} {
		got, ok := node.ColumnPath(c.in)
		bad := !ok || len(got) != len(c.want)
		for i := 0; !bad && i < len(got); i++ {
			bad = got[i] != c.want[i]
		}
		if bad {
			t.Errorf("%s: ColumnPath(%v) = %v,%v want %v", c.name, c.in, got, ok, c.want)
		}
	}

	// ToGoDoc — regular schemas expose id/parentId from the physical columns; the
	// mirror element (which carries only `_id`) still surfaces its id.
	got := node.ToGoDoc(map[string]any{
		"order_pk": "o1",
		"mail":     "e@x.test",
		seg:        []any{map[string]any{"id": "l1", "order_id": "o1", "label": "L1"}},
		"items":    []any{map[string]any{"_id": "it1", "label": "I1"}},
	})
	if got["ID"] != "o1" {
		t.Errorf("root ID = %v want o1", got["ID"])
	}
	child := got[seg].([]any)[0].(map[string]any)
	if child["ID"] != "l1" || child["ParentID"] != "o1" {
		t.Errorf("child ID/ParentID = %v/%v want l1/o1", child["ID"], child["ParentID"])
	}
	mItem := got["Items"].([]any)[0].(map[string]any)
	if mItem["ID"] != "it1" {
		t.Errorf("mirror item ID = %v want it1 (promoted from _id)", mItem["ID"])
	}
}

// The id is a field like any other — it is just carried under a different name
// depending on the source: a REGULAR schema (root, child, materialized view
// embed) keeps it in the physical PK column (so it obeys the projection like any
// column); an EXTERNAL upstream MIRROR has no physical id column and carries the
// identity only in the Mongo `_id`, so there it is promoted onto the ID Go field.
// A regular schema is NOT promoted from the incidental `_id` — that would leak the
// id past a ?fields= that excluded it.
func TestViewNode_IDProjection(t *testing.T) {
	rootSchema := core.NewTableSchema[vsRoot]("people").
		ID("person_pk").
		Field("Email", "mail")
	itemSchema := core.NewExternalSchema("items"). // upstream mirror: id lives in `_id`
							ID("id").
							Field("Label", "label")

	v := View("people").Schema(rootSchema).
		Embed(JoinUpstream(itemSchema, "Featured", "featured")).On("featured_ref")
	node := v.BuildViewNode()

	// Regular ROOT: id resolves to the physical PK column (subject to the projection).
	if col, ok := node.ColumnPath([]string{"ID"}); !ok || len(col) != 1 || col[0] != "person_pk" {
		t.Errorf("root ID path → %v,%v want [person_pk]", col, ok)
	}
	// Mirror SEGMENT: id lives in `_id`, so it resolves there.
	if col, ok := node.ColumnPath([]string{"Featured", "ID"}); !ok || len(col) != 2 || col[0] != "featured" || col[1] != "_id" {
		t.Errorf("Featured.ID path → %v,%v want [featured _id]", col, ok)
	}

	// Regular root WITH the physical id column exposes ID; the same root carrying
	// ONLY the incidental `_id` does NOT — a regular id is a physical-column field.
	if got := node.ToGoDoc(map[string]any{"person_pk": "p1", "mail": "a@x.test"}); got["ID"] != "p1" {
		t.Errorf("regular root ID = %v want p1 (physical column)", got["ID"])
	}
	if got := node.ToGoDoc(map[string]any{"_id": "p1", "mail": "a@x.test"}); func() bool { _, ok := got["ID"]; return ok }() {
		t.Errorf("regular root with only _id must NOT promote ID")
	}

	// Mirror segment: the id is ONLY in `_id`, so it IS promoted to ID (the mirror
	// has no physical id column — the whole reason the promotion exists).
	seg := node.ToGoDoc(map[string]any{
		"person_pk": "p1",
		"featured":  map[string]any{"_id": "it-9", "label": "Widget"},
	})["Featured"].(map[string]any)
	if seg["ID"] != "it-9" {
		t.Errorf("mirror segment ID = %v want it-9 (promoted from _id)", seg["ID"])
	}
	if seg["Label"] != "Widget" {
		t.Errorf("mirror segment Label = %v", seg["Label"])
	}
}
