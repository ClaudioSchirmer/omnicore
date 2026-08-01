package query

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// noopRelReader is a minimal query.RelationalReader: it only exists to mark a
// view as RelationalSource() in these tests. Its load methods panic — a
// RelationalSource view must NEVER be reached by the Mongo projection paths, so
// any call here is itself the failure the tests guard against.
type noopRelReader struct{ table string }

func (r noopRelReader) FindAllEntities(context.Context, *criteria.Query) ([]domain.Entity, error) {
	panic("relational reader must not be invoked by the projection path")
}
func (r noopRelReader) CountEntities(context.Context, *criteria.Query) (int64, error) {
	panic("relational reader must not be invoked by the projection path")
}
func (r noopRelReader) BoundTable() string { return r.table }

func relationalView(table string) *ViewDefinition {
	return View(table).Schema(rootSchema(table)).Version(1).RelationalSource(noopRelReader{table: table})
}

// A RelationalSource view is served from the SoR and never materialized. The
// base-revision handshake's pull-repair iterates the same view list as
// projectOwnViews, so it must apply the same skip — otherwise it would
// compose+upsert a Mongo document for a view that has no collection (reachable
// when a relational view is rooted on a SharedBase role table).
func TestPullSideRepair_SkipsRelationalView(t *testing.T) {
	coll := &fakeColl{}
	store := newFakeMongo(coll)
	// Seed the projection-state registry so stampedBaseRevision (which reads
	// _id=baseRevisionID(baseTable, baseID) from the state collection) returns a
	// value HIGHER than the event's BaseRevision — that is what makes
	// pullSideRepair proceed past its short-circuit and into the view loop.
	store.state = &fakeColl{docs: []any{
		map[string]any{"_id": baseRevisionID("bases", "b1"), "base_revision": int64(9)},
	}}
	rel := relationalView("t")
	s := NewSyncEngine(processEngineWithRow(), store, identityResolver, nil, "", []*ViewDefinition{rel}, 1)

	ev := kafkaEvent{AggregateType: "t", EventType: "INSERTED", AggregateID: "r1"}
	ids := payloadIDs{ID: "r1", BaseID: "b1", BaseRevision: 1}
	if err := s.pullSideRepair(context.Background(), ev, ids, "bases", []*ViewDefinition{rel}); err != nil {
		t.Fatalf("pullSideRepair: %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Fatalf("a RelationalSource view must never be materialized by the pull-repair handshake, got %d upserts %d deletes",
			len(coll.updates), len(coll.deletes))
	}
}

// The everyday projection path (projectOwnViews) already skips a relational
// view; this locks that in end-to-end through process(), the sibling of the
// pull-repair skip above.
func TestProcess_RelationalView_NeverMaterialized(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{relationalView("t")}, 1)
	ev := kafkaEvent{AggregateType: "t", EventType: "INSERTED", AggregateID: "r1",
		Payload: []byte(`{"name":"Ana","_ids":{"id":"r1"}}`)}
	if err := s.process(context.Background(), ev); err != nil {
		t.Fatalf("process INSERTED: %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Fatalf("a RelationalSource view must not project to Mongo, got %d upserts %d deletes",
			len(coll.updates), len(coll.deletes))
	}
}
