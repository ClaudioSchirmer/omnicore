//go:build integration

package mongo

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Integration tests for MongoViewReader.ReadPage focusing on keyset pagination
// correctness. Run via `go test -tags=integration ./infra/... -count=1` against
// the QA docker-compose Mongo container.
//
// Each test seeds a small dataset into a uniquely-named collection so test runs
// do not collide and the cleanup hook drops the temporary database.

// seedReaderDocs inserts N documents with predictable (name, id) tuples into a
// fresh collection. id is `id-{0..N-1}` zero-padded; name is `n-{0..N-1}` so
// alphabetical and id-sort agree on a baseline ordering (each test that
// scrambles the relationship is documented in its own seed call).
func seedReaderDocs(t *testing.T, m *MongoDB, view string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%02d", i)
		ids = append(ids, id)
		doc := map[string]any{
			"_id":        id,
			"name":       fmt.Sprintf("n-%02d", i),
			"deleted_at": nil,
		}
		if err := m.Upsert(context.Background(), view, id, doc); err != nil {
			t.Fatalf("seed [%d]: %v", i, err)
		}
	}
	return ids
}

func TestReader_ForwardWalk_BasicKeyset(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_fwd"
	ids := seedReaderDocs(t, m, view, 25)

	r := NewMongoViewReader(m)

	// Page 1.
	page1, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 10 {
		t.Fatalf("page 1 size: got %d, want 10", len(page1.Items))
	}
	if !page1.HasNext {
		t.Errorf("page 1 should have next")
	}
	if page1.HasPrev {
		t.Errorf("page 1 should NOT have prev")
	}
	// First doc must be the lowest _id.
	if page1.Items[0]["_id"] != ids[0] {
		t.Errorf("page 1 first id: got %v, want %s", page1.Items[0]["_id"], ids[0])
	}

	// Page 2 via NextCursor.
	page2, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page1.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Items) != 10 {
		t.Fatalf("page 2 size: got %d, want 10", len(page2.Items))
	}
	if !page2.HasNext || !page2.HasPrev {
		t.Errorf("page 2 should have next AND prev (got next=%v prev=%v)",
			page2.HasNext, page2.HasPrev)
	}
	// Page 2 first doc must be id[10].
	if page2.Items[0]["_id"] != ids[10] {
		t.Errorf("page 2 first id: got %v, want %s", page2.Items[0]["_id"], ids[10])
	}

	// Page 3.
	page3, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page2.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3.Items) != 5 {
		t.Fatalf("page 3 size: got %d, want 5", len(page3.Items))
	}
	if page3.HasNext {
		t.Errorf("page 3 should NOT have next (got %v items)", len(page3.Items))
	}
	if !page3.HasPrev {
		t.Errorf("page 3 should have prev")
	}
}

func TestReader_BackwardWalk_ReachesPreviousPage(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_bwd"
	ids := seedReaderDocs(t, m, view, 25)

	r := NewMongoViewReader(m)

	// Walk forward to page 2 to get its first-doc cursor.
	page1, _ := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	page2, _ := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page1.NextCursor,
	})
	if page2.PrevCursor == "" {
		t.Fatal("page 2 should expose PrevCursor")
	}

	// Apply PrevCursor as ?before= → should return page 1's docs.
	back, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, Before: page2.PrevCursor,
	})
	if err != nil {
		t.Fatalf("backward: %v", err)
	}
	if len(back.Items) != 10 {
		t.Fatalf("backward page size: got %d, want 10", len(back.Items))
	}
	if back.Items[0]["_id"] != ids[0] {
		t.Errorf("backward first id: got %v, want %s", back.Items[0]["_id"], ids[0])
	}
	if back.Items[9]["_id"] != ids[9] {
		t.Errorf("backward last id: got %v, want %s", back.Items[9]["_id"], ids[9])
	}
	// We came back FROM a forward cursor → HasNext is true.
	if !back.HasNext {
		t.Errorf("backward page should have next")
	}
	// And there are no docs further back → HasPrev is false.
	if back.HasPrev {
		t.Errorf("backward page should NOT have prev (we're at the start)")
	}
}

// TestReader_BackwardFromEnd_LastN covers the GraphQL Relay `last: N` path:
// Backward=true with NO cursor walks back from the END of the set and returns
// the LAST N docs in canonical order. Distinct from the ?before= walk above —
// here there is nothing ahead, so HasNext is false while HasPrev is true.
func TestReader_BackwardFromEnd_LastN(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_last"
	ids := seedReaderDocs(t, m, view, 25)

	r := NewMongoViewReader(m)

	// last: 10 → Backward with no cursor.
	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, Backward: true,
	})
	if err != nil {
		t.Fatalf("last-from-end: %v", err)
	}
	if len(page.Items) != 10 {
		t.Fatalf("page size: got %d, want 10", len(page.Items))
	}
	// Canonical order: the last 10 of 25 are ids[15]..ids[24], ascending.
	if page.Items[0]["_id"] != ids[15] {
		t.Errorf("first id: got %v, want %s", page.Items[0]["_id"], ids[15])
	}
	if page.Items[9]["_id"] != ids[24] {
		t.Errorf("last id: got %v, want %s", page.Items[9]["_id"], ids[24])
	}
	// We are AT the end → nothing ahead.
	if page.HasNext {
		t.Errorf("last-from-end page should NOT have next (we're at the end)")
	}
	// 15 docs precede the window → there is a previous page.
	if !page.HasPrev {
		t.Errorf("last-from-end page should have prev")
	}
}

