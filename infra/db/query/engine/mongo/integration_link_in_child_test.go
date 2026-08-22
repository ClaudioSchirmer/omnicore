//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// LinkInChild integration: the read-time twin of EmbedInChild on a ComposedView.
// Seeds a primary view collection whose docs carry a native child array, plus an
// external leg collection, and reads through the real MongoViewReader (which sets
// DefaultDocumentM, so nested children decode as bson.M and translate to Go form).

type licGadget struct{ ID, Code string }
type licLine struct{ ID, GadgetID, ItemID, Note string }

func licChildSchema() *core.TableSchema {
	return core.NewTableSchema[licLine]("lic_lines").ID("id").ParentID("gadget_id").
		Field("ItemID", "item_id").Field("Note", "note")
}

func licPrimary() *query.ViewDefinition {
	root := core.NewTableSchema[licGadget]("lic_gadgets").ID("id").Field("Code", "code").
		DeletedAt("deleted_at").Child(licChildSchema())
	return query.View("lic_gadgets").Version(1).Schema(root)
}

func licItemSchema() *core.TableSchema {
	return core.NewExternalSchema("lic_items").ID("id").Field("Label", "label")
}

func licComposed() *query.ComposedViewDefinition {
	return query.ComposedView("lic_full").Primary(licPrimary()).
		LinkInChild(licChildSchema(), query.JoinUpstream(licItemSchema(), "Item", "item")).On("item_id")
}

func TestIntegration_LinkInChild(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()

	primary := licPrimary()
	composed := licComposed()
	childSeg := composed.Links()[0].ChildSegment // Go segment == doc segment for a native child

	gDoc := map[string]any{
		"_id": "g1", "code": "A", "deleted_at": nil,
		childSeg: []any{
			map[string]any{"id": "l1", "gadget_id": "g1", "item_id": "i1", "note": "one"},
			map[string]any{"id": "l2", "gadget_id": "g1", "item_id": "i2", "note": "two"},
			map[string]any{"id": "l3", "gadget_id": "g1", "item_id": nil, "note": "three"}, // null ParentID → null
			map[string]any{"id": "l4", "gadget_id": "g1", "item_id": "i9", "note": "four"}, // no leg match → null
		},
	}
	if err := m.Upsert(ctx, pc("lic_gadgets"), "g1", gDoc); err != nil {
		t.Fatalf("seed gadget: %v", err)
	}
	for _, it := range []map[string]any{{"_id": "i1", "label": "UP-1"}, {"_id": "i2", "label": "UP-2"}} {
		if err := m.Upsert(ctx, pc("lic_items"), it["_id"].(string), it); err != nil {
			t.Fatalf("seed item: %v", err)
		}
	}

	inner := NewMongoViewReader(m, testResolver).SetViews([]*query.ViewDefinition{primary})
	reader := NewComposedViewReader(inner, []*query.ComposedViewDefinition{composed}, 0)

	linesOf := func(doc map[string]any) []map[string]any {
		arr, _ := doc[childSeg].([]any)
		out := make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			if mm, ok := e.(map[string]any); ok {
				out = append(out, mm)
			}
		}
		return out
	}
	itemLabel := func(el map[string]any) any {
		if it, ok := el["Item"].(map[string]any); ok {
			return it["Label"]
		}
		return nil
	}

	// 1) whole-doc read: each element enriched by its own ParentID; LEFT null per element.
	doc, found, err := reader.ReadByID(ctx, "lic_full", "g1", queries.ReadCriteria{})
	if err != nil || !found {
		t.Fatalf("ReadByID: err=%v found=%v", err, found)
	}
	lines := linesOf(doc)
	if len(lines) != 4 {
		t.Fatalf("expected 4 child lines, got %d (%#v)", len(lines), doc[childSeg])
	}
	for i, want := range []any{"UP-1", "UP-2", nil, nil} {
		if got := itemLabel(lines[i]); got != want {
			t.Errorf("line %d: Item.Label = %#v, want %#v", i, got, want)
		}
	}

	// 2) segment FILTER: filters what enters the enrichment, per element — never
	// which child elements or primary rows return. UP-2 is filtered to null.
	fdoc, _, err := reader.ReadByID(ctx, "lic_full", "g1", queries.ReadCriteria{
		Filter: map[string]any{childSeg + ".Item.Label": "UP-1"},
	})
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	flines := linesOf(fdoc)
	if len(flines) != 4 {
		t.Fatalf("filter must not drop child elements, got %d", len(flines))
	}
	if itemLabel(flines[0]) != "UP-1" {
		t.Errorf("filter: l1 should keep UP-1, got %#v", flines[0]["Item"])
	}
	if itemLabel(flines[1]) != nil {
		t.Errorf("filter: l2 (UP-2) must be filtered to null, got %#v", flines[1]["Item"])
	}

	// 3) ?fields= into the in-child segment alongside a primary field:
	// ensureChildProjection must force the child array + element ParentID so the join
	// survives the sparse projection, and the helper ParentID is stripped afterward.
	// (Pruning the child's OWN fields is a general reader concern, not LinkInChild's
	// — the child array is materialized on the primary and returns whole.)
	pdoc, _, err := reader.ReadByID(ctx, "lic_full", "g1", queries.ReadCriteria{
		Projection: queries.ProjectOnlyPaths("Code", ".Item.Label"),
	})
	if err != nil {
		t.Fatalf("projected read: %v", err)
	}
	plines := linesOf(pdoc)
	el0 := plines[0]
	if itemLabel(el0) != "UP-1" {
		t.Errorf("fields: enrichment must survive a sparse projection, got %#v", el0["Item"])
	}
	if _, present := el0["ItemID"]; present {
		t.Errorf("fields: the helper ParentID must be stripped from the element, got %#v", el0)
	}
}

func (licLine) CollectionName() string { return "LicLines" }
