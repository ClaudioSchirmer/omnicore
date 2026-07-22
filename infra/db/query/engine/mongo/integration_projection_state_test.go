//go:build integration && postgres

package mongo

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	query "github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The projection-convergence primitives evaluated by a REAL MongoDB — the
// unit fakes only record pipeline shapes; these prove the server-side
// semantics the mechanisms depend on: the guarded delete's revision predicate,
// the bulk pipeline upsert, the equal-revision fill expressions
// ($type/$mergeObjects/$map/$filter over $literal) and the registry's TTL
// provisioning.

func TestIntegration_DeleteGuarded_RevisionPredicate(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()
	coll := pc("it_delete_guarded")
	defer func() { _ = m.DropCollection(ctx, coll) }()

	seed := func(id string, doc query.Document) {
		if err := m.Upsert(ctx, coll, id, doc); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	exists := func(id string) bool {
		docs, err := m.FindManyByField(ctx, coll, "_id", id)
		if err != nil {
			t.Fatalf("find %s: %v", id, err)
		}
		return len(docs) == 1
	}

	// Older-or-equal watermark → deleted.
	seed("old", query.Document{"_revision": int64(3)})
	if err := m.DeleteGuarded(ctx, coll, "old", 3, 0); err != nil {
		t.Fatalf("delete old: %v", err)
	}
	if exists("old") {
		t.Error("a document at the delete's revision must be removed")
	}
	// Newer watermark → survives (a fresher writer re-materialized it).
	seed("new", query.Document{"_revision": int64(7)})
	if err := m.DeleteGuarded(ctx, coll, "new", 3, 0); err != nil {
		t.Fatalf("delete new: %v", err)
	}
	if !exists("new") {
		t.Error("a document past the delete's revision must survive the guarded delete")
	}
	// Watermark-less document counts as older → deleted.
	seed("bare", query.Document{"name": "x"})
	if err := m.DeleteGuarded(ctx, coll, "bare", 3, 0); err != nil {
		t.Fatalf("delete bare: %v", err)
	}
	if exists("bare") {
		t.Error("a watermark-less document must count as older and be removed")
	}
	// Missing target → no error (Delete parity).
	if err := m.DeleteGuarded(ctx, coll, "ghost", 3, 0); err != nil {
		t.Errorf("a missing target must not error: %v", err)
	}
}

// The incarnation scope: a DETERMINISTIC id reborn under the same natural key
// restarts its revision — the tombstone's created_at keeps the old life's
// guarded delete away from the new life's document.
func TestIntegration_DeleteGuarded_CreatedAtScopesIncarnation(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()
	coll := pc("it_delete_guarded_rebirth")
	defer func() { _ = m.DropCollection(ctx, coll) }()

	oldLife := time.Date(2026, 7, 20, 10, 0, 0, 123_000_000, time.UTC)
	newLife := time.Date(2026, 7, 22, 16, 24, 13, 456_000_000, time.UTC)

	// The REBORN document: same _id, revision restarted, NEW created_at.
	if err := m.Upsert(ctx, coll, "d1", query.Document{
		"_revision": int64(1), "created_at": newLife, "name": "reborn",
	}); err != nil {
		t.Fatalf("seed reborn: %v", err)
	}
	// The old life's tombstone delete (rev 7 >= 1) must NOT kill it: the
	// created_at scope misses.
	if err := m.DeleteGuarded(ctx, coll, "d1", 7, oldLife.UnixMilli()); err != nil {
		t.Fatalf("delete old-life: %v", err)
	}
	docs, _ := m.FindManyByField(ctx, coll, "_id", "d1")
	if len(docs) != 1 {
		t.Fatal("the reborn document must survive the old incarnation's tombstone delete")
	}
	// A zombie of the SAME incarnation dies: created_at matches.
	if err := m.DeleteGuarded(ctx, coll, "d1", 7, newLife.UnixMilli()); err != nil {
		t.Fatalf("delete same-life: %v", err)
	}
	docs, _ = m.FindManyByField(ctx, coll, "_id", "d1")
	if len(docs) != 0 {
		t.Error("a document of the SAME incarnation (created_at matches) must die under the guarded delete")
	}

	// A zombie re-materialized by a redelivered UPDATED after the delete has NO
	// created_at (updates never carry it) — it must die too: only a document
	// carrying a DIFFERENT created_at (a true rebirth) escapes the tombstone.
	if err := m.Upsert(ctx, coll, "d2", query.Document{"_revision": int64(5), "name": "zombie"}); err != nil {
		t.Fatalf("seed zombie: %v", err)
	}
	if err := m.DeleteGuarded(ctx, coll, "d2", 7, oldLife.UnixMilli()); err != nil {
		t.Fatalf("delete zombie: %v", err)
	}
	docs, _ = m.FindManyByField(ctx, coll, "_id", "d2")
	if len(docs) != 0 {
		t.Error("a created_at-less document under a tombstoned id is a zombie and must die")
	}

	// Second-grain range: the tombstone's created_at may be SECOND-truncated
	// (a MySQL DATETIME read-back) while the document carries the payload's
	// full precision — same second must still match.
	docSecond := time.Date(2026, 7, 22, 18, 37, 41, 431_000_000, time.UTC)
	truncated := time.Date(2026, 7, 22, 18, 37, 41, 0, time.UTC)
	if err := m.Upsert(ctx, coll, "d3", query.Document{
		"_revision": int64(4), "created_at": docSecond, "name": "same-second",
	}); err != nil {
		t.Fatalf("seed d3: %v", err)
	}
	if err := m.DeleteGuarded(ctx, coll, "d3", 7, truncated.UnixMilli()); err != nil {
		t.Fatalf("delete d3: %v", err)
	}
	docs, _ = m.FindManyByField(ctx, coll, "_id", "d3")
	if len(docs) != 0 {
		t.Error("a second-truncated tombstone must still kill the same second's incarnation")
	}

	// MySQL DATETIME(0) ROUNDS up: a document born at 12.731 is read back as
	// 13.000 — the range must still kill it.
	rounded := time.Date(2026, 7, 22, 18, 42, 13, 0, time.UTC)
	docBefore := time.Date(2026, 7, 22, 18, 42, 12, 731_000_000, time.UTC)
	if err := m.Upsert(ctx, coll, "d4", query.Document{
		"_revision": int64(1), "created_at": docBefore, "name": "rounded-up",
	}); err != nil {
		t.Fatalf("seed d4: %v", err)
	}
	if err := m.DeleteGuarded(ctx, coll, "d4", 7, rounded.UnixMilli()); err != nil {
		t.Fatalf("delete d4: %v", err)
	}
	docs, _ = m.FindManyByField(ctx, coll, "_id", "d4")
	if len(docs) != 0 {
		t.Error("a rounded-up tombstone (MySQL DATETIME) must still kill the just-before-the-second incarnation")
	}
}

func TestIntegration_BulkApplyProjection_GuardedBatch(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()
	coll := pc("it_bulk_apply")
	defer func() { _ = m.DropCollection(ctx, coll) }()

	guarded := func(name string, rev int64) []query.Document {
		newer := query.Document{"$lt": []any{query.Document{"$ifNull": []any{"$_revision", int64(-1)}}, rev}}
		return []query.Document{{"$set": query.Document{
			"name":      query.Document{"$cond": []any{newer, query.Document{"$literal": name}, "$name"}},
			"_revision": query.Document{"$cond": []any{newer, rev, query.Document{"$ifNull": []any{"$_revision", rev}}}},
		}}}
	}
	// Pre-existing fresher doc + a brand-new one, in one unordered batch.
	if err := m.ApplyProjection(ctx, coll, "d1", guarded("fresh", 5), true); err != nil {
		t.Fatalf("seed d1: %v", err)
	}
	err := m.BulkApplyProjection(ctx, coll, []query.IdentifiedStages{
		{ID: "d1", Stages: guarded("stale", 2)}, // must be a no-op (5 > 2)
		{ID: "d2", Stages: guarded("born", 4)},  // must upsert
	})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	d1, _ := m.FindManyByField(ctx, coll, "_id", "d1")
	if len(d1) != 1 || d1[0]["name"] != "fresh" {
		t.Errorf("the stale batch member must not regress d1, got %v", d1)
	}
	d2, _ := m.FindManyByField(ctx, coll, "_id", "d2")
	if len(d2) != 1 || d2[0]["name"] != "born" {
		t.Errorf("the new batch member must upsert d2, got %v", d2)
	}
}

// The equal-revision fill forms, evaluated by the server: a scalar the
// document lacks is filled, a present one kept; a stored child element
// shallow-merges the PK-matched composed element (stored keys win); a stored
// sub-document segment shallow-merges the composed one.
func TestIntegration_EqualRevisionFill_ServerSemantics(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()
	coll := pc("it_equal_fill")
	defer func() { _ = m.DropCollection(ctx, coll) }()

	// The document a previous-binary writer produced at revision 2: no
	// "nickname", child element without "kind", segment without "grade".
	if err := m.Upsert(ctx, coll, "d1", query.Document{
		"_revision": int64(2),
		"email":     "stored@x",
		"addresses": []any{map[string]any{"id": "a1", "city": "POA"}},
		"segment":   map[string]any{"id": "u1", "name": "Ana"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rev := int64(2)
	newer := query.Document{"$lt": []any{query.Document{"$ifNull": []any{"$_revision", int64(-1)}}, rev}}
	equal := query.Document{"$eq": []any{"$_revision", rev}}
	lit := func(v any) query.Document { return query.Document{"$literal": v} }
	fillScalar := func(col string, v any) query.Document {
		missing := query.Document{"$eq": []any{query.Document{"$type": "$" + col}, "missing"}}
		return query.Document{"$cond": []any{newer, lit(v),
			query.Document{"$cond": []any{equal,
				query.Document{"$cond": []any{missing, lit(v), "$" + col}}, "$" + col}}}}
	}
	composedAddresses := []any{map[string]any{"id": "a1", "city": "COMPOSED", "kind": "home"}}
	matched := query.Document{"$ifNull": []any{
		query.Document{"$arrayElemAt": []any{
			query.Document{"$filter": query.Document{
				"input": lit(composedAddresses), "as": "fresh",
				"cond": query.Document{"$eq": []any{"$$fresh.id", "$$stored.id"}},
			}}, 0}},
		query.Document{},
	}}
	elemMerge := query.Document{"$cond": []any{newer, lit(composedAddresses),
		query.Document{"$cond": []any{equal,
			query.Document{"$cond": []any{
				query.Document{"$eq": []any{query.Document{"$type": "$addresses"}, "array"}},
				query.Document{"$map": query.Document{
					"input": "$addresses", "as": "stored",
					"in": query.Document{"$mergeObjects": []any{matched, "$$stored"}},
				}},
				"$addresses"}},
			"$addresses"}}}}
	composedSegment := map[string]any{"id": "u1", "name": "COMPOSED", "grade": "A"}
	segMerge := query.Document{"$cond": []any{newer, lit(composedSegment),
		query.Document{"$cond": []any{equal,
			query.Document{"$cond": []any{
				query.Document{"$eq": []any{query.Document{"$type": "$segment"}, "object"}},
				query.Document{"$mergeObjects": []any{lit(composedSegment), "$segment"}},
				"$segment"}},
			"$segment"}}}}

	stages := []query.Document{{"$set": query.Document{
		"email":     fillScalar("email", "composed@x"),
		"nickname":  fillScalar("nickname", "filled"),
		"addresses": elemMerge,
		"segment":   segMerge,
	}}}
	if err := m.ApplyProjection(ctx, coll, "d1", stages, true); err != nil {
		t.Fatalf("apply fill: %v", err)
	}

	docs, err := m.FindManyByField(ctx, coll, "_id", "d1")
	if err != nil || len(docs) != 1 {
		t.Fatalf("find: %v %d", err, len(docs))
	}
	d := docs[0]
	if d["email"] != "stored@x" {
		t.Errorf("a present scalar must keep the STORED value at the equal revision, got %v", d["email"])
	}
	if d["nickname"] != "filled" {
		t.Errorf("a missing scalar must be filled at the equal revision, got %v", d["nickname"])
	}
	addrs, _ := d["addresses"].(bson.A)
	if len(addrs) != 1 {
		t.Fatalf("the stored element set must be unchanged, got %v", d["addresses"])
	}
	elem, _ := addrs[0].(bson.M) // nested documents decode as bson.M
	if elem["city"] != "POA" {
		t.Errorf("a present element key must keep the STORED value, got %v", elem["city"])
	}
	if elem["kind"] != "home" {
		t.Errorf("a missing element key must be filled from the composed element, got %v", elem["kind"])
	}
	seg, _ := d["segment"].(bson.M)
	if seg["name"] != "Ana" {
		t.Errorf("a present segment key must keep the STORED value, got %v", seg["name"])
	}
	if seg["grade"] != "A" {
		t.Errorf("a missing segment key must be filled from the composed segment, got %v", seg["grade"])
	}
}

func TestIntegration_EnsureProjectionState_TTLIndexIdempotent(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()

	// Twice: CreateOne must absorb the identical existing index.
	if err := m.EnsureProjectionState(ctx); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if err := m.EnsureProjectionState(ctx); err != nil {
		t.Fatalf("second ensure (idempotency): %v", err)
	}
	cur, err := m.db.Collection(query.ProjectionStateCollectionName).Indexes().List(ctx)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var found bool
	for cur.Next(ctx) {
		var idx bson.M
		if err := cur.Decode(&idx); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if keys, ok := idx["key"].(bson.M); ok {
			if _, at := keys["at"]; at {
				found = true
				ttl, _ := idx["expireAfterSeconds"].(int32)
				if int(ttl) != int(query.TombstoneTTL/time.Second) {
					t.Errorf("TTL = %d, want %d", ttl, int(query.TombstoneTTL/time.Second))
				}
			}
		}
	}
	if !found {
		t.Error("the tombstone TTL index on \"at\" must exist")
	}
}
