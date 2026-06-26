package integration

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// recordIntegrationFailure must SURFACE a slog.Warn (not silently swallow) when
// the failure row itself cannot be persisted — the side-channel degradation
// signal an operator alerts on. Best-effort: it never returns or panics, and the
// consumer loop carries on (the Kafka offset still advances upstream).
func TestRecordIntegrationFailure_LogsWarnOnPersistError(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	r := &Receiver{sourceKey: "partners", eventKey: "onboarded", consumerGroup: "g1"}
	exec := &fakeExec{execErr: errors.New("pg down")}

	recordIntegrationFailure(context.Background(), exec, r, uuid.New(), []byte(`{}`), "boom")

	if exec.calls != 1 {
		t.Fatalf("want 1 persist attempt, got %d", exec.calls)
	}
	out := buf.String()
	if !strings.Contains(out, "integration.failure.persist_error") {
		t.Errorf("persist failure must be logged at Warn, got: %s", out)
	}
	if !strings.Contains(out, "partners") || !strings.Contains(out, "g1") {
		t.Errorf("warn should carry the receiver coordinates, got: %s", out)
	}
}
