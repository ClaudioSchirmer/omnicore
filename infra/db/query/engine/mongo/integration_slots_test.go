//go:build integration && postgres

package mongo

import (
	"context"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// TestReader_FollowsActiveSlotPointer is the blue-green Phase 2 end-to-end: once
// the registry's active_collection points at a slot and the resolver is
// refreshed, the reader resolves reads to that slot and never consults the bare
// collection. It exercises provisionSlot (shape the slot) and DropCollection
// (reclaim) alongside the read path.
func TestReader_FollowsActiveSlotPointer(t *testing.T) {
	pg, cleanupPG := newTestPG(t)
	defer cleanupPG()
	m, cleanupMongo := newTestMongo(t)
	defer cleanupMongo()

	ctx := context.Background()
	v := query.View("slotview").Version(1)
	resolver := query.NewViewResolver(pg)
	reader := NewMongoViewReader(m, resolver)
	reader.SetViews([]*query.ViewDefinition{v})

	if err := query.InitViewRegistry(ctx, pg.Querier(), pg.Dialect(), query.InitViewRegistryInput{
		ViewName: "slotview", Version: 1, RebuildHash: "h", ArtifactHash: "h", CombinedHash: "h",
		ServiceName: "svc", Now: time.Now(),
	}); err != nil {
		t.Fatalf("InitViewRegistry: %v", err)
	}

	// The first shadow off the bare state is slotview__0. Shape it, then point
	// the registry pointer at it (a manual stand-in for the driver's flip).
	slot := resolver.Shadow("slotview")
	if slot.String() != "slotview__0" {
		t.Fatalf("Shadow = %q, want slotview__0", slot.String())
	}
	if err := provisionSlot(ctx, m, v, slot); err != nil {
		t.Fatalf("provisionSlot: %v", err)
	}
	if err := pg.Querier().Exec(ctx,
		`UPDATE omnicore_mongo_views SET active_collection = $1 WHERE view_name = $2`,
		slot.String(), "slotview"); err != nil {
		t.Fatalf("point active_collection: %v", err)
	}
	if err := resolver.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	// Write into the ACTIVE slot; the bare collection stays empty.
	if err := m.Upsert(ctx, resolver.Active("slotview"), "id1",
		map[string]any{"_id": "id1", "name": "x", "deleted_at": nil}); err != nil {
		t.Fatalf("Upsert into active slot: %v", err)
	}

	// The reader resolves to the slot and finds the doc.
	doc, ok, err := reader.ReadByID(ctx, "slotview", "id1", queries.ReadCriteria{})
	if err != nil {
		t.Fatalf("ReadByID: %v", err)
	}
	if !ok || doc["name"] != "x" {
		t.Fatalf("reader did not follow the pointer: ok=%v doc=%v", ok, doc)
	}

	// The bare collection was never written — proof the read went to the slot.
	// pc() resolves to the bare name (identity), unlike resolver.Active now.
	bareHas, err := m.HasDocuments(ctx, pc("slotview"))
	if err != nil {
		t.Fatalf("HasDocuments(bare): %v", err)
	}
	if bareHas {
		t.Error("bare collection should be empty; reads must not target it")
	}

	// Reclaim drops the slot; a fresh read then finds nothing.
	if err := m.DropCollection(ctx, slot); err != nil {
		t.Fatalf("DropCollection: %v", err)
	}
	if _, ok, _ := reader.ReadByID(ctx, "slotview", "id1", queries.ReadCriteria{}); ok {
		t.Error("doc still found after the slot was dropped")
	}
}
