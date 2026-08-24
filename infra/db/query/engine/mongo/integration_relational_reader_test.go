//go:build integration && postgres

package mongo

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query/engine/relational"
)

// TestRelationalViewReader_EndToEnd drives the relational read path exactly as a
// web surface does — through the queries.ViewReader port — proving the whole
// chain against a real database: a query.RelationalView is served from the SoR,
// the loader (a real read.AggregateLoader passed as the query.AggregateReader —
// this call alone proves the loader satisfies the interface structurally) loads
// the aggregate, BuildDocument maps it, ToGoDoc names it, and the offset-in-cursor
// pagination walks it under the unchanged limit/after surface.
//
// The unit suite covers each of those in isolation over fakes; only a live run
// proves the SQL the loader compiles is accepted by a backend and that the
// document assembled from the scanned entity carries what the surfaces read.
func TestRelationalViewReader_EndToEnd(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createLoaderTables(t, pg)

	ctx := context.Background()
	ids := map[string]string{}
	for _, name := range []string{"Ada", "Bob", "Cy"} {
		var id string
		if err := pg.Pool().QueryRow(ctx,
			`INSERT INTO loader_roots (name, email) VALUES ($1, $2) RETURNING id`,
			name, name+"@x.com").Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if _, err := pg.Pool().Exec(ctx,
			`INSERT INTO loader_tag_vos (loader_root_id, label) VALUES ($1, 'tag')`, id); err != nil {
			t.Fatalf("seed tag %s: %v", name, err)
		}
		ids[name] = id
	}

	schema := loaderRootSchema()
	loader := read.NewAggregateLoader[*loaderRoot](pg, func() *loaderRoot { return &loaderRoot{} }).WithSchema(schema)
	var reader queries.ViewReader = relational.NewViewReader([]*query.RelationalViewDefinition{
		query.RelationalView("loader_roots", loader),
	})

	nameOf := func(item map[string]any) string {
		s, _ := item["Name"].(string)
		return s
	}

	// ── page 1: limit 2, sorted by Name → Ada, Bob; HasNext ──
	sort := []queries.OrderByField{{Field: "Name"}}
	p1, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{Limit: 2, OrderBy: sort})
	if err != nil {
		t.Fatalf("ReadPage p1: %v", err)
	}
	if len(p1.Items) != 2 || nameOf(p1.Items[0]) != "Ada" || nameOf(p1.Items[1]) != "Bob" {
		t.Fatalf("p1 items = %v", relNames(p1.Items))
	}
	if !p1.HasNextPage {
		t.Error("p1 should have a next page")
	}
	if p1.HasPreviousPage {
		t.Error("p1 (first page) must not have a previous page")
	}
	// A LISTING carries the full match count, not the page size and not 0 — the
	// same number ?onlyTotal reports below, counted by a real COUNT against the SoR.
	if p1.TotalCount != 3 {
		t.Errorf("p1 Total = %d, want 3 (the full match count, page size 2)", p1.TotalCount)
	}
	// The identity reaches the document under the Go field the Entity contract
	// fixes — never under a store's own key, which stops at the read engine.
	if _, ok := p1.Items[0]["ID"]; !ok {
		t.Errorf("doc missing the ID Go field: %v", p1.Items[0])
	}
	if _, ok := p1.Items[0]["_id"]; ok {
		t.Errorf("the relational document must carry no store key: %v", p1.Items[0])
	}
	if _, ok := p1.Items[0]["CreatedAt"]; !ok {
		t.Errorf("doc missing CreatedAt (managed): %v", p1.Items[0])
	}

	// ── page 2: after p1's end cursor → Cy; no next ──
	p2, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{Limit: 2, OrderBy: sort, After: p1.EndCursor})
	if err != nil {
		t.Fatalf("ReadPage p2: %v", err)
	}
	if len(p2.Items) != 1 || nameOf(p2.Items[0]) != "Cy" {
		t.Fatalf("p2 items = %v", relNames(p2.Items))
	}
	if p2.HasNextPage {
		t.Error("p2 (last page) must not have a next page")
	}
	if !p2.HasPreviousPage {
		t.Error("p2 must have a previous page")
	}
	if p2.TotalCount != 3 {
		t.Errorf("p2 Total = %d, want 3 (total is a property of the match set, not the window)", p2.TotalCount)
	}

	// ── onlyTotal ──
	tot, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{OnlyTotal: true})
	if err != nil {
		t.Fatalf("ReadPage onlyTotal: %v", err)
	}
	if !tot.OnlyTotal || tot.TotalCount != 3 {
		t.Errorf("onlyTotal = %+v, want Total 3", tot)
	}
	if tot.TotalCount != p1.TotalCount {
		t.Errorf("onlyTotal Total = %d but the listing reported %d — they must agree", tot.TotalCount, p1.TotalCount)
	}

	// ── the child collection rides the served document ──
	tags, _ := p1.Items[0]["LoaderTagVOs"].([]any)
	if len(tags) != 1 {
		t.Errorf("the aggregate's child collection must be served, got %#v", p1.Items[0]["LoaderTagVOs"])
	}

	// ── ReadByID: found ──
	doc, found, err := reader.ReadByID(ctx, "loader_roots", ids["Ada"], queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if !found || doc["Name"] != "Ada" {
		t.Fatalf("ReadByID Ada = (%v, %v), want found Ada", doc, found)
	}

	// ── ReadByID: absent → not found, no error ──
	if _, found, err := reader.ReadByID(ctx, "loader_roots",
		"00000000-0000-0000-0000-000000000000", queries.ReadCriteria{}); err != nil || found {
		t.Errorf("ReadByID absent = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

func relNames(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%v", it["Name"])
	}
	return out
}
