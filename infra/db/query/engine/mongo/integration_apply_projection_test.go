//go:build integration && postgres

package mongo

import (
	"context"
	"testing"

	query "github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// FURO-9: ApplyProjection evaluated by a REAL MongoDB — the unit fakes only
// record the pipeline shape; this proves the server-side semantics the
// payload-direct projection depends on: upsert-by-pipeline, the revision
// watermark refusing older writes, and the surgical child-array edit.
func TestIntegration_ApplyProjection_RevisionGuard(t *testing.T) {
	m, cleanup := newTestMongo(t)
	defer cleanup()
	ctx := context.Background()
	coll := pc("it_apply_projection")
	defer func() { _ = m.DropCollection(ctx, coll) }()

	stage := func(name string, rev int64) []query.Document {
		newer := query.Document{"$lt": []any{query.Document{"$ifNull": []any{"$_revision", int64(-1)}}, rev}}
		return []query.Document{{"$set": query.Document{
			"name":      query.Document{"$cond": []any{newer, query.Document{"$literal": name}, "$name"}},
			"_revision": query.Document{"$cond": []any{newer, rev, query.Document{"$ifNull": []any{"$_revision", rev}}}},
		}}}
	}
	// rev 2 lands…
	if err := m.ApplyProjection(ctx, coll, "d1", stage("new", 2), true); err != nil {
		t.Fatalf("apply rev2: %v", err)
	}
	// …a zombie rev 1 must be a no-op…
	if err := m.ApplyProjection(ctx, coll, "d1", stage("old", 1), true); err != nil {
		t.Fatalf("apply rev1: %v", err)
	}
	docs, err := m.FindManyByField(ctx, coll, "_id", "d1")
	if err != nil || len(docs) != 1 {
		t.Fatalf("find: %v %d", err, len(docs))
	}
	if docs[0]["name"] != "new" {
		t.Fatalf("the older revision must not regress the document, got %v", docs[0])
	}
	// …and a newer rev 3 lands again.
	if err := m.ApplyProjection(ctx, coll, "d1", stage("newer", 3), true); err != nil {
		t.Fatalf("apply rev3: %v", err)
	}
	docs, _ = m.FindManyByField(ctx, coll, "_id", "d1")
	if docs[0]["name"] != "newer" {
		t.Fatalf("a newer revision must land, got %v", docs[0])
	}
}