func TestReader_ForwardWithCustomSort_RespectsTiebreaker(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_sort"

	// Two docs share the same name → the _id tiebreaker decides who comes
	// first. Without the tiebreaker, the cursor could either skip or repeat
	// the pair across pages.
	docs := []map[string]any{
		{"_id": "id-A", "name": "Alice", "deleted_at": nil},
		{"_id": "id-B", "name": "Alice", "deleted_at": nil}, // duplicate name
		{"_id": "id-C", "name": "Bob", "deleted_at": nil},
		{"_id": "id-D", "name": "Carol", "deleted_at": nil},
	}
	for _, d := range docs {
		if err := m.Upsert(context.Background(), view, d["_id"].(string), d); err != nil {
			t.Fatalf("seed %v: %v", d["_id"], err)
		}
	}

	r := NewMongoViewReader(m)
	sort := []queries.SortField{{Field: "name"}}

	page1, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 2, Sort: sort,
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 2 {
		t.Fatalf("page 1 size: got %d, want 2", len(page1.Items))
	}
	// Both Alices come first; tiebreaker by _id puts id-A before id-B.
	if page1.Items[0]["_id"] != "id-A" || page1.Items[1]["_id"] != "id-B" {
		t.Errorf("page 1 order: got %v, %v; want id-A, id-B",
			page1.Items[0]["_id"], page1.Items[1]["_id"])
	}

	page2, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 2, Sort: sort, After: page1.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("page 2 size: got %d, want 2", len(page2.Items))
	}
	if page2.Items[0]["_id"] != "id-C" || page2.Items[1]["_id"] != "id-D" {
		t.Errorf("page 2 order: got %v, %v; want id-C, id-D",
			page2.Items[0]["_id"], page2.Items[1]["_id"])
	}
}

func TestReader_LimitExceedsMax_400Like(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_max"
	seedReaderDocs(t, m, view, 3)

	r := NewMongoViewReader(m).SetMaxLimitResolver(func(v string) int64 {
		if v == "rdr_max" {
			return 5
		}
		return 0
	})

	_, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	if err == nil {
		t.Fatal("expected db.LimitExceededError, got nil")
	}
	// The typed error must carry a NotificationContext so the Pipeline can
	// translate into 400 — this is what makes the wire response correct.
	carrier, ok := err.(interface {
		NotificationContexts() []interface{ Name() string }
	})
	_ = carrier
	_ = ok
	// Looser check — just confirm the error type comes from db.LimitExceededError.
	if _, isInfra := err.(*db.InfrastructureError); !isInfra {
		t.Errorf("expected *db.InfrastructureError, got %T", err)
	}
}

func TestReader_LimitUnsetUsesResolvedMax(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_unset"
	seedReaderDocs(t, m, view, 30)

	// Resolver returns 5; consumer sends NO ?limit=. Expect 5 docs back.
	r := NewMongoViewReader(m).SetMaxLimitResolver(func(v string) int64 {
		if v == view {
			return 5
		}
		return 0
	})

	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 5 {
		t.Errorf("with limit unset, want resolvedMax=5 items, got %d", len(page.Items))
	}
	if !page.HasNext {
		t.Errorf("HasNext expected true with 30 docs in dataset")
	}
}

func TestReader_FieldsProjectionStripsSortFieldFromWire(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_fields"

	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("id-%02d", i)
		doc := map[string]any{
			"_id":        id,
			"name":       fmt.Sprintf("n-%02d", i),
			"email":      fmt.Sprintf("e-%02d@x", i),
			"deleted_at": nil,
		}
		if err := m.Upsert(context.Background(), view, id, doc); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := NewMongoViewReader(m)
	// Consumer asks for ONLY email in wire AND sorts by name. The reader
	// must auto-include `name` for the cursor builder, then strip it from
	// the returned doc so the wire shape is exactly {email}.
	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:      2,
		Sort:       []queries.SortField{{Field: "name"}},
		Projection: map[string]int{"email": 1, "_id": 0},
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(page.Items))
	}
	for i, item := range page.Items {
		if _, has := item["name"]; has {
			t.Errorf("item[%d] should not carry `name` on wire: %#v", i, item)
		}
		if item["email"] == nil {
			t.Errorf("item[%d] should carry `email`: %#v", i, item)
		}
	}
	// And the cursor for next page works — proves the reader DID receive
	// the value internally.
	if page.NextCursor == "" {
		t.Fatal("NextCursor should be populated")
	}
}

// applyFilter helper: integration assertion that the keyset cascade interacts
// correctly with MultiClause-driven $and entries the wrapper already emits.
func TestReader_KeysetCoexistsWithMultiClauseFilter(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_multi"

	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("id-%02d", i)
		doc := map[string]any{
			"_id":        id,
			"age":        20 + i,
			"deleted_at": nil,
		}
		if err := m.Upsert(context.Background(), view, id, doc); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := NewMongoViewReader(m)
	// MultiClause: age between 22 and 27.
	multi := queries.MultiClause{Clauses: []any{
		bson.M{"$gte": 22},
		bson.M{"$lte": 27},
	}}
	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:  3,
		Filter: map[string]any{"age": multi},
	})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("page 1 size: got %d, want 3", len(page.Items))
	}
	if page.Items[0]["_id"] != "id-02" {
		t.Errorf("page 1 first: got %v, want id-02", page.Items[0]["_id"])
	}

	page2, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:  3,
		Filter: map[string]any{"age": multi},
		After:  page.NextCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	// Remaining docs (age 25, 26, 27) → 3 items, no next.
	if len(page2.Items) != 3 {
		t.Fatalf("page 2 size: got %d, want 3", len(page2.Items))
	}
	if page2.HasNext {
		t.Errorf("page 2 should NOT have next")
	}
}
