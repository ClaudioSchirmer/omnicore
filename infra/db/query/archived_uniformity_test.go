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
// ArchivedAt(col) is filtered; one that declares none has no archived concept
// and is never touched. That condition is the whole rule, so it is pinned here
// for every shape rather than assumed.

type arcRoot struct{ ID string }
type arcItem struct {
	ID    string
	Label string
}

func arcRootSchema(table string) *core.TableSchema {
	return core.NewTableSchema[arcRoot](table).ID("id").ArchivedAt("archived_at")
}

// mirrorWithArchived / mirrorNoArchived are the two source shapes the rule distinguishes.
func mirrorWithArchived() *Leg {
	return JoinUpstream(core.NewExternalSchema("upstream_items").ID("id").
		Field("Label", "label").ArchivedAt("archived_at"), "Item", "item")
}

func mirrorNoArchived() *Leg {
	return JoinUpstream(core.NewExternalSchema("upstream_plain").ID("id").
		Field("Label", "label"), "Plain", "plain")
}

const archivedStamp = "2026-01-02T00:00:00Z"

func TestArchived_OneToOneSegmentHiddenWhenSourceDeclaresArchivedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorWithArchived()).On("item_id").Indexes(Index("item_id"))
	doc := map[string]any{"_id": "o1", "item": map[string]any{"_id": "i1", "label": "x", "archived_at": archivedStamp}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] != nil {
		t.Fatalf("an archived 1:1 segment must become the explicit null, got %v", doc["item"])
	}
}

func TestArchived_OneToOneSegmentKeptWhenActive(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorWithArchived()).On("item_id").Indexes(Index("item_id"))
	doc := map[string]any{"_id": "o1", "item": map[string]any{"_id": "i1", "label": "x", "archived_at": nil}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] == nil {
		t.Fatal("an ACTIVE segment must survive the default read")
	}
}

// The condition that governs everything: no ArchivedAt on the source schema, no
// filtering — the framework cannot invent a lifecycle the source never declared.
func TestArchived_SegmentUntouchedWhenSourceDeclaresNoArchivedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		Embed(mirrorNoArchived()).On("plain_id").Indexes(Index("plain_id"))
	// Even carrying a archived_at-looking field, an undeclared lifecycle is not a
	// lifecycle: the segment must survive untouched.
	doc := map[string]any{"_id": "o1", "plain": map[string]any{"_id": "p1", "archived_at": archivedStamp}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["plain"] == nil {
		t.Fatal("a source declaring no ArchivedAt must never be filtered")
	}
}

func TestArchived_OneToManyDropsArchivedElementsOnly(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		EmbedMany(mirrorWithArchived()).On("order_id")
	doc := map[string]any{"_id": "o1", "item": []any{
		map[string]any{"_id": "i1", "label": "live", "archived_at": nil},
		map[string]any{"_id": "i2", "label": "gone", "archived_at": archivedStamp},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	items, _ := doc["item"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["_id"] != "i1" {
		t.Fatalf("an archived element must LEAVE the array, the rest must stay, got %v", items)
	}
}

func TestArchived_OneToManyUntouchedWithoutArchivedAt(t *testing.T) {
	v := View("orders").Version(1).Schema(arcRootSchema("orders")).
		EmbedMany(mirrorNoArchived()).On("order_id")
	doc := map[string]any{"_id": "o1", "plain": []any{
		map[string]any{"_id": "p1", "archived_at": archivedStamp},
		map[string]any{"_id": "p2"},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	if items, _ := doc["plain"].([]any); len(items) != 2 {
		t.Fatalf("no declared ArchivedAt ⇒ no filtering, got %v", items)
	}
}

// An EmbedInChild enrichment is a segment one level down, and follows the same
// rule inside every child element.
func TestArchived_EnrichmentInsideAChildElement(t *testing.T) {
	child := core.NewTableSchema[arcItem]("order_lines").ID("id").ParentID("orders_id").
		Field("Label", "label").ArchivedAt("archived_at")
	root := core.NewTableSchema[arcRoot]("orders").ID("id").ArchivedAt("archived_at").Child(child)
	v := View("orders").Version(1).Schema(root).
		EmbedInChild(child, mirrorWithArchived()).On("item_id").
		Indexes(Index(childDocSegment(child) + ".item_id"))
	seg := childDocSegment(child)
	doc := map[string]any{"_id": "o1", seg: []any{
		map[string]any{"_id": "l1", "archived_at": nil, "item": map[string]any{"_id": "i1", "archived_at": archivedStamp}},
		map[string]any{"_id": "l2", "archived_at": nil, "item": map[string]any{"_id": "i2", "archived_at": nil}},
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
// segment that declares a lifecycle must contribute its ArchivedAt path for
// the reader's auto-include — segments included, not just child collections.
func TestArchived_ArchivedAtPathsCoverEverySegment(t *testing.T) {
	child := core.NewTableSchema[arcItem]("order_lines").ID("id").ParentID("orders_id").
		ArchivedAt("archived_at")
	root := core.NewTableSchema[arcRoot]("orders").ID("id").ArchivedAt("archived_at").Child(child)
	v := View("orders").Version(1).Schema(root).
		Embed(mirrorWithArchived()).On("item_id").
		Embed(mirrorNoArchived()).On("plain_id").
		Indexes(Index("item_id"), Index("plain_id"))
	paths := v.BuildViewNode().ChildArchivedAtPaths()
	if paths["item"] != "archived_at" {
		t.Errorf("a segment declaring ArchivedAt must contribute its path, got %v", paths)
	}
	if _, present := paths["plain"]; present {
		t.Errorf("a segment declaring none must contribute nothing, got %v", paths)
	}
	if paths[childDocSegment(child)] != "archived_at" {
		t.Errorf("child collections still contribute, got %v", paths)
	}
}

func (arcItem) CollectionName() string { return "ArcItems" }
