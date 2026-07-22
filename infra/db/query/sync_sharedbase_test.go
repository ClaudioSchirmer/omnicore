package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// Coverage for the SharedBase (M2) CDC fan-out: buildViewIndex registers a role
// view under its base table, and SyncEngine.process recomposes every role
// document referencing a changed shared identity.

func fanOutRoleSchema() *core.TableSchema {
	base := core.NewSharedBase("pessoa").Revision("revision").PK("id").Field("Name", "name").NaturalKey("name")
	return core.NewTableSchema[*builderTestEntity]("aluno").
		PK("id").
		Field("Email", "email").
		SharedBase(base, "pessoa_id")
}

func TestBuildViewIndex_RegistersSharedBase(t *testing.T) {
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	idx := buildViewIndex([]*ViewDefinition{view})
	if got := len(idx.bySharedBase["pessoa"]); got != 1 {
		t.Fatalf("bySharedBase[pessoa] = %d views, want 1", got)
	}
	// A base table is not a view root, so it must not land in byPGTable.
	if _, ok := idx.byPGTable["pessoa"]; ok {
		t.Errorf("a shared base must not be indexed as a view root")
	}
}

// A base event recomposes the role docs referencing that identity (fan-out).
func TestProcess_SharedBaseFanOut(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}} // one aluno references the base
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	// A base change (aggregate_type = the base table) fans out to the role views.
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "pessoa", EventType: "UPDATED", AggregateID: "p1"}); err != nil {
		t.Fatalf("process base event: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Errorf("base fan-out must recompose the referencing role doc, got %d upserts", len(coll.updates))
	}
}

// No role doc references the changed identity — the fan-out is a no-op (neither
// upsert nor delete), exercising the len(roleIDs)==0 short-circuit.
func TestProcess_SharedBaseFanOut_NoReferencingDocs(t *testing.T) {
	coll := &fakeColl{} // no aluno references the base
	eng := composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil })
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	if err := s.process(context.Background(), kafkaEvent{AggregateType: "pessoa", EventType: "UPDATED", AggregateID: "p1"}); err != nil {
		t.Fatalf("process base event: %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Errorf("no referencing role docs must be a no-op, got %d upserts / %d deletes", len(coll.updates), len(coll.deletes))
	}
}

// The projection still holds a1 but its role row vanished from the source, so the
// batched recompose yields nothing for a1 and the fan-out removes the stale doc —
// covering the "role id absent from the composed set → applyDelete" branch.
func TestProcess_SharedBaseFanOut_VanishedRoleDeleted(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
	eng := composerEngine(func(sql string, _ []any) ([]map[string]any, error) {
		return nil, nil // FROM aluno returns nothing — the role row is gone
	})
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	if err := s.process(context.Background(), kafkaEvent{AggregateType: "pessoa", EventType: "DELETED", AggregateID: "p1"}); err != nil {
		t.Fatalf("process base event: %v", err)
	}
	if len(coll.updates) != 0 {
		t.Errorf("a vanished role must not upsert, got %d", len(coll.updates))
	}
	if len(coll.deletes) != 1 || coll.deletes[0] != "a1" {
		t.Errorf("a vanished role doc must be deleted, got %v", coll.deletes)
	}
}

// Error paths of the fan-out propagate to the caller (fail the event → redelivery).
func TestProcess_SharedBaseFanOut_Errors(t *testing.T) {
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	aluno := func(sql string, _ []any) ([]map[string]any, error) {
		if strings.Contains(sql, "FROM aluno") {
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	}
	event := kafkaEvent{AggregateType: "pessoa", EventType: "UPDATED", AggregateID: "p1"}

	t.Run("findIDsError", func(t *testing.T) {
		coll := &fakeColl{findErr: errFake}
		s := NewSyncEngine(composerEngine(aluno), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)
		if err := s.process(context.Background(), event); err == nil {
			t.Fatal("expected the FindIDsByField error to propagate")
		}
	})
	t.Run("composeError", func(t *testing.T) {
		coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
		s := NewSyncEngine(composerEngine(func(string, []any) ([]map[string]any, error) { return nil, errFake }),
			newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)
		if err := s.process(context.Background(), event); err == nil {
			t.Fatal("expected the ComposeBatch error to propagate")
		}
	})
	t.Run("upsertError", func(t *testing.T) {
		coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}, updateErr: errFake}
		s := NewSyncEngine(composerEngine(aluno), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)
		if err := s.process(context.Background(), event); err == nil {
			t.Fatal("expected the applyUpsert error to propagate")
		}
	})
}

