package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// fakeTx satisfies the neutral audit.Execer surface — the single method
// InsertAuditEvent exercises. It captures the rendered SQL + bound args so the
// binding can be asserted without a live database.
type fakeTx struct {
	execErr  error
	lastSQL  string
	lastArgs []any
	calls    int
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) error {
	f.calls++
	f.lastSQL = sql
	f.lastArgs = args
	return f.execErr
}

// pgPlaceholder renders the Postgres-flavored positional placeholder used by the
// binding tests; the dialect choice is irrelevant to what they assert.
func pgPlaceholder(n int) string { return fmt.Sprintf("$%d", n) }

func sampleEvent() AuditEvent {
	return AuditEvent{
		ThreadID:    uuid.NewString(),
		EntityType:  "User",
		EntityID:    uuid.NewString(),
		Verb:        "insert",
		ActionName:  "GetInsertable",
		Kind:        "snapshot",
		Actor:       "user-42",
		ActorIssuer: "https://idp.example",
		TenantID:    "acme",
		DateTime:    time.Now().UTC(),
		Snapshot:    map[string]any{"name": "alice"},
	}
}

func TestInsertAuditEvent_BindsEveryColumn(t *testing.T) {
	tx := &fakeTx{}
	if err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, sampleEvent()); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if tx.calls != 1 {
		t.Fatalf("expected exactly one Exec, got %d", tx.calls)
	}
	if len(tx.lastArgs) != 13 {
		t.Fatalf("expected 13 bound args, got %d", len(tx.lastArgs))
	}
	// arg[0] is the Go-generated audit row id (a valid UUID).
	idStr, ok := tx.lastArgs[0].(string)
	if !ok {
		t.Fatalf("id arg = %T, want string", tx.lastArgs[0])
	}
	if _, err := uuid.Parse(idStr); err != nil {
		t.Errorf("id arg = %q, want a UUID: %v", idStr, err)
	}
	// Identified actor/issuer/tenant pass through verbatim (shifted by the id).
	if tx.lastArgs[6] != "user-42" {
		t.Errorf("actor arg = %v, want user-42", tx.lastArgs[6])
	}
	if tx.lastArgs[7] != "https://idp.example" {
		t.Errorf("issuer arg = %v, want passthrough", tx.lastArgs[7])
	}
	if tx.lastArgs[8] != "acme" {
		t.Errorf("tenant arg = %v, want passthrough", tx.lastArgs[8])
	}
	// trace_id ($13) is NULL when no span is active in the context.
	if tx.lastArgs[12] != nil {
		t.Errorf("trace_id arg = %v, want nil (no active span)", tx.lastArgs[12])
	}
}

func TestInsertAuditEvent_TraceIDBoundWhenSet(t *testing.T) {
	tx := &fakeTx{}
	ev := sampleEvent()
	ev.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	if err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, ev); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if tx.lastArgs[12] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id arg = %v, want the stamped hex (mirrors the echo)", tx.lastArgs[12])
	}
}

func TestInsertAuditEvent_AnonymousActorBecomesNull(t *testing.T) {
	tx := &fakeTx{}
	ev := sampleEvent()
	ev.Actor = persistence.AnonymousActor
	ev.ActorIssuer = ""
	ev.TenantID = ""
	if err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, ev); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if tx.lastArgs[6] != nil {
		t.Errorf("anonymous actor must bind NULL, got %v", tx.lastArgs[6])
	}
	if tx.lastArgs[7] != nil || tx.lastArgs[8] != nil {
		t.Errorf("empty issuer/tenant must bind NULL, got %v / %v", tx.lastArgs[7], tx.lastArgs[8])
	}
}

func TestInsertAuditEvent_ExecErrorWrapped(t *testing.T) {
	tx := &fakeTx{execErr: errors.New("db down")}
	err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, sampleEvent())
	if err == nil || !errors.Is(err, tx.execErr) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
}
