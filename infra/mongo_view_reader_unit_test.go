package infra

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
)

// viewReaderFixture registers a single flat view backed by builderTestSchema
// and returns a reader plus the fake collection so a test can program docs.
func viewReaderFixture(coll mongoColl) *MongoViewReader {
	vd := View("builder_view").Version(1).Root("builder_test_entities").
		Schema(builderTestSchema)
	r := NewMongoViewReader(newFakeMongo(coll))
	r.SetViews([]*ViewDefinition{vd})
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
	if page.Total != 7 || !page.OnlyTotal {
		t.Errorf("Total=%d OnlyTotal=%v, want 7/true", page.Total, page.OnlyTotal)
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

	if err := m.Upsert(ctx, "c", "id1", map[string]any{"name": "x"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Errorf("expected one UpdateOne, got %d", len(coll.updates))
	}
	if err := m.Delete(ctx, "c", "id1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(coll.deletes) != 1 {
		t.Errorf("expected one DeleteOne, got %d", len(coll.deletes))
	}
	if err := m.UpdateFields(ctx, "c", "id1", map[string]any{"name": nil}); err != nil {
		t.Fatalf("UpdateFields: %v", err)
	}
	// Empty field map is a no-op (no driver call).
	if err := m.UpdateFields(ctx, "c", "id1", map[string]any{}); err != nil {
		t.Fatalf("UpdateFields empty: %v", err)
	}
}

func TestMongoDB_FindIDsByField(t *testing.T) {
	coll := &fakeColl{docs: []any{
		map[string]any{"_id": "a"},
		map[string]any{"_id": "b"},
	}}
	m := newFakeMongo(coll)

	ids, err := m.FindIDsByField(context.Background(), "c", "fk", "x")
	if err != nil {
		t.Fatalf("FindIDsByField: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v, want 2", ids)
	}
}
