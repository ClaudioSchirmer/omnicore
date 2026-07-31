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
// chain: a view marked RelationalSource(loader) is served from the SoR, the
// loader (a real read.AggregateLoader passed as the RelationalReader — this call
// alone proves the loader satisfies the interface structurally) loads the
// aggregate, BuildDocument maps it, ToGoDoc names it, and the offset-in-cursor
// pagination walks it under the unchanged limit/after surface.
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
	vdef := query.View("loader_roots").Schema(schema).Version(1).RelationalSource(loader)
	var reader queries.ViewReader = relational.NewRelationalViewReader([]*query.ViewDefinition{vdef})

	nameOf := func(item map[string]any) string {
		s, _ := item["Name"].(string)
		return s
	}

	// ── page 1: limit 2, sorted by Name → Ada, Bob; HasNext ──
	sort := []queries.SortField{{Field: "Name"}}
	p1, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{Limit: 2, Sort: sort})
	if err != nil {
		t.Fatalf("ReadPage p1: %v", err)
	}
	if len(p1.Items) != 2 || nameOf(p1.Items[0]) != "Ada" || nameOf(p1.Items[1]) != "Bob" {
		t.Fatalf("p1 items = %v", names(p1.Items))
	}
	if !p1.HasNext {
		t.Error("p1 should have a next page")
	}
	if p1.HasPrev {
		t.Error("p1 (first page) must not have a previous page")
	}
	// the doc carries the mapped root fields + the managed columns
	if _, ok := p1.Items[0]["_id"]; !ok {
		t.Errorf("doc missing _id: %v", p1.Items[0])
	}
	if _, ok := p1.Items[0]["CreatedAt"]; !ok {
		t.Errorf("doc missing CreatedAt (managed): %v", p1.Items[0])
	}

	// ── page 2: after p1's end cursor → Cy; no next ──
	p2, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{Limit: 2, Sort: sort, After: p1.NextCursor})
	if err != nil {
		t.Fatalf("ReadPage p2: %v", err)
	}
	if len(p2.Items) != 1 || nameOf(p2.Items[0]) != "Cy" {
		t.Fatalf("p2 items = %v", names(p2.Items))
	}
	if p2.HasNext {
		t.Error("p2 (last page) must not have a next page")
	}
	if !p2.HasPrev {
		t.Error("p2 must have a previous page")
	}

	// ── onlyTotal ──
	tot, err := reader.ReadPage(ctx, "loader_roots", queries.ReadCriteria{OnlyTotal: true})
	if err != nil {
		t.Fatalf("ReadPage onlyTotal: %v", err)
	}
	if !tot.OnlyTotal || tot.Total != 3 {
		t.Errorf("onlyTotal = %+v, want Total 3", tot)
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

func names(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = fmt.Sprintf("%v", it["Name"])
	}
	return out
}
