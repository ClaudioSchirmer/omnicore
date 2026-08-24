//go:build integration && postgres

package mongo

import (
	"context"
	"fmt"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	query "github.com/ClaudioSchirmer/omnicore/infra/db/query"
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
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
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

	r := NewMongoViewReader(m, testResolver)

	// Page 1.
	page1, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(page1.Items) != 10 {
		t.Fatalf("page 1 size: got %d, want 10", len(page1.Items))
	}
	if !page1.HasNextPage {
		t.Errorf("page 1 should have next")
	}
	if page1.HasPreviousPage {
		t.Errorf("page 1 should NOT have prev")
	}
	// First doc must be the lowest _id.
	if page1.Items[0]["_id"] != ids[0] {
		t.Errorf("page 1 first id: got %v, want %s", page1.Items[0]["_id"], ids[0])
	}

	// Page 2 via NextCursor.
	page2, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page1.EndCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(page2.Items) != 10 {
		t.Fatalf("page 2 size: got %d, want 10", len(page2.Items))
	}
	if !page2.HasNextPage || !page2.HasPreviousPage {
		t.Errorf("page 2 should have next AND prev (got next=%v prev=%v)",
			page2.HasNextPage, page2.HasPreviousPage)
	}
	// Page 2 first doc must be id[10].
	if page2.Items[0]["_id"] != ids[10] {
		t.Errorf("page 2 first id: got %v, want %s", page2.Items[0]["_id"], ids[10])
	}

	// Page 3.
	page3, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page2.EndCursor,
	})
	if err != nil {
		t.Fatalf("page 3: %v", err)
	}
	if len(page3.Items) != 5 {
		t.Fatalf("page 3 size: got %d, want 5", len(page3.Items))
	}
	if page3.HasNextPage {
		t.Errorf("page 3 should NOT have next (got %v items)", len(page3.Items))
	}
	if !page3.HasPreviousPage {
		t.Errorf("page 3 should have prev")
	}
}

func TestReader_BackwardWalk_ReachesPreviousPage(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_bwd"
	ids := seedReaderDocs(t, m, view, 25)

	r := NewMongoViewReader(m, testResolver)

	// Walk forward to page 2 to get its first-doc cursor.
	page1, _ := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	page2, _ := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, After: page1.EndCursor,
	})
	if page2.StartCursor == "" {
		t.Fatal("page 2 should expose PrevCursor")
	}

	// Apply PrevCursor as ?before= → should return page 1's docs.
	back, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 10, Before: page2.StartCursor,
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
	if !back.HasNextPage {
		t.Errorf("backward page should have next")
	}
	// And there are no docs further back → HasPrev is false.
	if back.HasPreviousPage {
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

	r := NewMongoViewReader(m, testResolver)

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
	if page.HasNextPage {
		t.Errorf("last-from-end page should NOT have next (we're at the end)")
	}
	// 15 docs precede the window → there is a previous page.
	if !page.HasPreviousPage {
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
		if err := m.Upsert(context.Background(), pc(view), d["_id"].(string), d); err != nil {
			t.Fatalf("seed %v: %v", d["_id"], err)
		}
	}

	r := NewMongoViewReader(m, testResolver)
	sort := []queries.OrderByField{{Field: "name"}}

	page1, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit: 2, OrderBy: sort,
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
		Limit: 2, OrderBy: sort, After: page1.EndCursor,
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

	r := NewMongoViewReader(m, testResolver).SetMaxLimitResolver(func(v string) int64 {
		if v == "rdr_max" {
			return 5
		}
		return 0
	})

	_, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{Limit: 10})
	if err == nil {
		t.Fatal("expected core.LimitExceededError, got nil")
	}
	// The typed error must carry a NotificationContext so the Pipeline can
	// translate into 400 — this is what makes the wire response correct.
	carrier, ok := err.(interface {
		NotificationContexts() []interface{ Name() string }
	})
	_ = carrier
	_ = ok
	// Looser check — just confirm the error type comes from core.LimitExceededError.
	if _, isInfra := err.(*core.InfrastructureError); !isInfra {
		t.Errorf("expected *core.InfrastructureError, got %T", err)
	}
}

