package mongo

import (
	"context"
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The document-CRUD half of the adapter, driven through the collFn seam: what
// these pin is the FILTER/UPDATE SHAPES the framework emits — the part a live
// server cannot tell you it received wrongly (a too-loose guard filter still
// "works", it just deletes the wrong incarnation).

var errMongoFake = errors.New("fake mongo error")

// pcoll: the zero PhysicalCollection is enough — newFakeMongo routes every
// name to the same fake, and the name is only diagnostics here.
func pcoll(_ string) query.PhysicalCollection { return query.PhysicalCollection{} }

func TestApplyProjection_EmitsPipelineWithUpsertFlag(t *testing.T) {
	coll := &fakeColl{}
	m := newFakeMongo(coll)
	stages := []query.Document{{"$set": bson.M{"name": "x"}}}
	if err := m.ApplyProjection(context.Background(), pcoll("users"), "u1", stages, true); err != nil {
		t.Fatalf("ApplyProjection: %v", err)
	}
	if len(coll.pipelines) != 1 || len(coll.pipelines[0]) != 1 {
		t.Fatalf("the update must be an aggregation PIPELINE (bson.A), got %v", coll.pipelines)
	}
	coll.updateErr = errMongoFake
	if err := m.ApplyProjection(context.Background(), pcoll("users"), "u1", stages, false); err == nil {
		t.Fatal("the driver error must surface — the retry loop depends on it")
	}
}

func TestDeleteGuarded_FilterShapes(t *testing.T) {
	coll := &fakeColl{}
	m := newFakeMongo(coll)

	// Without an incarnation stamp: the watermark guard alone.
	if err := m.DeleteGuarded(context.Background(), pcoll("users"), "u1", 7, 0); err != nil {
		t.Fatalf("DeleteGuarded: %v", err)
	}
	f := coll.deletes[0].(bson.M)
	if f["_id"] != "u1" {
		t.Fatalf("filter must key by _id, got %v", f)
	}
	or := f["$or"].(bson.A)
	if len(or) != 2 {
		t.Fatalf("the guard must accept rev<=N OR no watermark, got %v", or)
	}
	if _, hasAnd := f["$and"]; hasAnd {
		t.Fatal("without a birth stamp there must be no incarnation clause")
	}

	// With the incarnation stamp: the two-second window + the zombie clause.
	if err := m.DeleteGuarded(context.Background(), pcoll("users"), "u1", 7, 1_700_000_000_500); err != nil {
		t.Fatalf("DeleteGuarded: %v", err)
	}
	f2 := coll.deletes[1].(bson.M)
	and := f2["$and"].(bson.A)
	inner := and[0].(bson.M)["$or"].(bson.A)
	if len(inner) != 2 {
		t.Fatalf("the incarnation clause must accept the window OR an absent created_at (the zombie), got %v", inner)
	}
	win := inner[0].(bson.M)["created_at"].(bson.M)
	if _, ok := win["$gte"]; !ok {
		t.Fatalf("the incarnation match must be a RANGE (dialect rounding), got %v", win)
	}

	coll.deleteErr = errMongoFake
	if err := m.DeleteGuarded(context.Background(), pcoll("users"), "u1", 7, 0); err == nil {
		t.Fatal("the driver error must surface")
	}
}

func TestBulkApplyProjection_Batching(t *testing.T) {
	coll := &fakeColl{}
	m := newFakeMongo(coll)
	if err := m.BulkApplyProjection(context.Background(), pcoll("users"), nil); err != nil {
		t.Fatalf("an empty batch must be a no-op (the driver rejects zero models): %v", err)
	}
	if coll.bulkCalls != 0 {
		t.Fatal("empty batch must not reach the driver")
	}
	items := []query.IdentifiedStages{
		{ID: "a", Stages: []query.Document{{"$set": bson.M{"x": 1}}}},
		{ID: "b", Stages: []query.Document{{"$set": bson.M{"x": 2}}}},
	}
	if err := m.BulkApplyProjection(context.Background(), pcoll("users"), items); err != nil {
		t.Fatalf("BulkApplyProjection: %v", err)
	}
	if coll.bulkCalls != 1 || coll.bulkModels != 2 {
		t.Fatalf("want one unordered bulk with 2 models, got calls=%d models=%d", coll.bulkCalls, coll.bulkModels)
	}
	coll.bulkErr = errMongoFake
	if err := m.BulkApplyProjection(context.Background(), pcoll("users"), items); err == nil {
		t.Fatal("a bulk error must surface — the rebuild aborts on it")
	}
}

func TestHasDocuments_Semantics(t *testing.T) {
	coll := &fakeColl{count: 1}
	m := newFakeMongo(coll)
	if got, err := m.HasDocuments(context.Background(), pcoll("users")); err != nil || !got {
		t.Fatalf("count>0 must report true, got %v %v", got, err)
	}
	coll.count = 0
	if got, _ := m.HasDocuments(context.Background(), pcoll("users")); got {
		t.Fatal("count==0 must report false")
	}
	coll.countErr = errMongoFake
	if _, err := m.HasDocuments(context.Background(), pcoll("users")); err == nil {
		t.Fatal("the count error must surface")
	}
}

// RevisionsByIDs is the parity sweep's read half; toInt64 must normalize every
// BSON numeric shape, and a watermark-less document is PRESENT at 0 — distinct
// from absent, which is what the sweep acts on.
func TestRevisionsByIDs_NormalizesWatermarks(t *testing.T) {
	coll := &fakeColl{docs: []any{
		bson.M{"_id": "a", query.DocRevisionField: int64(5)},
		bson.M{"_id": "b", query.DocRevisionField: int32(4)},
		bson.M{"_id": "c", query.DocRevisionField: float64(3)},
		bson.M{"_id": "d"}, // pre-watermark document
	}}
	m := newFakeMongo(coll)
	got, err := m.RevisionsByIDs(context.Background(), pcoll("users"), []string{"a", "b", "c", "d"})
	if err != nil {
		t.Fatalf("RevisionsByIDs: %v", err)
	}
	want := map[string]int64{"a": 5, "b": 4, "c": 3, "d": 0}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	if out, err := m.RevisionsByIDs(context.Background(), pcoll("users"), nil); err != nil || len(out) != 0 {
		t.Fatalf("empty ids must short-circuit, got %v %v", out, err)
	}
	coll.findErr = errMongoFake
	if _, err := m.RevisionsByIDs(context.Background(), pcoll("users"), []string{"a"}); err == nil {
		t.Fatal("the find error must surface — a sweep that cannot read must not report 'absent'")
	}
}
