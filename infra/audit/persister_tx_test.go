package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// fakeTx satisfies pgx.Tx by embedding the interface (nil) and overriding the
// single method InsertAuditEvent exercises — Exec. Any other method call
// would panic, which is the desired guard: the persister must touch only Exec.
type fakeTx struct {
	pgx.Tx
	execErr  error
	lastSQL  string
	lastArgs []any
	calls    int
}

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.lastArgs = args
	return pgconn.CommandTag{}, f.execErr
}

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
	if err := InsertAuditEvent(context.Background(), tx, sampleEvent()); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if tx.calls != 1 {
		t.Fatalf("expected exactly one Exec, got %d", tx.calls)
	}
	if len(tx.lastArgs) != 11 {
		t.Fatalf("expected 11 bound args, got %d", len(tx.lastArgs))
	}
	// Identified actor/issuer/tenant pass through verbatim.
	if tx.lastArgs[5] != "user-42" {
		t.Errorf("actor arg = %v, want user-42", tx.lastArgs[5])
	}
	if tx.lastArgs[6] != "https://idp.example" {
		t.Errorf("issuer arg = %v, want passthrough", tx.lastArgs[6])
	}
	if tx.lastArgs[7] != "acme" {
		t.Errorf("tenant arg = %v, want passthrough", tx.lastArgs[7])
	}
}

func TestInsertAuditEvent_AnonymousActorBecomesNull(t *testing.T) {
	tx := &fakeTx{}
	ev := sampleEvent()
	ev.Actor = persistence.AnonymousActor
	ev.ActorIssuer = ""
	ev.TenantID = ""
	if err := InsertAuditEvent(context.Background(), tx, ev); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if tx.lastArgs[5] != nil {
		t.Errorf("anonymous actor must bind NULL, got %v", tx.lastArgs[5])
	}
	if tx.lastArgs[6] != nil || tx.lastArgs[7] != nil {
		t.Errorf("empty issuer/tenant must bind NULL, got %v / %v", tx.lastArgs[6], tx.lastArgs[7])
	}
}

func TestInsertAuditEvent_ExecErrorWrapped(t *testing.T) {
	tx := &fakeTx{execErr: errors.New("db down")}
	err := InsertAuditEvent(context.Background(), tx, sampleEvent())
	if err == nil || !errors.Is(err, tx.execErr) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
}