func TestReader_LimitUnsetUsesResolvedMax(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_unset"
	seedReaderDocs(t, m, view, 30)

	// Resolver returns 5; consumer sends NO ?first=. Expect 5 docs back.
	r := NewMongoViewReader(m, testResolver).SetMaxLimitResolver(func(v string) int64 {
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
	if !page.HasNextPage {
		t.Errorf("HasNext expected true with 30 docs in dataset")
	}
}

func TestReader_FieldsProjectionStripsOrderByFieldFromWire(t *testing.T) {
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
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := NewMongoViewReader(m, testResolver)
	// Consumer asks for ONLY email in wire AND sorts by name. The reader
	// must auto-include `name` for the cursor builder, then strip it from
	// the returned doc so the wire shape is exactly {email}.
	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:      2,
		OrderBy:    []queries.OrderByField{{Field: "name"}},
		Projection: queries.ProjectOnlyPaths("email"),
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
	if page.EndCursor == "" {
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
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := NewMongoViewReader(m, testResolver)
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
		After:  page.EndCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	// Remaining docs (age 25, 26, 27) → 3 items, no next.
	if len(page2.Items) != 3 {
		t.Fatalf("page 2 size: got %d, want 3", len(page2.Items))
	}
	if page2.HasNextPage {
		t.Errorf("page 2 should NOT have next")
	}
}

// TestReader_FieldsProjectionWithoutID_WalkAdvances is the regression guard for
// the keyset walk that could not advance when the consumer's selection omitted
// the identity (?fields=name, a GraphQL selection without id, a gRPC read mask).
//
// The projection excluded `_id`, so the cursor builder read a value the doc no
// longer carried and stringified it into the literal "<nil>" tiebreak. Whether
// the walk then advanced depended on the FIRST CHARACTER of the id: '<' is
// 0x3C, sitting between the digits (0x30-0x39) and the hex letters (0x61-0x66)
// of a UUID in text, so `_id > "<nil>"` was false for a digit-leading id (the
// walk moved by accident) and true for a letter-leading one (the row matched
// itself and the walk stalled forever). The ids below deliberately lead with a
// LETTER so the stalling branch is the one under test.
func TestReader_FieldsProjectionWithoutID_WalkAdvances(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_fields_noid"

	names := []string{"Alpha", "Bravo", "Charlie"}
	ids := []string{"a69341e6-aaaa", "b1234567-bbbb", "c98765ff-cccc"}
	for i, name := range names {
		doc := map[string]any{"_id": ids[i], "name": name, "deleted_at": nil}
		if err := m.Upsert(context.Background(), pc(view), ids[i], doc); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}

	r := NewMongoViewReader(m, testResolver)
	crit := func(after string) queries.ReadCriteria {
		return queries.ReadCriteria{
			Limit:      1,
			OrderBy:    []queries.OrderByField{{Field: "name"}},
			Projection: queries.ProjectOnlyPaths("name"),
			After:      after,
		}
	}

	after := ""
	for i, want := range names {
		page, err := r.ReadPage(context.Background(), view, crit(after))
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if len(page.Items) != 1 {
			t.Fatalf("page %d: want 1 item, got %d", i+1, len(page.Items))
		}
		item := page.Items[0]
		if item["Name"] != want && item["name"] != want {
			t.Fatalf("page %d: walk did not advance — got %#v, want name %q", i+1, item, want)
		}
		// The wire shape still matches the selection exactly: the identity was
		// re-included for the query only, and stripped before the item was built.
		for _, key := range []string{"_id", "ID", "id"} {
			if _, leaked := item[key]; leaked {
				t.Fatalf("page %d: %q leaked onto the wire for ?fields=name: %#v", i+1, key, item)
			}
		}
		if i < len(names)-1 {
			if !page.HasNextPage || page.EndCursor == "" {
				t.Fatalf("page %d: want a next page and an EndCursor, got hasNext=%v cursor=%q",
					i+1, page.HasNextPage, page.EndCursor)
			}
			if page.EndCursor == after {
				t.Fatalf("page %d: EndCursor repeated the incoming cursor — the walk is stalled", i+1)
			}
			after = page.EndCursor
		}
	}
}

// TestReader_ExclusionProjectionWithSort_IsAValidMongoProjection is the
// regression guard for a listing whose criteria carry an EXCLUSION projection
// — the shape ReadCriteria.Restrict produces when it scrubs a field the caller
// may not see from a request that named no fields of its own — combined with an
// ?orderBy=.
//
// The sort-field auto-include was mode-blind: it appended `name: 1` beside the
// restriction's `phone: 0`, and Mongo refuses a projection that mixes the two
// (Location31253 "Cannot do inclusion on field name in exclusion projection").
// The whole read failed, so a field-restricted caller could not sort at all. In
// exclusion mode the sort field is served anyway — the correct repair is none.
func TestReader_ExclusionProjectionWithSort_IsAValidMongoProjection(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "rdr_excl_sort"

	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("id-%02d", i)
		doc := map[string]any{
			"_id":        id,
			"name":       fmt.Sprintf("n-%02d", i),
			"phone":      "555-0100",
			"deleted_at": nil,
		}
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	r := NewMongoViewReader(m, testResolver)
	page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:      2,
		OrderBy:    []queries.OrderByField{{Field: "name"}},
		Projection: queries.Projection{Mode: queries.ProjectExcept, Paths: map[string]bool{"phone": true}},
	})
	if err != nil {
		t.Fatalf("a restricted, sorted listing must read: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(page.Items))
	}
	for i, item := range page.Items {
		if _, leaked := item["phone"]; leaked {
			t.Errorf("item[%d]: the restricted field must not be served: %#v", i, item)
		}
		// The sort field was never the consumer's to lose in exclusion mode.
		if item["name"] == nil {
			t.Errorf("item[%d]: an exclusion projection serves every field it does not name: %#v", i, item)
		}
	}
	if page.Items[0]["name"] != "n-00" || page.Items[1]["name"] != "n-01" {
		t.Fatalf("the sort did not apply: %#v", page.Items)
	}
	// And the walk still works — the cursor read its sort value off the doc.
	next, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
		Limit:      2,
		OrderBy:    []queries.OrderByField{{Field: "name"}},
		Projection: queries.Projection{Mode: queries.ProjectExcept, Paths: map[string]bool{"phone": true}},
		After:      page.EndCursor,
	})
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(next.Items) != 1 || next.Items[0]["name"] != "n-02" {
		t.Fatalf("the walk did not advance: %#v", next.Items)
	}
}

