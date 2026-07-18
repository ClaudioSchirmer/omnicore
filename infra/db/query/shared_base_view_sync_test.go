package query

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/transport"
)

// Coverage for the SharedBaseView SyncEngine routing: the byRoleTable index,
// the role-table topic subscription, and the base-rooted recompose — base id
// resolution per link model (shared-PK identity / separate-FK source lookup /
// DELETED payload keys), the purge-convergence delete and the malformed-payload
// skip.

func TestBuildViewIndex_RegistersRoleTables(t *testing.T) {
	idx := buildViewIndex([]*ViewDefinition{sbvView()})
	if got := len(idx.byRoleTable["sbv_users"]); got != 1 {
		t.Errorf("byRoleTable[sbv_users] = %d routes, want 1", got)
	}
	if got := len(idx.byRoleTable["sbv_employees"]); got != 1 {
		t.Errorf("byRoleTable[sbv_employees] = %d routes, want 1", got)
	}
	// The base IS this view's root: byPGTable routes base events, and the view
	// must NOT land in bySharedBase (that bucket is the role-view fan-out).
	if got := len(idx.byPGTable["sbv_persons"]); got != 1 {
		t.Errorf("byPGTable[sbv_persons] = %d views, want 1 (base events route as root events)", got)
	}
	if _, ok := idx.bySharedBase["sbv_persons"]; ok {
		t.Error("a base-rooted view must not register in bySharedBase (it is not a role view)")
	}
}

func TestNewSyncEngine_TopicsIncludeRoleTables(t *testing.T) {
	s := NewSyncEngine(composerEngine(nil), newFakeMongo(&fakeColl{}), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	want := map[string]bool{
		"sbv_persons.events":   false, // the root
		"sbv_users.events":     false, // role — ARCHIVE/UNARCHIVE emit only the role event
		"sbv_employees.events": false,
	}
	for _, topic := range s.topics {
		if _, tracked := want[topic]; tracked {
			want[topic] = true
		}
	}
	for topic, seen := range want {
		if !seen {
			t.Errorf("topic %q missing from the subscription (%v)", topic, s.topics)
		}
	}
}

// sbvSyncEngine builds a SyncEngine over the two-role fixture with a scriptable
// relational read and a capturing Mongo collection.
func sbvSyncEngine(t *testing.T, rows map[string][]map[string]any, calls *[]string, args *[][]any) (*SyncEngine, *fakeColl) {
	t.Helper()
	coll := &fakeColl{}
	eng := sbvComposerEngine(t, rows, calls, args)
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	return s, coll
}

func sbvBaseRows() map[string][]map[string]any {
	return map[string][]map[string]any{
		"FROM sbv_persons":   mapsFromColsData([]string{"id", "document", "name"}, [][]any{{"p1", "D1", "Ana"}}),
		"FROM sbv_users":     mapsFromColsData([]string{"id", "user_name"}, [][]any{{"p1", "ana"}}),
		"FROM sbv_employees": mapsFromColsData([]string{"id", "person_id", "employee_number"}, [][]any{{"e9", "p1", "M1"}}),
	}
}

// A shared-PK role event resolves the base id by identity (aggregate_id IS the
// base id) and recomposes the person document.
func TestProcess_RoleEvent_SharedPKIdentity(t *testing.T) {
	var calls []string
	var args [][]any
	s, coll := sbvSyncEngine(t, sbvBaseRows(), &calls, &args)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_users", EventType: "UPDATED", AggregateID: "p1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Fatalf("a shared-PK role event must recompose the person doc, got %d upserts", len(coll.updates))
	}
	doc, _ := coll.updates[0]["$set"].(Document)
	if doc["id"] != "p1" || doc["name"] != "Ana" {
		t.Errorf("recomposed doc must be the person p1, got %v", doc)
	}
}

// A separate-FK role event (row still exists) consults the source for the FK.
func TestProcess_RoleEvent_SeparateFKConsultsSource(t *testing.T) {
	var calls []string
	var args [][]any
	s, coll := sbvSyncEngine(t, sbvBaseRows(), &calls, &args)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "ARCHIVED", AggregateID: "e9",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Fatalf("a separate-FK role event must recompose the person doc, got %d upserts", len(coll.updates))
	}
	// The resolution consults the role row BY ITS PK (the consult-always rule).
	consulted := false
	for i, sql := range calls {
		if strings.Contains(sql, "FROM sbv_employees") && strings.Contains(sql, "WHERE id =") {
			if len(args[i]) == 1 && args[i][0] == "e9" {
				consulted = true
			}
		}
	}
	if !consulted {
		t.Errorf("the FK must be resolved from the source row (pk lookup), calls: %v", calls)
	}
}

