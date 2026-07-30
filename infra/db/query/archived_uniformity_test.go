package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// ONE archived rule, every segment. A default read hides an archived entry in
// EVERY segment of a document — a native child collection, a role, a
// materialized embed (1:1 or 1:N, over a local view or an upstream mirror) and
// an EmbedInChild enrichment — and `?includeArchived=true` (which skips this
// pass entirely at the reader) brings them all back.
//
// The gate is the SOURCE SCHEMA: a segment whose schema declares
// DeletedAt(col) is filtered; one that declares none has no archived concept
// and is never touched. That condition is the whole rule, so it is pinned here
// for every shape rather than assumed.

type arcRoot struct{ ID string }
type arcItem struct {
	ID    string
	Label string
}

func arcRootSchema(table string) *core.TableSchema {
	return core.NewTableSchema[arcRoot](table).ID("id").DeletedAt("deleted_at")
}

// mirrorWithSD / mirrorNoSD are the two source shapes the rule distinguishes.
func mirrorWithSD() *Leg {
	return JoinUpstream(core.NewExternalSchema("upstream_items").ID("id").
		Field("Label", "label").DeletedAt("deleted_at"), "Item", "item")
}

func mirrorNoSD() *Leg {
	return JoinUpstream(core.NewExternalSchema("upstream_plain").ID("id").
		Field("Label", "label"), "Plain", "plain")
}

const archivedStamp = "2026-01-02T00:00:00Z"

func TestArchived_OneToOneSegmentHiddenWhenSourceDeclaresDeletedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorWithSD()).On("item_id").Indexes(Index("item_id"))
	doc := map[string]any{"_id": "o1", "item": map[string]any{"_id": "i1", "label": "x", "deleted_at": archivedStamp}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] != nil {
		t.Fatalf("an archived 1:1 segment must become the explicit null, got %v", doc["item"])
	}
}

func TestArchived_OneToOneSegmentKeptWhenActive(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorWithSD()).On("item_id").Indexes(Index("item_id"))
	doc := map[string]any{"_id": "o1", "item": map[string]any{"_id": "i1", "label": "x", "deleted_at": nil}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] == nil {
		t.Fatal("an ACTIVE segment must survive the default read")
	}
}

// The condition that governs everything: no DeletedAt on the source schema, no
// filtering — the framework cannot invent a lifecycle the source never declared.
func TestArchived_SegmentUntouchedWhenSourceDeclaresNoDeletedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorNoSD()).On("plain_id").Indexes(Index("plain_id"))
	// Even carrying a deleted_at-looking field, an undeclared lifecycle is not a
	// lifecycle: the segment must survive untouched.
	doc := map[string]any{"_id": "o1", "plain": map[string]any{"_id": "p1", "deleted_at": archivedStamp}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["plain"] == nil {
		t.Fatal("a source declaring no DeletedAt must never be filtered")
	}
}

func TestArchived_OneToManyDropsArchivedElementsOnly(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		EmbedMany(mirrorWithSD()).On("order_id")
	doc := map[string]any{"_id": "o1", "item": []any{
		map[string]any{"_id": "i1", "label": "live", "deleted_at": nil},
		map[string]any{"_id": "i2", "label": "gone", "deleted_at": archivedStamp},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	items, _ := doc["item"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["_id"] != "i1" {
		t.Fatalf("an archived element must LEAVE the array, the rest must stay, got %v", items)
	}
}

func TestArchived_OneToManyUntouchedWithoutDeletedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		EmbedMany(mirrorNoSD()).On("order_id")
	doc := map[string]any{"_id": "o1", "plain": []any{
		map[string]any{"_id": "p1", "deleted_at": archivedStamp},
		map[string]any{"_id": "p2"},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if items, _ := doc["plain"].([]any); len(items) != 2 {
		t.Fatalf("no declared DeletedAt ⇒ no filtering, got %v", items)
	}
}

// An EmbedInChild enrichment is a segment one level down, and follows the same
// rule inside every child element.
func TestArchived_EnrichmentInsideAChildElement(t *testing.T) {
	child := core.NewTableSchema[arcItem]("order_lines").ID("id").ParentID("orders_id").
		Field("Label", "label").DeletedAt("deleted_at")
	root := core.NewTableSchema[arcRoot]("orders").ID("id").DeletedAt("deleted_at").Child(child)
	v := View("orders").Version(1).Schema(root).
		EmbedInChild(child, mirrorWithSD()).On("item_id").
		Indexes(Index(childDocSegment(child) + ".item_id"))
	seg := childDocSegment(child)
	doc := map[string]any{"_id": "o1", seg: []any{
		map[string]any{"_id": "l1", "deleted_at": nil, "item": map[string]any{"_id": "i1", "deleted_at": archivedStamp}},
		map[string]any{"_id": "l2", "deleted_at": nil, "item": map[string]any{"_id": "i2", "deleted_at": nil}},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	lines, _ := doc[seg].([]any)
	if len(lines) != 2 {
		t.Fatalf("active child lines must survive, got %v", lines)
	}
	if lines[0].(map[string]any)["item"] != nil {
		t.Error("an archived enrichment must be nulled inside its element")
	}
	if lines[1].(map[string]any)["item"] == nil {
		t.Error("an active enrichment must survive")
	}
}

// The strip can only hide what the projected entries still carry, so every
// segment that declares a lifecycle must contribute its DeletedAt path for
// the reader's auto-include — segments included, not just child collections.
func TestArchived_DeletedAtPathsCoverEverySegment(t *testing.T) {
	child := core.NewTableSchema[arcItem]("order_lines").ID("id").ParentID("orders_id").
		DeletedAt("deleted_at")
	root := core.NewTableSchema[arcRoot]("orders").ID("id").DeletedAt("deleted_at").Child(child)
	v := View("orders").Version(1).Schema(root).
		Embed(mirrorWithSD()).On("item_id").
		Embed(mirrorNoSD()).On("plain_id").
		Indexes(Index("item_id"), Index("plain_id"))
	paths := v.BuildViewNode().ChildDeletedAtPaths()
	if paths["item"] != "deleted_at" {
		t.Errorf("a segment declaring DeletedAt must contribute its path, got %v", paths)
	}
	if _, present := paths["plain"]; present {
		t.Errorf("a segment declaring none must contribute nothing, got %v", paths)
	}
	if paths[childDocSegment(child)] != "deleted_at" {
		t.Errorf("child collections still contribute, got %v", paths)
	}
}