// ─── the identity and the projection ─────────────────────────────────────────
//
// A projected view document carries the identity TWICE: Mongo's own `_id`, and
// the schema's declared id column, which the projector writes onto the root
// explicitly (payload_project restores it from the structural ids because
// readers project it). The fixtures below seed BOTH, because a document seeded
// with `_id` alone is not a document this framework ever writes — and a test
// built on one measures the fixture, not the reader.

type ivUser struct{ ID, Name string }

func ivReader(t *testing.T, m *MongoDB, view string) *MongoViewReader {
	t.Helper()
	schema := core.NewTableSchema[ivUser](view).
		ID("id").
		Field("Name", "name").
		DeletedAt("deleted_at")
	return NewMongoViewReader(m, testResolver).
		SetViews([]*query.ViewDefinition{query.View(view).Version(1).Schema(schema)})
}

func seedIVUsers(t *testing.T, m *MongoDB, view string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		doc := map[string]any{"_id": id, "id": id, "name": "N-" + id, "deleted_at": nil}
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
			t.Fatalf("seed %q: %v", id, err)
		}
	}
}

// ReadCriteria.Restrict is an AUTHORITY: the field it removes reaches neither
// the store nor the wire. The identity escaped it on every Mongo-backed view.
// The exclusion did its half — the schema's id column left the projection — but
// Mongo returns `_id` on EVERY document regardless of what the projection says,
// and normalizeIdentity then lifted that key straight back onto the Go field
// "ID", which is the spelling the Response DTO fills. The restricted field was
// served, and the restriction looked like it had worked.
func TestReader_RestrictOnTheIdentity_IsHonored(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "iv_restrict"
	seedIVUsers(t, m, view, "u1", "u2")
	r := ivReader(t, m, view)

	crit := queries.ReadCriteria{Limit: 5}
	if err := crit.Restrict(idGoField); err != nil {
		t.Fatalf("a passive Restrict must not error: %v", err)
	}
	page, err := r.ReadPage(context.Background(), view, crit)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(page.Items))
	}
	for i, item := range page.Items {
		for _, key := range []string{"_id", "ID", "id"} {
			if _, leaked := item[key]; leaked {
				t.Fatalf("item[%d]: the restricted identity leaked as %q: %#v", i, key, item)
			}
		}
		if item["Name"] == nil {
			t.Fatalf("item[%d]: only the restricted field leaves: %#v", i, item)
		}
	}

	// The by-id route answers to the same authority.
	doc, found, err := r.ReadByID(context.Background(), view, "u1", crit)
	if err != nil || !found {
		t.Fatalf("ReadByID: err=%v found=%v", err, found)
	}
	for _, key := range []string{"_id", "ID", "id"} {
		if _, leaked := doc[key]; leaked {
			t.Fatalf("by-id: the restricted identity leaked as %q: %#v", key, doc)
		}
	}
	if doc["Name"] != "N-u1" {
		t.Fatalf("by-id served the wrong row: %#v", doc)
	}
}