// A separate-FK role DELETED has nothing left to consult: the base id comes
// from the payload's structural keys, and the recompose flips the segment.
func TestProcess_RoleDeleted_SeparateFKUsesPayload(t *testing.T) {
	rows := sbvBaseRows()
	delete(rows, "FROM sbv_employees") // the row is gone
	var calls []string
	var args [][]any
	s, coll := sbvSyncEngine(t, rows, &calls, &args)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "DELETED", AggregateID: "e9",
		Payload: []byte(`{"id":"e9","person_id":"p1"}`),
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.updates) != 1 {
		t.Fatalf("a role DELETED must recompose the person doc via the payload FK, got %d upserts", len(coll.updates))
	}
	doc, _ := coll.updates[0]["$set"].(Document)
	if seg, present := doc["sbvEmployee"]; !present || seg != nil {
		t.Errorf("the deleted role's segment must recompose to explicit nil, got %#v", seg)
	}
	// Nothing may consult the vanished row by PK.
	for i, sql := range calls {
		if strings.Contains(sql, "FROM sbv_employees") && strings.Contains(sql, "WHERE id =") && len(args[i]) == 1 && args[i][0] == "e9" {
			t.Errorf("a DELETED role must not be consulted by PK (the row is gone), got %q", sql)
		}
	}
}

// The purge convergence: the last role's DELETE also purged the identity —
// the composition comes back nil and the person document is removed.
func TestProcess_RoleDeleted_PurgedIdentityRemovesDoc(t *testing.T) {
	var calls []string
	var args [][]any
	s, coll := sbvSyncEngine(t, map[string][]map[string]any{ /* everything gone */ }, &calls, &args)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "DELETED", AggregateID: "e9",
		Payload: []byte(`{"id":"e9","person_id":"p1"}`),
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.deletes) != 1 || coll.deletes[0] != "p1" {
		t.Errorf("a purged identity must remove the person doc, got deletes=%v", coll.deletes)
	}
	if len(coll.updates) != 0 {
		t.Errorf("no upsert may follow a nil composition, got %d", len(coll.updates))
	}
}

// A malformed DELETED payload (missing the FK) cannot resolve the base id —
// the event is skipped with a log, never an error (at-least-once safe).
func TestProcess_RoleDeleted_MissingPayloadSkips(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte(`not-json`), []byte(`{"id":"e9"}`)} {
		var calls []string
		var args [][]any
		s, coll := sbvSyncEngine(t, sbvBaseRows(), &calls, &args)
		if err := s.process(context.Background(), kafkaEvent{
			AggregateType: "sbv_employees", EventType: "DELETED", AggregateID: "e9",
			Payload: payload,
		}); err != nil {
			t.Fatalf("payload %q: process must not error, got %v", payload, err)
		}
		if len(coll.updates) != 0 || len(coll.deletes) != 0 {
			t.Errorf("payload %q: an unresolvable base id must skip, got updates=%d deletes=%d",
				payload, len(coll.updates), len(coll.deletes))
		}
	}
}

// A non-DELETED event whose row vanished before processing yields "" and skips —
// the trailing DELETED (same partition, ordered) converges the document.
func TestProcess_RoleEvent_VanishedRowSkips(t *testing.T) {
	rows := sbvBaseRows()
	delete(rows, "FROM sbv_employees")
	var calls []string
	var args [][]any
	s, coll := sbvSyncEngine(t, rows, &calls, &args)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "UPDATED", AggregateID: "e9",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.updates) != 0 {
		t.Errorf("a vanished role row must skip the recompose, got %d upserts", len(coll.updates))
	}
}

// Base events flow through the root branch: ARCHIVED under DeleteOnArchive
// removes the person document.
func TestProcess_BaseArchived_DeleteOnArchiveRemovesDoc(t *testing.T) {
	coll := &fakeColl{}
	eng := composerEngine(nil)
	view := SharedBaseView(sbvBase(), "persons_hot").Role(sbvUserSchema()).Version(1).DeleteOnArchive()
	s := NewSyncEngine(eng, newFakeMongo(coll), identityResolver, nil, "", []*ViewDefinition{view}, 1)

	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_persons", EventType: "ARCHIVED", AggregateID: "p1",
	}); err != nil {
		t.Fatalf("process: %v", err)
	}
	if len(coll.deletes) != 1 || coll.deletes[0] != "p1" {
		t.Errorf("base ARCHIVED under DeleteOnArchive must remove the person doc, got %v", coll.deletes)
	}
}

// --- rebuild scan SQL ---------------------------------------------------------

func TestRebuildScanSQL(t *testing.T) {
	// A view whose root declares CreatedAt keeps the creation-order scan.
	withCreated := core.NewTableSchema[*sbvEmployee]("stamped").
		PK("emp_id").Field("EmployeeNumber", "employee_number").CreatedAt("created_at")
	regular := View("emps").Version(1).Root("stamped").Schema(withCreated)
	if q := rebuildScanSQL(regular); q != "SELECT emp_id FROM stamped ORDER BY created_at" {
		t.Errorf("scan = %q, want SELECT emp_id FROM stamped ORDER BY created_at", q)
	}
	// A base root declares no CreatedAt — the scan falls back to the PK.
	person := sbvView()
	if q := rebuildScanSQL(person); q != "SELECT id FROM sbv_persons ORDER BY id" {
		t.Errorf("base-rooted scan = %q, want SELECT id FROM sbv_persons ORDER BY id", q)
	}
}

