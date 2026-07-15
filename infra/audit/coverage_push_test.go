package audit

import (
	"context"
	"testing"
	"time"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/google/uuid"
)

// TestInsertAuditEvent_PayloadMarshalError drives the buildAuditPayload error
// branch: a Snapshot carrying an unmarshalable value (a channel) makes
// json.Marshal fail before any Exec is attempted.
func TestInsertAuditEvent_PayloadMarshalError(t *testing.T) {
	tx := &fakeTx{}
	ev := sampleEvent()
	ev.Snapshot = map[string]any{"bad": make(chan int)}
	err := InsertAuditEvent(context.Background(), tx, pgPlaceholder, passthroughEncode, ev)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if tx.calls != 0 {
		t.Fatalf("Exec must not run when payload marshal fails, got %d calls", tx.calls)
	}
}

// TestEchoSlog_AllOptionalBlocksEmitted populates ActorIssuer, ActorClaims,
// Snapshot and Changes so every omitempty branch in EchoSlog runs.
func TestEchoSlog_AllOptionalBlocksEmitted(t *testing.T) {
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID:    uuid.NewString(),
		EntityType:  "User",
		EntityID:    uuid.NewString(),
		Verb:        "update",
		ActionName:  "GetUpdatable",
		Kind:        "delta",
		Actor:       "user-7",
		ActorIssuer: "https://idp.example",
		ActorClaims: map[string]any{"roles": []string{"admin"}},
		DateTime:    time.Now().UTC(),
		Snapshot:    map[string]any{"name": "alice"},
		Changes:     []appaudit.FieldChange{{Field: "name", From: "a", To: "b"}},
	})
	entry := extractAuditLogLine(t, buf)
	for _, k := range []string{"actorIssuer", "actorClaims", "snapshot", "changes"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("expected %q on the audit slog line", k)
		}
	}
}
