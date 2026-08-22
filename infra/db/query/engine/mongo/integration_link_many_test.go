//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// LinkMany integration: the 1:N leg of a ComposedView resolved by the real
// server — one $match($in) + $group/$topN aggregation per leg. The unit suite
// proves the control flow against a pipeline-interpreting fake; this test
// proves the emitted pipeline against MongoDB itself: per-group ordering, the
// per-parent ceiling, LEFT semantics, the leg's archived gate and the sparse
// segment projection with the forced parent-key column stripped back out.

type lmGadget struct{ ID, Code string }
type lmNote struct{ ID, GadgetID, Text string }

func lmPrimary() *query.ViewDefinition {
	root := core.NewTableSchema[lmGadget]("lm_gadgets").ID("id").Field("Code", "code").
		DeletedAt("deleted_at")
	return query.View("lm_gadgets").Version(1).Schema(root)
}

func lmNotesView() *query.ViewDefinition {
	schema := core.NewTableSchema[lmNote]("lm_notes").ID("id").
		Field("GadgetID", "gadget_id").Field("Text", "text").DeletedAt("deleted_at")
	return query.View("lm_notes").Version(1).Schema(schema)
}

func lmComposed(primary, notes *query.ViewDefinition) *query.ComposedViewDefinition {
	return query.ComposedView("lm_full").Primary(primary).
		LinkMany(query.JoinView(notes, "Notes", "notes")).
		OrderBy("text").MaxLinkManyLimit(2).On("gadget_id")
}

func lmNotesOf(item map[string]any) []map[string]any {
	arr, _ := item["Notes"].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func lmTexts(item map[string]any) []string {
	var out []string
	for _, n := range lmNotesOf(item) {
		s, _ := n["Text"].(string)
		out = append(out, s)
	}
	return out
}

func TestIntegration_LinkManyTopN(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()

	primary, notes := lmPrimary(), lmNotesView()
	composed := lmComposed(primary, notes)

	for _, g := range []map[string]any{
		{"_id": "g1", "code": "A", "deleted_at": nil},
		{"_id": "g2", "code": "B", "deleted_at": nil},
		{"_id": "g3", "code": "C", "deleted_at": nil}, // no notes at all
	} {
		if err := m.Upsert(ctx, pc("lm_gadgets"), g["_id"].(string), g); err != nil {
			t.Fatalf("seed gadget: %v", err)
		}
	}
	// g1 is the fat parent: four active notes seeded OUT of order plus one
	// archived; g2 has one. The ceiling is 2, the declared order `text` asc.
	for _, n := range []map[string]any{
		{"_id": "n3", "gadget_id": "g1", "text": "c", "deleted_at": nil},
		{"_id": "n1", "gadget_id": "g1", "text": "a", "deleted_at": nil},
		{"_id": "n4", "gadget_id": "g1", "text": "d", "deleted_at": nil},
		{"_id": "n2", "gadget_id": "g1", "text": "b", "deleted_at": nil},
		{"_id": "n0", "gadget_id": "g1", "text": "0-archived", "deleted_at": "2026-01-01"},
		{"_id": "n5", "gadget_id": "g2", "text": "only", "deleted_at": nil},
	} {
		if err := m.Upsert(ctx, pc("lm_notes"), n["_id"].(string), n); err != nil {
			t.Fatalf("seed note: %v", err)
		}
	}

	inner := NewMongoViewReader(m, testResolver).SetViews([]*query.ViewDefinition{primary, notes})
	reader := NewComposedViewReader(inner, []*query.ComposedViewDefinition{composed}, 0)

	// 1) Ordering + per-parent ceiling, server-side: g1's segment is the first
	// TWO in declared order ([a b], never the seeded order), the archived note
	// is gated out even though "0-archived" sorts first, g2 keeps its single
	// note (the fat parent starves nobody) and g3 gets the LEFT empty array.
	page, err := reader.ReadPage(ctx, "lm_full", queries.ReadCriteria{
		OrderBy: []queries.OrderByField{{Field: "Code"}},
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("expected 3 primary rows, got %d", len(page.Items))
	}
	if got := lmTexts(page.Items[0]); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("g1 segment: want [a b] (ordered, capped, archived gated), got %v", got)
	}
	if got := lmTexts(page.Items[1]); len(got) != 1 || got[0] != "only" {
		t.Fatalf("g2 segment: want [only], got %v", got)
	}
	if got := lmNotesOf(page.Items[2]); len(got) != 0 {
		t.Fatalf("g3 segment: want the LEFT empty array, got %#v", got)
	}

	// 2) includeArchived reaches the leg: the archived note re-enters the
	// order ("0-archived" sorts first) and the ceiling still applies.
	archived, err := reader.ReadPage(ctx, "lm_full", queries.ReadCriteria{
		OrderBy:         []queries.OrderByField{{Field: "Code"}},
		IncludeArchived: true,
	})
	if err != nil {
		t.Fatalf("archived read: %v", err)
	}
	if got := lmTexts(archived.Items[0]); len(got) != 2 || got[0] != "0-archived" || got[1] != "a" {
		t.Fatalf("archived g1 segment: want [0-archived a], got %v", got)
	}

	// 3) A segment filter shapes segment CONTENT under the same aggregation —
	// and never row selection.
	filtered, err := reader.ReadPage(ctx, "lm_full", queries.ReadCriteria{
		OrderBy: []queries.OrderByField{{Field: "Code"}},
		Filter:  map[string]any{"Notes.Text": "b"},
	})
	if err != nil {
		t.Fatalf("filtered read: %v", err)
	}
	if len(filtered.Items) != 3 {
		t.Fatalf("segment filter must not select rows, got %d", len(filtered.Items))
	}
	if got := lmTexts(filtered.Items[0]); len(got) != 1 || got[0] != "b" {
		t.Fatalf("filtered g1 segment: want [b], got %v", got)
	}

	// 4) Sparse projection into the segment: the parent-key column is forced
	// into the $project for the $group and stripped back out, so an entry
	// carries exactly the asked leaf.
	projected, err := reader.ReadPage(ctx, "lm_full", queries.ReadCriteria{
		OrderBy:    []queries.OrderByField{{Field: "Code"}},
		Projection: queries.ProjectOnlyPaths("Code", "Notes.Text"),
	})
	if err != nil {
		t.Fatalf("projected read: %v", err)
	}
	pn := lmNotesOf(projected.Items[0])
	if len(pn) != 2 || pn[0]["Text"] != "a" {
		t.Fatalf("projected g1 segment: want the ordered capped pair, got %#v", pn)
	}
	for _, key := range []string{"GadgetID", "_id"} {
		if _, present := pn[0][key]; present {
			t.Fatalf("projected entry must not leak the %s helper, got %#v", key, pn[0])
		}
	}
}