// Error propagation of the base-rooted recompose: the resolve lookup, the
// composition, the delete and the upsert each surface their failure.
func TestProcess_RoleEvent_ErrorPropagation(t *testing.T) {
	// resolveBaseID lookup failure (separate-FK, non-DELETED).
	engErr := composerEngine(func(sql string, a []any) ([]map[string]any, error) {
		if strings.Contains(sql, "FROM sbv_employees") {
			return nil, errFake
		}
		return nil, nil
	})
	s := NewSyncEngine(engErr, newFakeMongo(&fakeColl{}), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "UPDATED", AggregateID: "e9",
	}); err == nil {
		t.Error("a resolve lookup failure must propagate")
	}

	// Compose failure (the person fetch errors after a successful resolve).
	engCompose := composerEngine(func(sql string, a []any) ([]map[string]any, error) {
		switch {
		case strings.Contains(sql, "FROM sbv_persons"):
			return nil, errFake
		case strings.Contains(sql, "FROM sbv_employees"):
			return mapsFromColsData([]string{"id", "person_id", "employee_number"}, [][]any{{"e9", "p1", "M1"}}), nil
		}
		return nil, nil
	})
	s = NewSyncEngine(engCompose, newFakeMongo(&fakeColl{}), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "UPDATED", AggregateID: "e9",
	}); err == nil {
		t.Error("a compose failure must propagate")
	}

	// Upsert failure.
	var calls []string
	var args [][]any
	engOK := sbvComposerEngine(t, sbvBaseRows(), &calls, &args)
	s = NewSyncEngine(engOK, newFakeMongo(&fakeColl{updateErr: errFake}), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_users", EventType: "UPDATED", AggregateID: "p1",
	}); err == nil {
		t.Error("an upsert failure must propagate")
	}

	// Delete failure on a nil composition (purged identity).
	calls, args = nil, nil
	engEmpty := sbvComposerEngine(t, map[string][]map[string]any{}, &calls, &args)
	s = NewSyncEngine(engEmpty, newFakeMongo(&fakeColl{deleteErr: errFake}), identityResolver, nil, "", []*ViewDefinition{sbvView()}, 1)
	if err := s.process(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "DELETED", AggregateID: "e9",
		Payload: []byte(`{"id":"e9","person_id":"p1"}`),
	}); err == nil {
		t.Error("a delete failure must propagate")
	}
}

// resolveBaseID defensive branches: a role without a SharedBase reference
// yields "" (unreachable through Role(), which vetoes it), and a role row whose
// FK column is NULL yields "" too.
func TestResolveBaseID_DefensiveBranches(t *testing.T) {
	var calls []string
	var args [][]any
	s, _ := sbvSyncEngine(t, map[string][]map[string]any{
		"FROM sbv_employees": mapsFromColsData([]string{"id", "person_id", "employee_number"},
			[][]any{{"e9", nil, "M1"}}), // NULL FK
	}, &calls, &args)

	noRef := roleDef{schema: core.NewTableSchema[*sbvUser]("loose").PK("id").Field("UserName", "user_name")}
	if id, err := s.resolveBaseID(context.Background(), kafkaEvent{AggregateID: "x"}, noRef); err != nil || id != "" {
		t.Errorf("a role without SharedBaseRef must resolve to nothing, got %q err=%v", id, err)
	}
	empRole := roleDef{schema: sbvEmployeeSchema(), segment: "sbvEmployee"}
	if id, err := s.resolveBaseID(context.Background(), kafkaEvent{
		AggregateType: "sbv_employees", EventType: "UPDATED", AggregateID: "e9",
	}, empRole); err != nil || id != "" {
		t.Errorf("a NULL FK must resolve to nothing, got %q err=%v", id, err)
	}
}

// extractEvent carries the payload (routing hint) and every header, including
// the traceparent link.
func TestExtractEvent_CarriesPayloadAndHeaders(t *testing.T) {
	msg := kafkaMessageFor(t, "sbv_employees", "DELETED", "e9", `{"id":"e9","person_id":"p1"}`)
	e := extractEvent(msg)
	if e.AggregateType != "sbv_employees" || e.EventType != "DELETED" || e.AggregateID != "e9" {
		t.Fatalf("headers mis-extracted: %+v", e)
	}
	if e.Traceparent != "00-abc-def-01" {
		t.Errorf("traceparent header must extract, got %q", e.Traceparent)
	}
	if string(e.Payload) != `{"id":"e9","person_id":"p1"}` {
		t.Errorf("payload must carry the message value, got %q", e.Payload)
	}
}

func kafkaMessageFor(t *testing.T, aggType, eventType, id, payload string) transport.Message {
	t.Helper()
	return transport.Message{
		Key:   []byte(id),
		Value: []byte(payload),
		Headers: map[string]string{
			"aggregate_type": aggType,
			"event_type":     eventType,
			"traceparent":    "00-abc-def-01",
		},
	}
}
