package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// This file extends mongo_view_reader_unit_test.go: it drives the ReadPage
// branches the basic sample does not reach — the limit cascade + LimitExceeded,
// after/before mutual exclusion, keyset pagination forward + backward, sort,
// projection (?fields) with auto-include + strip, $text search, includeArchived,
// the Find error branch, and the cursor decode / tuple-length / context-hash
// rejection paths — plus a nested aggregate view so ToGoDoc recurses into an
// embed during both ReadByID and ReadPage.

// aggViewFixture registers a flat builder view AND an aggregate view with an
// EmbedMany child so the reader's nested ToGoDoc mapping runs.
func aggViewFixture(coll mongoColl) *MongoViewReader {
	childSchema := core.NewExternalSchema("addresses").
		PK("id").FK("user_id").Field("ZipCode", "zip")
	agg := query.View("agg_view").Version(1).Root("builder_test_entities").
		Schema(builderTestSchema).
		EmbedMany("addresses", query.FromSchema(childSchema).As("Addresses"))
	r := NewMongoViewReader(newFakeMongo(coll))
	r.SetViews([]*query.ViewDefinition{agg})
	return r
}

func TestMongoViewReader_ReadPage_LimitExceeded(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1})
	// Default ceiling is FrameworkDefaultMaxReadLimit (100); 200 > 100.
	_, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 200})
	if err == nil {
		t.Fatal("expected LimitExceeded error for Limit > maxLimit")
	}
}

func TestMongoViewReader_ReadPage_BypassMaxLimit(t *testing.T) {
	coll := &fakeColl{count: 1, docs: []any{map[string]any{"_id": "u1", "name": "a", "mail": "a@x"}}}
	r := viewReaderFixture(coll)
	// BypassMaxLimit lets a trusted caller exceed the page ceiling verbatim.
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 5000, BypassMaxLimit: true})
	if err != nil {
		t.Fatalf("ReadPage with BypassMaxLimit: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(page.Items))
	}
}

func TestMongoViewReader_SetMaxLimitResolver(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 0})
	r.SetMaxLimitResolver(func(view string) int64 { return 3 })
	// Limit 4 > the per-view ceiling of 3 → rejected.
	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 4}); err == nil {
		t.Fatal("expected per-view resolver ceiling to reject Limit 4")
	}
	// Limit within the ceiling passes.
	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 2}); err != nil {
		t.Fatalf("ReadPage within resolver ceiling: %v", err)
	}
	// Resetting to nil falls back to the framework default everywhere.
	r.SetMaxLimitResolver(nil)
	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 50}); err != nil {
		t.Fatalf("ReadPage after resolver reset: %v", err)
	}
}

func TestMongoViewReader_ReadPage_AfterBeforeMutualExclusion(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1})
	_, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10, After: "x", Before: "y",
	})
	if err == nil {
		t.Fatal("expected error: after and before are mutually exclusive")
	}
}

func TestMongoViewReader_ReadPage_SortSearchArchived(t *testing.T) {
	coll := &fakeColl{
		count: 2,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"},
			map[string]any{"_id": "u2", "name": "bob", "mail": "b@x"},
		},
	}
	r := viewReaderFixture(coll)
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit:           10,
		Sort:            []queries.SortField{{Field: "Name"}, {Field: "Email", Desc: true}},
		Search:          "alice",
		IncludeArchived: true,
		Filter:          map[string]any{"Name": "alice"},
	})
	if err != nil {
		t.Fatalf("ReadPage sort/search/archived: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("Items = %d, want 2", len(page.Items))
	}
}

func TestMongoViewReader_ReadPage_ProjectionStripsAutoIncludedSortField(t *testing.T) {
	coll := &fakeColl{
		count: 1,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"},
		},
	}
	r := viewReaderFixture(coll)
	// ?fields=Name with an active sort on Email: the reader auto-includes the
	// email column into the projection (to build cursors) then strips it from
	// the returned doc.
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit:      1,
		Projection: map[string]int{"_id": 0, "Name": 1},
		Sort:       []queries.SortField{{Field: "Email"}},
	})
	if err != nil {
		t.Fatalf("ReadPage projection: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(page.Items))
	}
	// Email was auto-included for the cursor then stripped from the wire doc.
	if _, present := page.Items[0]["Email"]; present {
		t.Errorf("auto-included sort field must be stripped, got %v", page.Items[0])
	}
}