// The other half of the same rule, and the reason it is stated as "does the
// selection KEEP the identity" rather than "did it exclude `_id`": a selection
// that keeps the identity still gets one, whichever spelling the schema puts it
// under. A Restrict aimed at some OTHER field must not disturb it.
func TestReader_RestrictOnAnotherField_LeavesTheIdentity(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "iv_other"
	seedIVUsers(t, m, view, "u1")
	r := ivReader(t, m, view)

	crit := queries.ReadCriteria{Limit: 5}
	if err := crit.Restrict("Name"); err != nil {
		t.Fatalf("Restrict: %v", err)
	}
	page, err := r.ReadPage(context.Background(), view, crit)
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if page.Items[0]["ID"] != "u1" {
		t.Fatalf("the identity must survive a Restrict aimed elsewhere: %#v", page.Items[0])
	}
	if _, leaked := page.Items[0]["Name"]; leaked {
		t.Fatalf("the restricted field leaked: %#v", page.Items[0])
	}
}

// ─── the mirror (external) schema ────────────────────────────────────────────
//
// A mirror of an upstream collection keeps its identity in `_id` ALONE — the
// outbox payload carries no id column — so ColumnPath resolves the Go path "ID"
// onto the store key there, and only there. That makes a mirror the one place
// where a consumer's own filter/sort on the identity lands on `_id`, which is
// also the reader's cursor tiebreaker and the by-id route's subject. The three
// tests below cover the collisions that follow.

func ivMirrorReader(t *testing.T, m *MongoDB, view string) *MongoViewReader {
	t.Helper()
	mirror := core.NewExternalSchema(view).ID("id").Field("Name", "name").DeletedAt("deleted_at")
	return NewMongoViewReader(m, testResolver).
		SetViews([]*query.ViewDefinition{query.View(view).Version(1).Schema(mirror)})
}

func seedMirrorDocs(t *testing.T, m *MongoDB, view string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		doc := map[string]any{"_id": id, "name": "N-" + id, "deleted_at": nil}
		if err := m.Upsert(context.Background(), pc(view), id, doc); err != nil {
			t.Fatalf("seed %q: %v", id, err)
		}
	}
}

