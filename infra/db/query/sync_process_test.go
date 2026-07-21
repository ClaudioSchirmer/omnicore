package query

import (
	"context"
	"testing"
)

// In-package unit coverage of SyncEngine.process — the CDC event handler that
// routes by event type and composes+upserts (or deletes) the read-model doc.
// Driven through the backend-neutral fake ReadModelStore + a fake relational
// engine (no live Postgres/Mongo, no Kafka), so the routing + compose + store
// interaction run end-to-end in the default unit suite.

func processView(table string, deleteOnArchive bool) *ViewDefinition {
	v := View(table).Root(table).Schema(rootSchema(table)).Version(1)
	if deleteOnArchive {
		v.DeleteOnArchive()
	}
	return v
}

func processEngineWithRow() *fakeEngine {
	return composerEngine(func(string, []any) ([]map[string]any, error) {
		return mapsFromColsData([]string{"id", "name"}, [][]any{{"r1", "Alice"}}), nil
	})
}

func TestProcess_Inserted_UpsertsDoc(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	// v2 payload → payload-direct projection (one atomic pipeline, no re-read).
	ev := kafkaEvent{AggregateType: "t", EventType: "INSERTED", AggregateID: "r1",
		Payload: []byte(`{"name":"Ana","_ids":{"id":"r1"}}`)}
	if err := s.process(context.Background(), ev); err != nil {
		t.Fatalf("process INSERTED: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Fatalf("INSERTED must project once, got %d updates", len(coll.updates))
	}
	if _, ok := coll.updates[0]["$pipeline"]; !ok {
		t.Errorf("the projection must be a pipeline update, got %v", coll.updates[0])
	}
}

// A pre-v2 payload on an entity view is a WARNING + SKIP — never a silent
// relational re-read (maintainer decision; the post-upgrade rebuild converges).
func TestProcess_NonV2Payload_SkipsProjection(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "t", EventType: "INSERTED", AggregateID: "r1"}); err != nil {
		t.Fatalf("process legacy INSERTED: %v", err)
	}
	if len(coll.updates) != 0 {
		t.Errorf("a non-v2 payload must be skipped, got %d updates", len(coll.updates))
	}
}

func TestProcess_Deleted_RemovesDoc(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "t", EventType: "DELETED", AggregateID: "r1"}); err != nil {
		t.Fatalf("process DELETED: %v", err)
	}
	if len(coll.deletes) != 1 || len(coll.updates) != 0 {
		t.Errorf("DELETED must delete (no upsert), got %d deletes %d updates", len(coll.deletes), len(coll.updates))
	}
}

func TestProcess_ArchivedDefault_KeepsDocViaUpsert(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "t", EventType: "ARCHIVED", AggregateID: "r1",
		Payload: []byte(`{"name":"Ana","deleted_at":"2026-07-20T12:00:00Z","_ids":{"id":"r1"}}`)}); err != nil {
		t.Fatalf("process ARCHIVED: %v", err)
	}
	if len(coll.updates) != 1 || len(coll.deletes) != 0 {
		t.Errorf("default ARCHIVED must project (keep), got %d updates %d deletes", len(coll.updates), len(coll.deletes))
	}
}

func TestProcess_ArchivedDeleteOnArchive_RemovesDoc(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", true)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "t", EventType: "ARCHIVED", AggregateID: "r1"}); err != nil {
		t.Fatalf("process ARCHIVED: %v", err)
	}
	if len(coll.deletes) != 1 || len(coll.updates) != 0 {
		t.Errorf("DeleteOnArchive ARCHIVED must delete, got %d deletes %d updates", len(coll.deletes), len(coll.updates))
	}
}

func TestProcess_UnknownAggregateType_Noop(t *testing.T) {
	coll := &fakeColl{}
	s := NewSyncEngine(processEngineWithRow(), newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "ghost", EventType: "INSERTED", AggregateID: "r1"}); err != nil {
		t.Errorf("unknown aggregate_type must not fail, got %v", err)
	}
	if len(coll.updates) != 0 || len(coll.deletes) != 0 {
		t.Error("unknown aggregate_type must be a no-op")
	}
}

func TestProcess_AbsentRoot_NoUpsert(t *testing.T) {
	coll := &fakeColl{}
	emptyEng := composerEngine(func(string, []any) ([]map[string]any, error) { return nil, nil })
	s := NewSyncEngine(emptyEng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{processView("t", false)}, 1)
	if err := s.process(context.Background(), kafkaEvent{AggregateType: "t", EventType: "INSERTED", AggregateID: "missing"}); err != nil {
		t.Errorf("absent root must not fail process(), got %v", err)
	}
	if len(coll.updates) != 0 {
		t.Error("absent root composes to nil → no upsert")
	}
}

func TestTopicFromTable(t *testing.T) {
	if got := topicFromTable("users"); got != "users.events" {
		t.Errorf("topicFromTable = %q, want users.events", got)
	}
}