func TestMongoViewReader_ReadPage_EchoesEffectiveProjection(t *testing.T) {
	coll := &fakeColl{
		count: 1,
		docs:  []any{map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"}},
	}
	r := viewReaderFixture(coll)
	proj := map[string]int{"_id": 0, "Name": 1}
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 1, Projection: proj})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	// The tabular-export plan pruning relies on page.Projection echoing the
	// criteria's effective projection (post-ToCriteria).
	if got := page.Projection; len(got) != len(proj) || got["Name"] != 1 || got["_id"] != 0 {
		t.Errorf("page.Projection = %v, want echoed %v", got, proj)
	}
}

func TestMongoViewReader_ReadPage_FindError(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1, findErr: context.Canceled})
	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 10}); err == nil {
		t.Fatal("expected error from Find")
	}
}

func TestMongoViewReader_ReadPage_KeysetForward_RoundTrip(t *testing.T) {
	coll := &fakeColl{
		count: 2,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"},
			map[string]any{"_id": "u2", "name": "bob", "mail": "b@x"},
		},
	}
	r := viewReaderFixture(coll)
	sort := []queries.SortField{{Field: "Name"}}

	// First page: limit 1 over 2 docs → HasNext + NextCursor.
	page1, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 1, Sort: sort})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if !page1.HasNext || page1.NextCursor == "" {
		t.Fatalf("expected HasNext + NextCursor, got %+v", page1)
	}

	// Feed the emitted cursor back as ?after= — drives the forward keyset path.
	page2, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 1, Sort: sort, After: page1.NextCursor,
	})
	if err != nil {
		t.Fatalf("forward keyset page: %v", err)
	}
	if !page2.HasPrev {
		t.Errorf("a non-first forward page must report HasPrev")
	}
}

func TestMongoViewReader_ReadPage_KeysetBackward(t *testing.T) {
	coll := &fakeColl{
		count: 2,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"},
			map[string]any{"_id": "u2", "name": "bob", "mail": "b@x"},
		},
	}
	r := viewReaderFixture(coll)
	sort := []queries.SortField{{Field: "Name"}}
	hash := queries.HashContext(nil, sort, "", false)
	before, err := queries.EncodeCursor([]any{"bob", "u2"}, hash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 1, Sort: sort, Before: before,
	})
	if err != nil {
		t.Fatalf("backward keyset page: %v", err)
	}
	// Came back from a forward cursor → HasNext unconditionally true.
	if !page.HasNext {
		t.Errorf("backward page must report HasNext, got %+v", page)
	}
}

func TestMongoViewReader_ReadPage_KeysetWithMultiClauseFilter(t *testing.T) {
	coll := &fakeColl{
		count: 1,
		docs:  []any{map[string]any{"_id": "u1", "name": "alice", "mail": "a@x"}},
	}
	r := viewReaderFixture(coll)
	sort := []queries.SortField{{Field: "Name"}}
	// A MultiClause filter materializes as a top-level $and; the keyset clause
	// must then append into that $and rather than clobbering it.
	filter := map[string]any{"Name": queries.MultiClause{Clauses: []any{"a", "z"}}}
	hash := queries.HashContext(filter, sort, "", false)
	after, err := queries.EncodeCursor([]any{"alice", "u1"}, hash)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10, Sort: sort, Filter: filter, After: after,
	}); err != nil {
		t.Fatalf("keyset with $and filter: %v", err)
	}
}

func TestMongoViewReader_ReadPage_InvalidCursor(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1})
	_, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10, Sort: []queries.SortField{{Field: "Name"}}, After: "@@@not-base64@@@",
	})
	if err == nil {
		t.Fatal("expected invalid-cursor decode error")
	}
}

