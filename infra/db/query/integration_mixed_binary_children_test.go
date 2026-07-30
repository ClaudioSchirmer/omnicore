//go:build integration

package query

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The mixed-binary rolling-deploy window, at CHILD-ELEMENT grain, evaluated by
// a REAL MongoDB through the PRODUCTION builders (decodePayloadEvent →
// buildProjectionStages / consultGuardedStages) — no hand-copied stage shapes.
//
// The scalar half of this window is closed (guardedSetStage applies its full
// carried state at the equal revision) and covered by the rebuild_scale QA
// oracle. This test asks the SAME question one level down: a previous-binary
// pod's consult repair composes the document at the row's CURRENT revision
// through its own (older) schema — its child elements lack the columns the new
// binary added — and the new binary's event for that same revision then
// arrives carrying the only copy of the child column's value in its child op.
//
// Sequence (the exact interleave the rebuild_scale Phase 3.5 forces end to
// end, here in isolation):
//  1. V2 insert event rev1 — child c1 born with the new column null;
//  2. V1 consult repair at row revision 2 — composed WITHOUT the new column
//     keys (root scalar and child element alike): watermark advances to 2,
//     the child array is replaced by the schema-blind composition;
//  3. V2 update event rev2 — root scalar nick + child column rank carry the
//     values.
//
// Contract under test: after step 3 the document holds BOTH values. The root
// scalar (`nick`) is the CONTROL — the fixed path. The child column (`rank`)
// is the question: ownGuardedChildStages wraps child edits in a
// strictly-newer guard with no equal-revision arm, so a loss here is the same
// defect class the scalar fix closed, at element grain.
func TestIntegration_MixedBinaryWindow_OwnChildEqualRevision(t *testing.T) {
	uri := os.Getenv("OMNICORE_TEST_MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27018"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Skipf("skipping: cannot build Mongo client for %s (%v)", uri, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("skipping: cannot reach Mongo at %s (%v)", uri, err)
	}
	db := client.Database("omnicore_it_mixed_binary")
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	}()
	coll := db.Collection("mb_docs")

	// V2 schema — Nick (root) and Rank (child) are the columns "added by the
	// new binary". The V1 side needs no schema of its own: its blindness is
	// represented by the consult DOCUMENT lacking those keys, which is all
	// consultGuardedStages ever sees of the composer.
	child := core.NewTableSchema[*pdChild]("mb_children").ID("id").ParentID("root_id").
		Field("Label", "label").Field("Rank", "rank")
	root := core.NewTableSchema[*pdRoot]("mb_roots").ID("id").Revision("revision").
		Field("Name", "name").Field("Nick", "nick").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(child)
	view := View("mb_view").Version(2).Schema(root)
	seg := childDocSegment(child)

	apply := func(label string, stages []Document) {
		t.Helper()
		pipeline := make(bson.A, 0, len(stages))
		for _, st := range stages {
			pipeline = append(pipeline, bson.M(st))
		}
		if _, err := coll.UpdateOne(ctx, bson.M{"_id": "r1"}, pipeline,
			options.UpdateOne().SetUpsert(true)); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	// 1. V2 insert, revision 1 — the child is born, new columns null.
	ev1, ok := decodePayloadEvent(root, []byte(`{
		"name": "r", "nick": null,
		"_ids": {"id": "r1", "revision": 1},
		"_children": {"pdChild": [
			{"_op": "insert", "id": "c1", "label": "a", "rank": null}
		]}
	}`))
	if !ok {
		t.Fatal("insert payload must decode")
	}
	apply("insert rev1", buildProjectionStages(root, ev1))

	// 2. V1 consult repair composed at row revision 2 — the previous binary's
	// composition: no `nick` key, child elements without `rank`. Its newer-
	// than-the-document write advances the watermark and replaces the child
	// array with the schema-blind shape (exactly what a stale/racing base
	// stamp triggers on the old pod mid-rollout).
	consultDoc := Document{
		"id": "r1", "name": "r",
		seg:              []any{Document{"id": "c1", "label": "a"}},
		docRevisionField: int64(2),
	}
	apply("v1 consult at rev2", consultGuardedStages(view, consultDoc))

	// 3. The V2 update event for that same revision 2 — the only carrier of
	// both values.
	ev2, ok := decodePayloadEvent(root, []byte(`{
		"name": "r", "nick": "nick-value",
		"_ids": {"id": "r1", "revision": 2},
		"_children": {"pdChild": [
			{"_op": "update", "id": "c1", "label": "a", "rank": 7}
		]}
	}`))
	if !ok {
		t.Fatal("update payload must decode")
	}
	apply("update rev2", buildProjectionStages(root, ev2))

	var doc bson.M
	if err := coll.FindOne(ctx, bson.M{"_id": "r1"}).Decode(&doc); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if rev, _ := doc[docRevisionField].(int64); rev != 2 {
		t.Fatalf("the document must sit at revision 2 after the window, got %v", doc[docRevisionField])
	}
	// CONTROL — the closed scalar path: the equal-revision payload re-assert
	// must have landed the root value the schema-blind consult could not carry.
	if doc["nick"] != "nick-value" {
		t.Errorf("control failed: the ROOT scalar value must survive the window (the fixed path), got %v", doc["nick"])
	}
	// THE QUESTION — the child column at element grain. (Driver v2 decodes a
	// nested document as bson.D by default — normalize before the lookup.)
	kids, _ := doc[seg].(bson.A)
	if len(kids) != 1 {
		t.Fatalf("expected exactly one child element, got %v", doc[seg])
	}
	elem := map[string]any{}
	switch e := kids[0].(type) {
	case bson.M:
		elem = e
	case bson.D:
		for _, kv := range e {
			elem[kv.Key] = kv.Value
		}
	default:
		t.Fatalf("unexpected child element shape %T: %v", kids[0], kids[0])
	}
	if got, present := elem["rank"]; !present || got == nil {
		t.Errorf("own-child column VALUE lost in the mixed-binary window: element %v has no non-null 'rank' — "+
			"the child edit was discarded at the equal revision (strictly-newer guard, no equal arm)", elem)
	} else if n, _ := got.(int64); n != 7 {
		t.Errorf("own-child column must carry the rev-2 value 7, got %v", got)
	}
}

// The SAME window exactly as the rebuild_scale e2e drives it: the CONSUMER is
// the PREVIOUS binary (its schemas do not declare the new child column), and
// the rev-2 write REPLACES the child collection (the id-less wire), so the
// event's ops are archive(old element) + insert(new element carrying the
// value). The V1 consumer's decode must pass the unknown child column through
// and its equal-revision child stages must land it.
func TestIntegration_MixedBinaryWindow_V1ConsumerChildReplace(t *testing.T) {
	uri := os.Getenv("OMNICORE_TEST_MONGO_URI")
	if uri == "" {
		uri = "mongodb://localhost:27018"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Skipf("skipping: cannot build Mongo client for %s (%v)", uri, err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		t.Skipf("skipping: cannot reach Mongo at %s (%v)", uri, err)
	}
	db := client.Database("omnicore_it_mixed_binary_v1")
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	}()
	coll := db.Collection("mb_docs_v1")

	// The CONSUMER's schemas — the V1 binary: NO Rank on the child, NO Nick on
	// the root. Everything the consumer does (decode, stage build, consult
	// shapes) runs through THESE, exactly like POD B.
	childV1 := core.NewTableSchema[*pdChild]("mb1_children").ID("id").ParentID("root_id").
		Field("Label", "label")
	rootV1 := core.NewTableSchema[*pdRoot]("mb1_roots").ID("id").Revision("revision").
		Field("Name", "name").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(childV1)
	viewV1 := View("mb1_view").Version(1).Schema(rootV1)
	seg := childDocSegment(childV1)

	apply := func(label string, stages []Document) {
		t.Helper()
		pipeline := make(bson.A, 0, len(stages))
		for _, st := range stages {
			pipeline = append(pipeline, bson.M(st))
		}
		if _, err := coll.UpdateOne(ctx, bson.M{"_id": "r1"}, pipeline,
			options.UpdateOne().SetUpsert(true)); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}

	// 1. V2-produced insert (rev1) decoded BY THE V1 CONSUMER — child c1 born,
	// the unknown `rank` passes through as null.
	ev1, ok := decodePayloadEvent(rootV1, []byte(`{
		"name": "r", "nick": null,
		"_ids": {"id": "r1", "revision": 1},
		"_children": {"pdChild": [
			{"_op": "insert", "id": "c1", "label": "a", "rank": null}
		]}
	}`))
	if !ok {
		t.Fatal("insert payload must decode")
	}
	apply("insert rev1 (v1 consumer)", buildProjectionStages(rootV1, ev1))

	// 2. The V1 consult repair at row revision 2 — the row already carries the
	// rev-2 state (old child archived, NEW child c2 present), composed through
	// the V1 columns: no rank anywhere.
	consultDoc := Document{
		"id": "r1", "name": "r",
		seg: []any{
			Document{"id": "c1", "label": "a", "deleted_at": "2026-07-23T14:12:32Z"},
			Document{"id": "c2", "label": "a"},
		},
		docRevisionField: int64(2),
	}
	apply("v1 consult at rev2", consultGuardedStages(viewV1, consultDoc))

	// 3. The V2-produced update (rev2) decoded BY THE V1 CONSUMER — the
	// replace: archive(c1) + insert(c2 with rank), plus the root nick value.
	ev2, ok := decodePayloadEvent(rootV1, []byte(`{
		"name": "r", "nick": "nick-value",
		"_ids": {"id": "r1", "revision": 2},
		"_children": {"pdChild": [
			{"_op": "archive", "id": "c1", "deleted_at": "2026-07-23T14:12:32Z"},
			{"_op": "insert", "id": "c2", "label": "a", "rank": 7}
		]}
	}`))
	if !ok {
		t.Fatal("update payload must decode")
	}
	apply("update rev2 (v1 consumer)", buildProjectionStages(rootV1, ev2))

	var doc bson.M
	if err := coll.FindOne(ctx, bson.M{"_id": "r1"}).Decode(&doc); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if doc["nick"] != "nick-value" {
		t.Errorf("control failed: the ROOT scalar must pass through the V1 consumer, got %v", doc["nick"])
	}
	kids, _ := doc[seg].(bson.A)
	var c2 map[string]any
	for _, k := range kids {
		elem := map[string]any{}
		switch e := k.(type) {
		case bson.M:
			elem = e
		case bson.D:
			for _, kv := range e {
				elem[kv.Key] = kv.Value
			}
		}
		if elem["id"] == "c2" {
			c2 = elem
		}
	}
	if c2 == nil {
		t.Fatalf("the replaced child element c2 must exist, got %v", doc[seg])
	}
	if got, present := c2["rank"]; !present || got == nil {
		t.Errorf("own-child column VALUE lost on the V1-consumer replace: element %v has no non-null 'rank'", c2)
	} else if n, _ := got.(int64); n != 7 {
		t.Errorf("own-child column must carry the rev-2 value 7, got %v", got)
	}
}
