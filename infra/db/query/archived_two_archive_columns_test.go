package query

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// TWO archives in the same neighbourhood, both named "archived_at": the
// child element's own lifecycle and the enrichment's. Each must be read from
// ITS OWN map using ITS OWN schema's column — a shared column NAME must never
// let one decide the other's fate.
func TestTwoArchivedAts_AreIndependent(t *testing.T) {
	child := core.NewTableSchema[arcItem]("lines").ID("id").ParentID("orders_id").ArchivedAt("archived_at")
	root := core.NewTableSchema[arcRoot]("orders").ID("id").ArchivedAt("archived_at").Child(child)
	v := View("orders").Version(1).Schema(root).
		EmbedInChild(child, mirrorWithArchived()).On("item_id").
		Indexes(Index(childDocSegment(child) + ".item_id"))
	seg := childDocSegment(child)

	doc := map[string]any{"_id": "o1", seg: []any{
		// element ACTIVE, enrichment ARCHIVED → element stays, enrichment nulled
		map[string]any{"_id": "l1", "archived_at": nil, "item": map[string]any{"_id": "i1", "archived_at": "2026-01-01"}},
		// element ARCHIVED, enrichment ACTIVE → element leaves regardless
		map[string]any{"_id": "l2", "archived_at": "2026-01-01", "item": map[string]any{"_id": "i2", "archived_at": nil}},
		// both active → both survive
		map[string]any{"_id": "l3", "archived_at": nil, "item": map[string]any{"_id": "i3", "archived_at": nil}},
	}}
	v.BuildViewNode().StripArchivedChildren(doc)
	lines, _ := doc[seg].([]any)
	if len(lines) != 2 {
		t.Fatalf("only the ARCHIVED ELEMENT must leave, got %d lines: %v", len(lines), lines)
	}
	if lines[0].(map[string]any)["_id"] != "l1" || lines[0].(map[string]any)["item"] != nil {
		t.Errorf("l1 must survive with its enrichment nulled, got %v", lines[0])
	}
	if lines[1].(map[string]any)["_id"] != "l3" || lines[1].(map[string]any)["item"] == nil {
		t.Errorf("l3 must survive untouched, got %v", lines[1])
	}
}

// TWO sibling segments at the SAME level, each with its own ArchivedAt: one
// archived, one active. Each decides only itself.
func TestTwoSiblingSegments_DecideIndependently(t *testing.T) {
	v := View("parts").Version(1).Schema(arcRootSchema("parts")).
		Embed(mirrorWithArchived()).On("item_id").
		Embed(JoinUpstream(core.NewExternalSchema("upstream_brands").ID("id").
			Field("Name", "name").ArchivedAt("archived_at"), "Brand", "brand")).On("brand_id").
		Indexes(Index("item_id"), Index("brand_id"))
	doc := map[string]any{
		"_id":   "p1",
		"item":  map[string]any{"_id": "i1", "archived_at": "2026-01-01"}, // archived
		"brand": map[string]any{"_id": "b1", "archived_at": nil},          // active
	}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] != nil {
		t.Errorf("the archived sibling segment must be nulled, got %v", doc["item"])
	}
	if doc["brand"] == nil {
		t.Error("the ACTIVE sibling segment must survive — one segment must never decide another's fate")
	}
}

// A segment WITHOUT a declared ArchivedAt sitting beside one WITH it: the
// undeclared one is never filtered, even carrying an archived_at-looking field.
func TestSiblingSegments_UndeclaredIsNeverFiltered(t *testing.T) {
	v := View("parts").Version(1).Schema(arcRootSchema("parts")).
		Embed(mirrorWithArchived()).On("item_id").
		Embed(mirrorNoArchived()).On("plain_id").
		Indexes(Index("item_id"), Index("plain_id"))
	doc := map[string]any{
		"_id":   "p1",
		"item":  map[string]any{"_id": "i1", "archived_at": "2026-01-01"},
		"plain": map[string]any{"_id": "x1", "archived_at": "2026-01-01"},
	}
	v.BuildViewNode().StripArchivedChildren(doc)
	if doc["item"] != nil {
		t.Errorf("declared ⇒ filtered, got %v", doc["item"])
	}
	if doc["plain"] == nil {
		t.Error("undeclared ⇒ never filtered, even with an archived_at-looking field")
	}
}
