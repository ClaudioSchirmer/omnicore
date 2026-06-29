package audit

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
)

func newCaptureLogger() (*slog.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	h := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), buf
}

func extractAuditLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid json log line: %v\n%s", err, line)
		}
		if entry["msg"] == "audit" {
			return entry
		}
	}
	t.Fatalf("no audit line in buf:\n%s", buf.String())
	return nil
}

func TestEchoSlog_FlatTopLevelFields(t *testing.T) {
	logger, buf := newCaptureLogger()
	ev := appaudit.AuditEvent{
		ThreadID:   uuid.NewString(),
		EntityType: "User",
		EntityID:   "user-1",
		Verb:       "insert",
		ActionName: "GetInsertable",
		Kind:       "snapshot",
		Actor:      "alice",
		DateTime:   time.Now(),
		Snapshot:   map[string]any{"name": "alice", "email": "a@x.com"},
	}
	EchoSlog(nil, logger, ev)
	line := extractAuditLogLine(t, buf)

	// Mandatory fields always present.
	for _, key := range []string{"threadId", "entityType", "entityId", "verb", "actionName", "kind", "actor", "dateTime"} {
		if _, ok := line[key]; !ok {
			t.Errorf("missing top-level field %q in line: %+v", key, line)
		}
	}
	// Snapshot present.
	snap, ok := line["snapshot"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing or wrong shape: %+v", line)
	}
	if snap["name"] != "alice" {
		t.Errorf("snapshot.name = %v", snap["name"])
	}
}

func TestEchoSlog_OmitemptyFieldsSkippedWhenAbsent(t *testing.T) {
	logger, buf := newCaptureLogger()
	// Minimal event — no ActorIssuer, no ActorClaims, no TenantID, no
	// Snapshot/Changes/Children. Nothing should leak as empty values.
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID:   "t",
		EntityType: "T",
		EntityID:   "id",
		Verb:       "archive",
		ActionName: "GetArchivable",
		Kind:       "transition",
		Actor:      "anonymous",
		DateTime:   time.Now(),
	})
	line := extractAuditLogLine(t, buf)
	for _, key := range []string{"actorIssuer", "actorClaims", "tenantId", "snapshot", "changes", "children"} {
		if _, present := line[key]; present {
			t.Errorf("unexpected %q in line (omitempty should suppress): %v", key, line[key])
		}
	}
}

func TestEchoSlog_TenantIDEmittedWhenPopulated(t *testing.T) {
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID:   "t",
		EntityType: "T",
		EntityID:   "id",
		Verb:       "insert",
		ActionName: "GetInsertable",
		Kind:       "snapshot",
		Actor:      "alice",
		TenantID:   "acme",
		DateTime:   time.Now(),
	})
	line := extractAuditLogLine(t, buf)
	if line["tenantId"] != "acme" {
		t.Errorf("tenantId = %v, want acme", line["tenantId"])
	}
}

func TestEchoSlog_TraceIDMirroredToEcho(t *testing.T) {
	// The slog echo must carry the same trace_id the in-TX audit_events row
	// does, so both audit destinations pivot to the trace.
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID:   "t",
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		EntityType: "T",
		EntityID:   "id",
		Verb:       "insert",
		ActionName: "GetInsertable",
		Kind:       "snapshot",
		DateTime:   time.Now(),
	})
	line := extractAuditLogLine(t, buf)
	if line["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceId = %v, want the stamped hex", line["traceId"])
	}
}

func TestEchoSlog_TraceIDOmittedWhenEmpty(t *testing.T) {
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID: "t", EntityType: "T", EntityID: "id", Verb: "insert",
		ActionName: "GetInsertable", Kind: "snapshot", DateTime: time.Now(),
	})
	line := extractAuditLogLine(t, buf)
	if _, ok := line["traceId"]; ok {
		t.Error("traceId must be omitted when tracing is off (empty)")
	}
}

func TestEchoSlog_ChildrenBlockEmittedWhenPopulated(t *testing.T) {
	logger, buf := newCaptureLogger()
	EchoSlog(nil, logger, appaudit.AuditEvent{
		ThreadID:   "t",
		EntityType: "User",
		EntityID:   "u",
		Verb:       "update",
		ActionName: "GetUpdatable",
		Kind:       "delta",
		Actor:      "alice",
		DateTime:   time.Now(),
		Children: map[string][]appaudit.ChildEvent{
			"Address": {{ID: "a1", Op: "inserted", Snapshot: map[string]any{"city": "X"}}},
		},
	})
	line := extractAuditLogLine(t, buf)
	children, ok := line["children"].(map[string]any)
	if !ok {
		t.Fatalf("children block missing: %+v", line)
	}
	addrs, ok := children["Address"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("Address slice = %+v", children["Address"])
	}
}

func TestEchoSlog_NilLoggerFallsBackToDefault(t *testing.T) {
	// Should not panic when logger is nil — slog.Default() is the documented
	// fallback.
	EchoSlog(nil, nil, appaudit.AuditEvent{
		ThreadID: "t", EntityType: "T", EntityID: "id",
		Verb: "insert", ActionName: "x", Kind: "snapshot",
		Actor: "anonymous", DateTime: time.Now(),
	})
}
