package query

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The batched compose path (ComposeBatch / ComposeAll) must fetch each related
// source ONCE for the whole batch — one IN (...) query, not one per root — while
// still merging each root's own related rows. This is the N+1 collapse that turns
// a millions-row rebuild from hours into minutes; the win is largest on engines
// with a heavy per-query cost.
func TestComposeBatch_SiblingFetchedOncePerBatch(t *testing.T) {
	rootCalls, sibCalls, sibArgs := 0, 0, 0
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM orders_ext"):
			sibCalls++
			sibArgs = len(args)
			if !strings.Contains(sql, " IN (") {
				t.Errorf("sibling fetch must use an IN (...) predicate, got: %s", sql)
			}
			return mapsFromColsData([]string{"id", "email"}, [][]any{{"o1", "a@x"}, {"o2", "b@y"}}), nil
		case strings.Contains(sql, "FROM orders"):
			rootCalls++
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}, {"o2", "second"}}), nil
		}
		return nil, nil
	})
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(composerSiblingRootSchema())

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1", "o2"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("want 2 composed docs, got %d", len(docs))
	}

	// The whole point: the root AND the sibling are each read ONCE for the batch.
	if rootCalls != 1 {
		t.Errorf("root fetched %d times, want 1 (set-based)", rootCalls)
	}
	if sibCalls != 1 {
		t.Errorf("sibling fetched %d times, want 1 for a 2-root batch (no N+1)", sibCalls)
	}
	if sibArgs != 2 {
		t.Errorf("sibling IN bound %d ids, want 2", sibArgs)
	}

	// And each root got ITS OWN sibling merged flat — the grouping matched rows to
	// the right root by the shared key.
	byID := map[string]Document{}
	for _, d := range docs {
		byID[fmt.Sprintf("%v", d["id"])] = d
	}
	if byID["o1"]["email"] != "a@x" {
		t.Errorf("o1 email = %v, want a@x", byID["o1"]["email"])
	}
	if byID["o2"]["email"] != "b@y" {
		t.Errorf("o2 email = %v, want b@y", byID["o2"]["email"])
	}
	if byID["o1"]["name"] != "first" || byID["o2"]["name"] != "second" {
		t.Errorf("root columns lost: %v", docs)
	}
}

// An own child collection is nested per root from a single batched fetch: one
// IN (...) query over the child table, grouped by FK back to each root.
func TestComposeBatch_OwnChildrenGroupedPerRoot(t *testing.T) {
	childCalls := 0
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM lines"):
			childCalls++
			if !strings.Contains(sql, " IN (") {
				t.Errorf("own-child fetch must use IN (...), got: %s", sql)
			}
			// two lines for o1, one for o2 — keyed by the FK order_id.
			return mapsFromColsData([]string{"id", "order_id", "label"},
				[][]any{{"l1", "o1", "A"}, {"l2", "o1", "B"}, {"l3", "o2", "C"}}), nil
		case strings.Contains(sql, "FROM orders"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"o1", "first"}, {"o2", "second"}}), nil
		}
		return nil, nil
	})
	childSchema := core.NewTableSchema[csComposeVO]("lines").PK("id").FK("order_id").Field("Label", "label")
	rootWithChild := core.NewTableSchema[*builderTestEntity]("orders").
		PK("id").Field("Name", "name").SoftDelete("deleted_at").
		Child(childSchema)
	c := NewComposer(eng)
	view := View("orders").Version(1).Root("orders").Schema(rootWithChild)

	docs, err := c.ComposeBatch(context.Background(), view, []string{"o1", "o2"})
	if err != nil {
		t.Fatalf("ComposeBatch: %v", err)
	}
	if childCalls != 1 {
		t.Errorf("child table fetched %d times, want 1 for the batch", childCalls)
	}
	seg := childDocSegment(childSchema)
	byID := map[string]Document{}
	for _, d := range docs {
		byID[fmt.Sprintf("%v", d["id"])] = d
	}
	o1Lines, _ := byID["o1"][seg].([]Document)
	o2Lines, _ := byID["o2"][seg].([]Document)
	if len(o1Lines) != 2 {
		t.Errorf("o1 children = %d, want 2 (grouped by FK)", len(o1Lines))
	}
	if len(o2Lines) != 1 {
		t.Errorf("o2 children = %d, want 1 (grouped by FK)", len(o2Lines))
	}
}
