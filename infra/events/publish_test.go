package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// newCapture returns a SlogPublisher whose output is captured in a buffer.
func newCapture() (*SlogPublisher, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	handler := slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return NewSlogPublisher(slog.New(handler)), buf
}

func newCtx() persistence.RequestContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func newCtxWithIdentity(sub, iss string) persistence.RequestContext {
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{Subject: sub, Issuer: iss})
	return ctx
}

// extractEvent returns the first JSON line in buf with msg=event parsed as a
// flat map[string]any. Symmetric with audit.extractAuditLine — all flat fields
// (threadId, entityType, eventType, actor, dateTime, message, values,
// exception) live at the top level.
func extractEvent(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("invalid json log line: %v\n%s", err, line)
		}
		if entry["msg"] == "event" {
			return entry
		}
	}
	t.Fatalf("no event line in buf:\n%s", buf.String())
	return nil
}

func TestSlogPublisher_NilLoggerFallback(t *testing.T) {
	p := NewSlogPublisher(nil)
	if p.logger == nil {
		t.Fatal("nil logger should fall back to slog.Default()")
	}
}

func TestSlogPublisher_FlatTopLevelFields(t *testing.T) {
	pub, buf := newCapture()
	ev := domain.DomainEvent{
		Type:  domain.EventLog,
		Class: "User",
		Msg:   "user activated",
		Vals:  map[string]any{"plan": "premium"},
	}
	if err := pub.Publish(newCtxWithIdentity("alice", "https://idp.test"), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	line := extractEvent(t, buf)

	for _, key := range []string{"threadId", "entityType", "eventType", "actor", "actorIssuer", "dateTime", "message", "values"} {
		if _, ok := line[key]; !ok {
			t.Errorf("missing top-level field %q: %+v", key, line)
		}
	}
	if _, present := line["export"]; present {
		t.Errorf("the flat shape must NOT carry nested export envelope; got %+v", line["export"])
	}
}

func TestSlogPublisher_EventTypeLowercase(t *testing.T) {
	pub, buf := newCapture()
	cases := []struct {
		t    domain.EventType
		want string
	}{
		{domain.EventLog, "log"},
		{domain.EventDebug, "debug"},
		{domain.EventError, "error"},
		{domain.EventWarning, "warning"},
	}
	for _, c := range cases {
		buf.Reset()
		if err := pub.Publish(newCtx(), domain.DomainEvent{Type: c.t, Class: "User", Msg: "x"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		line := extractEvent(t, buf)
		if line["eventType"] != c.want {
			t.Errorf("EventType=%v: eventType wire = %v, want %q (lowercase)", c.t, line["eventType"], c.want)
		}
	}
}

func TestSlogPublisher_LevelMappingPerEventType(t *testing.T) {
	pub, buf := newCapture()
	cases := []struct {
		t    domain.EventType
		want string
	}{
		{domain.EventLog, "INFO"},
		{domain.EventDebug, "DEBUG"},
		{domain.EventError, "ERROR"},
		{domain.EventWarning, "WARN"},
	}
	for _, c := range cases {
		buf.Reset()
		if err := pub.Publish(newCtx(), domain.DomainEvent{Type: c.t, Class: "User", Msg: "x"}); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		line := extractEvent(t, buf)
		if line["level"] != c.want {
			t.Errorf("EventType=%v: slog level = %v, want %v", c.t, line["level"], c.want)
		}
	}
}

func TestSlogPublisher_AnonymousActor(t *testing.T) {
	pub, buf := newCapture()
	if err := pub.Publish(newCtx(), domain.DomainEvent{Type: domain.EventLog, Class: "User", Msg: "x"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	line := extractEvent(t, buf)
	if line["actor"] != "anonymous" {
		t.Errorf("actor = %v, want anonymous", line["actor"])
	}
	if _, present := line["actorIssuer"]; present {
		t.Errorf("actorIssuer must be omitted when empty; got %v", line["actorIssuer"])
	}
}

func TestSlogPublisher_OmitsEmptyEntityType(t *testing.T) {
	pub, buf := newCapture()
	// System-level event: no class bound to an entity.
	if err := pub.Publish(newCtx(), domain.DomainEvent{Type: domain.EventLog, Msg: "system tick"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	line := extractEvent(t, buf)
	if _, present := line["entityType"]; present {
		t.Errorf("entityType must be omitted when ClassName is empty; got %v", line["entityType"])
	}
}

func TestSlogPublisher_PropagatesException(t *testing.T) {
	pub, buf := newCapture()
	ev := domain.DomainEvent{
		Type:   domain.EventError,
		Class:  "User",
		Msg:    "activation failed",
		Reason: errors.New("downstream timeout"),
	}
	if err := pub.Publish(newCtx(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	line := extractEvent(t, buf)
	if line["exception"] != "downstream timeout" {
		t.Errorf("exception = %v, want 'downstream timeout'", line["exception"])
	}
}

func TestSlogPublisher_PublishAll_IteratesAll(t *testing.T) {
	pub, buf := newCapture()
	evts := []domain.DomainEvent{
		{Type: domain.EventLog, Class: "User", Msg: "one"},
		{Type: domain.EventLog, Class: "User", Msg: "two"},
		{Type: domain.EventLog, Class: "User", Msg: "three"},
	}
	if err := pub.PublishAll(newCtx(), evts); err != nil {
		t.Fatalf("PublishAll: %v", err)
	}
	count := strings.Count(buf.String(), `"msg":"event"`)
	if count != 3 {
		t.Errorf("expected 3 event lines, got %d:\n%s", count, buf.String())
	}
}