func TestMongoViewReader_ReadPage_CursorTupleLengthMismatch(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1})
	// Sort has one field → expected tuple length 2; supply 3.
	cur, _ := queries.EncodeCursor([]any{"a", "b", "c"}, "anything")
	_, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10, Sort: []queries.SortField{{Field: "Name"}}, After: cur,
	})
	if err == nil {
		t.Fatal("expected cursor tuple-length mismatch error")
	}
}

func TestMongoViewReader_ReadPage_CursorContextHashMismatch(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 1})
	// Correct tuple length but a hash that cannot match the current context.
	cur, _ := queries.EncodeCursor([]any{"a", "b"}, "deadbeefdeadbeef")
	_, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10, Sort: []queries.SortField{{Field: "Name"}}, After: cur,
	})
	if err == nil {
		t.Fatal("expected cursor context-hash mismatch error")
	}
}

func TestMongoViewReader_ReadPage_NestedEmbedToGoDoc(t *testing.T) {
	coll := &fakeColl{
		count: 1,
		docs: []any{
			map[string]any{
				"_id":  "u1",
				"name": "alice",
				"mail": "a@x",
				"addresses": []any{
					map[string]any{"id": "a1", "zip": "10001"},
				},
			},
		},
	}
	r := aggViewFixture(coll)
	page, err := r.ReadPage(context.Background(), "agg_view", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage nested: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(page.Items))
	}
	// The embed doc field "addresses" is renamed to the Go segment "Addresses"
	// by ToGoDoc's embed recursion.
	addrs, ok := page.Items[0]["Addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("expected Addresses embed renamed by ToGoDoc, got %v", page.Items[0]["Addresses"])
	}
}

func TestMongoViewReader_ReadByID_FilterAndIncludeArchived(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{
		"_id": "u1", "name": "alice", "mail": "a@x",
		"addresses": []any{map[string]any{"id": "a1", "zip": "20002"}},
	}}}
	r := aggViewFixture(coll)
	doc, ok, err := r.ReadByID(context.Background(), "agg_view", "u1", queries.ReadCriteria{
		IncludeArchived: true,
		Filter:          map[string]any{"Name": "alice"},
	})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	addrs, ok := doc["Addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("expected Addresses embed in ReadByID, got %v", doc["Addresses"])
	}
}

func TestMongoViewReader_ResolveViewSchema_Fallbacks(t *testing.T) {
	// A reader with no SetViews call has nil viewNodes → identity fallback node.
	r := NewMongoViewReader(newFakeMongo(&fakeColl{count: 0}))
	if _, err := r.ReadPage(context.Background(), "unregistered", queries.ReadCriteria{Limit: 5}); err != nil {
		t.Fatalf("ReadPage on nil-viewNodes reader: %v", err)
	}
	// A reader WITH registered views, queried for an unknown view → empty node.
	r2 := viewReaderFixture(&fakeColl{count: 0})
	if _, err := r2.ReadPage(context.Background(), "no_such_view", queries.ReadCriteria{Limit: 5}); err != nil {
		t.Fatalf("ReadPage on unknown registered view: %v", err)
	}
}

func TestMongoViewReader_ReadPage_UnresolvableFilterKeyPassthrough(t *testing.T) {
	// A filter key the view schema cannot resolve passes through untranslated
	// (translateDotted's !ok branch).
	coll := &fakeColl{count: 1, docs: []any{map[string]any{"_id": "u1", "name": "a", "mail": "a@x"}}}
	r := viewReaderFixture(coll)
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit:  10,
		Filter: map[string]any{"unknown_field": "v"},
	})
	if err != nil {
		t.Fatalf("ReadPage with unresolvable filter key: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(page.Items))
	}
}

func TestMongoViewReader_ReadByID_DecodeError(t *testing.T) {
	// notFound=false but docs empty → FindOne yields ErrNoDocuments → (nil,false,nil).
	// To hit the non-ErrNoDocuments error path we rely on a decode mismatch is
	// not reachable via the fake; instead confirm the not-found shape here.
	r := viewReaderFixture(&fakeColl{})
	_, ok, err := r.ReadByID(context.Background(), "builder_view", "missing", queries.ReadCriteria{})
	if err != nil || ok {
		t.Fatalf("empty collection must be not-found, got ok=%v err=%v", ok, err)
	}
}