// A ROLE event carrying the v2 payload drives the SAME fan-out from its
// _ids.base_id — the empty base-table row no longer exists, so this is the
// steady-state trigger. The role's own view recompose (byPGTable) runs too:
// the fan-out targets a1 via the base id AND the direct route targets a1 via
// the aggregate id — both upserts land (idempotent by _id).
func TestProcess_RoleEventFansOutViaPayloadIDs(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process role event: %v", err)
	}
	if len(coll.updates) < 2 {
		t.Errorf("the role event must fan out (base id from _ids) AND recompose its own doc, got %d upserts", len(coll.updates))
	}
}

// An OLD role event (no _ids) skips the payload fan-out silently — its paired
// base-table row from the old producer drives the fan-out instead.
func TestProcess_RoleEventWithoutIDs_SkipsPayloadFanOut(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
	eng := composerEngine(func(sql string, args []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM pessoa"):
			return mapsFromColsData([]string{"id", "name"}, [][]any{{"p1", "Ana"}}), nil
		case strings.Contains(sql, "FROM aluno"):
			return mapsFromColsData([]string{"id", "email", "pessoa_id"}, [][]any{{"a1", "a@x", "p1"}}), nil
		}
		return nil, nil
	})
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x"}`)}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process legacy role event: %v", err)
	}
	// A legacy (non-v2) event neither fans out nor projects — WARNING + skip;
	// its paired base row (old producer) and the post-upgrade rebuild converge.
	if len(coll.updates) != 0 {
		t.Errorf("a legacy role event must be skipped entirely, got %d upserts", len(coll.updates))
	}
}

// The payload fan-out writes with upsert=false; the writer's OWN projection
// with upsert=true. The fan-out targets ids from a FindIDsByField snapshot, so
// a document missing at write time is a concurrently-deleted role — upserting
// there would resurrect a base-fields-only skeleton (no PK, no FK, no
// deleted_at) that no future event could clean and that default reads would
// list as an active row. This pins the flag on both writes of one role event.
func TestProcess_PayloadFanOut_NeverUpserts(t *testing.T) {
	coll := &fakeColl{docs: []any{map[string]any{"_id": "a1"}}}
	eng := composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil })
	view := View("aluno").Root("aluno").Schema(fanOutRoleSchema()).Version(1)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	event := kafkaEvent{
		AggregateType: "aluno", EventType: "UPDATED", AggregateID: "a1",
		Payload: []byte(`{"email":"a@x","name":"Ana","_ids":{"id":"a1","revision":2,"base_id":"p1","base_revision":3}}`),
	}
	if err := s.process(context.Background(), event); err != nil {
		t.Fatalf("process role event: %v", err)
	}
	var fanOuts, own int
	for _, u := range coll.updates {
		upsert, ok := u["$upsert"].(bool)
		if !ok {
			t.Fatalf("expected only pipeline writes on this path, got %v", u)
		}
		if upsert {
			own++
		} else {
			fanOuts++
		}
	}
	if fanOuts != 1 {
		t.Errorf("the fan-out write must run exactly once with upsert=false, got %d (updates: %d)", fanOuts, len(coll.updates))
	}
	if own != 1 {
		t.Errorf("the own-document projection must run exactly once with upsert=true, got %d (updates: %d)", own, len(coll.updates))
	}
}