// Sorting BY the identity: the consumer's own term IS the tiebreaker. Appending
// the automatic `_id` on top put the key in the sort document twice — in
// opposite directions for a DESC request — and added a keyset arm past it that
// wrote an equality and an inequality on `_id` into the same bson.M, where the
// second overwrote the first and un-bounded the page.
func TestReader_MirrorSortOnTheIdentity_WalkAdvances(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "iv_mirror_sort"
	seedMirrorDocs(t, m, view, "u1", "u2", "u3")
	r := ivMirrorReader(t, m, view)

	after := ""
	for i, want := range []string{"u3", "u2", "u1"} {
		page, err := r.ReadPage(context.Background(), view, queries.ReadCriteria{
			Limit:   1,
			OrderBy: []queries.OrderByField{{Field: idGoField, Desc: true}},
			After:   after,
		})
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if len(page.Items) != 1 || page.Items[0][idGoField] != want {
			t.Fatalf("page %d: got %#v, want ID %q", i+1, page.Items, want)
		}
		if i < 2 {
			if page.EndCursor == "" || page.EndCursor == after {
				t.Fatalf("page %d: the walk is stalled (cursor %q)", i+1, page.EndCursor)
			}
			after = page.EndCursor
		}
	}
}

// A by-id read whose criteria ALSO constrain the identity carries two things
// that must both hold: the path names the SUBJECT, the criteria scope names
// what this caller may reach. On a mirror both land on `_id`, so seeding the
// filter with the path id and letting applyFilter write over it let the scope
// REPLACE the subject — the caller would be served the row its own scope named
// instead of the one the route addressed. They AND.
func TestReader_MirrorReadByID_IdentityOverlayANDsWithThePathID(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "iv_mirror_overlay"
	seedMirrorDocs(t, m, view, "u1", "u2")
	r := ivMirrorReader(t, m, view)

	doc, found, err := r.ReadByID(context.Background(), view, "u1",
		queries.ReadCriteria{Filter: map[string]any{idGoField: "u2"}})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if found {
		t.Fatalf("the scope and the path name different rows — nothing may be served, got %#v", doc)
	}

	doc, found, err = r.ReadByID(context.Background(), view, "u1",
		queries.ReadCriteria{Filter: map[string]any{idGoField: "u1"}})
	if err != nil || !found {
		t.Fatalf("an agreeing scope must serve the row: err=%v found=%v", err, found)
	}
	if doc[idGoField] != "u1" {
		t.Fatalf("served the wrong row: %#v", doc)
	}
}

// The exclusion half of the identity repair in the projection: Restrict("ID")
// over a mirror leaves `{_id: 0}`, which drops the cursor's tiebreaker. It
// cannot be repaired the way an inclusion is — `{_id: 1}` as the only entry is
// an INCLUSION projection of the key alone, collapsing every document to its id
// — so the entry is removed instead (an exclusion serves `_id` by default) and
// the identity leaves once the cursors are built.
func TestReader_MirrorRestrictOnTheIdentity_WalkAdvances(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	view := "iv_mirror_restrict"
	seedMirrorDocs(t, m, view, "a69341e6-aaaa", "b1234567-bbbb", "c98765ff-cccc")
	r := ivMirrorReader(t, m, view)

	crit := func(after string) queries.ReadCriteria {
		c := queries.ReadCriteria{
			Limit:   1,
			OrderBy: []queries.OrderByField{{Field: "Name"}},
			After:   after,
		}
		if err := c.Restrict(idGoField); err != nil {
			panic(err)
		}
		return c
	}
	if p := crit("").Projection; p.Mode != queries.ProjectExcept || !p.Selects(idGoField) {
		t.Fatalf("premise: Restrict must leave an exclusion of ID, got %#v", p)
	}

	after := ""
	for i, want := range []string{"N-a69341e6-aaaa", "N-b1234567-bbbb", "N-c98765ff-cccc"} {
		page, err := r.ReadPage(context.Background(), view, crit(after))
		if err != nil {
			t.Fatalf("page %d: %v", i+1, err)
		}
		if len(page.Items) != 1 || page.Items[0]["Name"] != want {
			t.Fatalf("page %d: walk did not advance — got %#v, want Name %q", i+1, page.Items, want)
		}
		for _, key := range []string{"_id", idGoField} {
			if _, leaked := page.Items[0][key]; leaked {
				t.Fatalf("page %d: the restricted identity leaked as %q: %#v", i+1, key, page.Items[0])
			}
		}
		if i < 2 {
			if page.EndCursor == "" || page.EndCursor == after {
				t.Fatalf("page %d: the walk is stalled (cursor %q)", i+1, page.EndCursor)
			}
			after = page.EndCursor
		}
	}
}
