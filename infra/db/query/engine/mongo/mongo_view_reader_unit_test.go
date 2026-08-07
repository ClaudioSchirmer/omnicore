package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// viewReaderFixture registers a single flat view backed by builderTestSchema
// and returns a reader plus the fake collection so a test can program docs.
func viewReaderFixture(coll mongoColl) *MongoViewReader {
	vd := query.View("builder_view").Version(1).
		Schema(builderTestSchema)
	r := NewMongoViewReader(newFakeMongo(coll), testResolver)
	r.SetViews([]*query.ViewDefinition{vd})
	return r
}

func TestMongoViewReader_ReadByID_Found(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "u1", "name": "alice", "mail": "a@x.com"}}}
	r := viewReaderFixture(coll)

	doc, ok, err := r.ReadByID(context.Background(), "builder_view", "u1", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if !ok {
		t.Fatal("expected found")
	}
	if doc["_id"] != "u1" {
		t.Errorf("doc id missing: %v", doc)
	}
	if doc["Name"] != "alice" {
		t.Errorf("column not mapped to Go field: %v", doc)
	}
}

func TestMongoViewReader_ReadByID_NotFound(t *testing.T) {
	r := viewReaderFixture(&fakeColl{notFound: true})

	_, ok, err := r.ReadByID(context.Background(), "builder_view", "missing", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if ok {
		t.Fatal("expected not found")
	}
}

func TestMongoViewReader_ReadPage_Basic(t *testing.T) {
	coll := &fakeColl{
		count: 2,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x.com"},
			map[string]any{"_id": "u2", "name": "bob", "mail": "b@x.com"},
		},
	}
	r := viewReaderFixture(coll)

	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("Items = %d, want 2", len(page.Items))
	}
}

func TestMongoViewReader_ReadPage_OnlyTotal(t *testing.T) {
	r := viewReaderFixture(&fakeColl{count: 7})

	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{OnlyTotal: true})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if page.TotalCount != 7 || !page.OnlyTotal {
		t.Errorf("Total=%d OnlyTotal=%v, want 7/true", page.TotalCount, page.OnlyTotal)
	}
}

func TestMongoViewReader_ReadPage_CountError(t *testing.T) {
	r := viewReaderFixture(&fakeColl{countErr: context.Canceled})

	if _, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 10}); err == nil {
		t.Fatal("expected error from CountDocuments")
	}
}

// MongoDB CRUD helpers exercised against the fake collection.

func TestMongoDB_Upsert_Delete_UpdateFields(t *testing.T) {
	coll := &fakeColl{}
	m := newFakeMongo(coll)
	ctx := context.Background()

	if err := m.Upsert(ctx, pc("c"), "id1", map[string]any{"name": "x"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Errorf("expected one UpdateOne, got %d", len(coll.updates))
	}
	if err := m.Delete(ctx, pc("c"), "id1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(coll.deletes) != 1 {
		t.Errorf("expected one DeleteOne, got %d", len(coll.deletes))
	}
	if err := m.UpdateFields(ctx, pc("c"), "id1", map[string]any{"name": nil}); err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
	// Empty field map is a no-op (no driver call).
	if err := m.UpdateFields(ctx, pc("c"), "id1", map[string]any{}); err != nil {
		t.Fatalf("UpdateFields empty: %v", err)
	}
}

func TestMongoDB_FindIDsByField(t *testing.T) {
	coll := &fakeColl{docs: []any{
		map[string]any{"_id": "a"},
		map[string]any{"_id": "b"},
	}}
	m := newFakeMongo(coll)

	ids, err := m.FindIDsByField(context.Background(), pc("c"), "fk", "x")
	if err != nil {
		t.Fatalf("FindIDsByField: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v, want 2", ids)
	}
}

func TestMongoViewReader_ReadPage_ItemCursorsAlignedAndDecodable(t *testing.T) {
	coll := &fakeColl{
		count: 2,
		docs: []any{
			map[string]any{"_id": "u1", "name": "alice", "mail": "a@x.com"},
			map[string]any{"_id": "u2", "name": "bob", "mail": "b@x.com"},
		},
	}
	r := viewReaderFixture(coll)

	// No sort → keyset tuple is [_id]; context hash is empty.
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{Limit: 10})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.ItemCursors) != len(page.Items) {
		t.Fatalf("ItemCursors len=%d, Items len=%d — must align", len(page.ItemCursors), len(page.Items))
	}
	wantIDs := []string{"u1", "u2"}
	for i, cur := range page.ItemCursors {
		dec, derr := queries.DecodeCursor(cur)
		if derr != nil {
			t.Fatalf("ItemCursors[%d] did not decode: %v", i, derr)
		}
		if len(dec.K) != 1 {
			t.Fatalf("no-sort tuple must be [_id], got %v", dec.K)
		}
		if dec.K[0] != wantIDs[i] {
			t.Errorf("ItemCursors[%d] _id = %v, want %s", i, dec.K[0], wantIDs[i])
		}
		if dec.H != "" {
			t.Errorf("empty context must hash to \"\", got %q", dec.H)
		}
	}
	// Edge cursors are the first / last item cursors.
	if page.StartCursor != "" && page.StartCursor != page.ItemCursors[0] {
		t.Errorf("PrevCursor must equal the first item cursor")
	}
}

func TestMongoViewReader_ReadPage_ItemCursorsCarrySortValue(t *testing.T) {
	coll := &fakeColl{
		count: 1,
		docs:  []any{map[string]any{"_id": "u1", "name": "alice", "mail": "a@x.com"}},
	}
	r := viewReaderFixture(coll)

	// Sort by Name → tuple is [name, _id]; context hash is non-empty.
	page, err := r.ReadPage(context.Background(), "builder_view", queries.ReadCriteria{
		Limit: 10,
		OrderBy:  []queries.OrderByField{{Field: "Name"}},
	})
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if len(page.ItemCursors) != 1 {
		t.Fatalf("expected 1 item cursor, got %d", len(page.ItemCursors))
	}
	dec, derr := queries.DecodeCursor(page.ItemCursors[0])
	if derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	if len(dec.K) != 2 || dec.K[0] != "alice" || dec.K[1] != "u1" {
		t.Errorf("tuple = %v, want [alice u1]", dec.K)
	}
	if dec.H == "" {
		t.Error("a sorted context must carry a non-empty hash")
	}
}
