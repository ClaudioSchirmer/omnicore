package mongo

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// This file extends the MongoDB CRUD helper coverage in
// mongo_view_reader_unit_test.go: FindManyByField (happy + find error), the
// driver error branches of Upsert / Delete / UpdateFields, and the
// FindIDsByField non-string _id fallback + empty/missing-_id skip.

func TestMongoDB_FindManyByField_Happy(t *testing.T) {
	coll := &fakeColl{docs: []any{
		map[string]any{"_id": "a", "fk": "x"},
		map[string]any{"_id": "b", "fk": "x"},
	}}
	m := newFakeMongo(coll)
	out, err := m.FindManyByField(context.Background(), "c", "fk", "x")
	if err != nil {
		t.Fatalf("FindManyByField: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("FindManyByField returned %d docs, want 2", len(out))
	}
}

func TestMongoDB_FindManyByField_FindError(t *testing.T) {
	m := newFakeMongo(&fakeColl{findErr: context.Canceled})
	if _, err := m.FindManyByField(context.Background(), "c", "fk", "x"); err == nil {
		t.Fatal("expected Find error to surface")
	}
}

func TestMongoDB_Upsert_Error(t *testing.T) {
	m := newFakeMongo(&fakeColl{updateErr: context.Canceled})
	if err := m.Upsert(context.Background(), "c", "id1", bson.M{"name": "x"}); err == nil {
		t.Fatal("expected Upsert UpdateOne error")
	}
}

func TestMongoDB_BulkUpsert_Batches(t *testing.T) {
	coll := &fakeColl{}
	m := newFakeMongo(coll)
	docs := []query.IdentifiedDocument{
		{ID: "a", Doc: bson.M{"name": "x"}},
		{ID: "b", Doc: bson.M{"name": "y"}},
	}
	if err := m.BulkUpsert(context.Background(), "c", docs); err != nil {
		t.Fatalf("BulkUpsert: %v", err)
	}
	if coll.bulkCalls != 1 {
		t.Errorf("expected 1 BulkWrite call, got %d", coll.bulkCalls)
	}
	if coll.bulkModels != 2 {
		t.Errorf("expected 2 write models, got %d", coll.bulkModels)
	}
}

func TestMongoDB_BulkUpsert_EmptyIsNoop(t *testing.T) {
	coll := &fakeColl{bulkErr: context.Canceled} // would surface if BulkWrite ran
	m := newFakeMongo(coll)
	if err := m.BulkUpsert(context.Background(), "c", nil); err != nil {
		t.Fatalf("empty BulkUpsert must be a no-op, got %v", err)
	}
	if coll.bulkCalls != 0 {
		t.Errorf("empty batch must not call BulkWrite, got %d calls", coll.bulkCalls)
	}
}

func TestMongoDB_BulkUpsert_Error(t *testing.T) {
	m := newFakeMongo(&fakeColl{bulkErr: context.Canceled})
	docs := []query.IdentifiedDocument{{ID: "a", Doc: bson.M{"name": "x"}}}
	if err := m.BulkUpsert(context.Background(), "c", docs); err == nil {
		t.Fatal("expected BulkWrite error to surface")
	}
}

func TestMongoDB_Delete_Error(t *testing.T) {
	m := newFakeMongo(&fakeColl{deleteErr: context.Canceled})
	if err := m.Delete(context.Background(), "c", "id1"); err == nil {
		t.Fatal("expected Delete DeleteOne error")
	}
}

func TestMongoDB_UpdateFields_Error(t *testing.T) {
	m := newFakeMongo(&fakeColl{updateErr: context.Canceled})
	if err := m.UpdateFields(context.Background(), "c", "id1", bson.M{"name": nil}); err == nil {
		t.Fatal("expected UpdateFields UpdateOne error")
	}
}

func TestMongoDB_FindIDsByField_NonStringAndEmpty(t *testing.T) {
	// One string _id, one numeric _id (fmt.Sprintf fallback), one missing _id
	// (skipped), one nil _id (skipped).
	coll := &fakeColl{docs: []any{
		map[string]any{"_id": "s1"},
		map[string]any{"_id": int32(42)},
		map[string]any{"other": "no-id"},
		map[string]any{"_id": nil},
	}}
	m := newFakeMongo(coll)
	ids, err := m.FindIDsByField(context.Background(), "c", "fk", "x")
	if err != nil {
		t.Fatalf("FindIDsByField: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 (string + numeric fallback)", ids)
	}
	if ids[0] != "s1" || ids[1] != "42" {
		t.Errorf("ids = %v, want [s1 42]", ids)
	}
}

func TestMongoDB_FindIDsByField_FindError(t *testing.T) {
	m := newFakeMongo(&fakeColl{findErr: context.Canceled})
	if _, err := m.FindIDsByField(context.Background(), "c", "fk", "x"); err == nil {
		t.Fatal("expected Find error to surface")
	}
}
