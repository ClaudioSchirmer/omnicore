package auditapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// ─── harness ─────────────────────────────────────────────────────────────────

// stubReader records what the handler asked the port for and replays a
// scripted answer.
type stubReader struct {
	events []*audit.AuditEvent
	err    error

	calls          int
	gotEntityType  string
	gotAggregateID string
	gotLimit       int
}

func (s *stubReader) FindByID(context.Context, uuid.UUID) (*audit.AuditEvent, error) {
	return nil, audit.ErrAuditNotFound
}

func (s *stubReader) FindByAggregate(_ context.Context, entityType, aggregateID string, limit int) ([]*audit.AuditEvent, error) {
	s.calls++
	s.gotEntityType, s.gotAggregateID, s.gotLimit = entityType, aggregateID, limit
	return s.events, s.err
}

// mountOn wires the routes onto a bare Fiber app — no bootstrap involved, so
// what is exercised here is this package's own chain.
func mountOn(t *testing.T, cfg Config, reader audit.Reader, registry *openapi.Registry) *fiber.App {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = "/audit"
	}
	if cfg.MaxLimit == 0 {
		cfg.MaxLimit = 20
	}
	app := fiber.New()
	Mount(app, registry, cfg, Deps{
		Pipeline:   pipeline.New(translation.Default()),
		Reader:     reader,
		Translator: translation.Default(),
	})
	return app
}

func get(t *testing.T, app *fiber.App, url string) (int, map[string]any, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil))
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	_ = json.Unmarshal(raw, &envelope)
	return resp.StatusCode, envelope, string(raw)
}

func timelineURL(first string) string {
	u := "/audit/User/" + uuid.NewString()
	if first != "" {
		u += "?first=" + first
	}
	return u
}

func deltaEvent() *audit.AuditEvent {
	return &audit.AuditEvent{
		EntityType: "User",
		EntityID:   "agg-1",
		Verb:       "update",
		Kind:       "delta",
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		Changes:    []audit.FieldChange{{Field: "Name", FieldLabel: "Name"}},
	}
}

// ─── the happy path ──────────────────────────────────────────────────────────

func TestMount_ServesTheTimelineEnvelope(t *testing.T) {
	reader := &stubReader{events: []*audit.AuditEvent{deltaEvent()}}
	app := mountOn(t, Config{RenderLabels: true}, reader, openapi.NewRegistry())

	status, envelope, body := get(t, app, "/audit/User/"+uuid.NewString())
	if status != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if envelope["success"] != true {
		t.Errorf("the canonical success envelope must be used, got %s", body)
	}
	data, ok := envelope["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data must carry the projected timeline, got %v", envelope["data"])
	}
	row := data[0].(map[string]any)
	if row["entityType"] != "User" || row["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("wire row drifted: %v", row)
	}
}

func TestMount_ForwardsThePathSegments(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{}, reader, openapi.NewRegistry())

	aggID := uuid.NewString()
	if status, _, body := get(t, app, "/audit/Employee/"+aggID); status != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", status, body)
	}
	if reader.gotEntityType != "Employee" || reader.gotAggregateID != aggID {
		t.Errorf("segments drifted: entityType=%q aggregateID=%q", reader.gotEntityType, reader.gotAggregateID)
	}
}

func TestMount_EmptyTimelineIsAnEmptyArrayNotNull(t *testing.T) {
	app := mountOn(t, Config{}, &stubReader{}, openapi.NewRegistry())

	_, envelope, body := get(t, app, "/audit/User/"+uuid.NewString())
	data, ok := envelope["data"].([]any)
	if !ok || len(data) != 0 {
		t.Errorf("want an empty array (not null), got %s", body)
	}
}

func TestMount_HonorsTheConfiguredPath(t *testing.T) {
	app := mountOn(t, Config{Path: "/trail"}, &stubReader{}, openapi.NewRegistry())

	if status, _, _ := get(t, app, "/trail/User/"+uuid.NewString()); status != 200 {
		t.Errorf("configured path = %d, want 200", status)
	}
	if status, _, _ := get(t, app, "/audit/User/"+uuid.NewString()); status != 404 {
		t.Errorf("default path = %d, want 404 once overridden", status)
	}
}

// ─── the window control ──────────────────────────────────────────────────────

func TestMount_AbsentFirstSendsTheCeiling(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{MaxLimit: 7}, reader, openapi.NewRegistry())

	get(t, app, "/audit/User/"+uuid.NewString())
	if reader.gotLimit != 7 {
		t.Errorf("limit = %d, want the ceiling 7", reader.gotLimit)
	}
}

func TestMount_FirstIsHonored(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{MaxLimit: 20}, reader, openapi.NewRegistry())

	get(t, app, timelineURL("3"))
	if reader.gotLimit != 3 {
		t.Errorf("limit = %d, want the requested 3", reader.gotLimit)
	}
}

func TestMount_FirstAboveCeilingIs400(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{MaxLimit: 5}, reader, openapi.NewRegistry())

	status, _, body := get(t, app, timelineURL("6"))
	if status != 400 {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if !strings.Contains(body, "LimitExceededNotification") {
		t.Errorf("body must carry LimitExceededNotification, got %s", body)
	}
	if reader.calls != 0 {
		t.Errorf("the read must not reach the port, got %d call(s)", reader.calls)
	}
}

// A non-numeric window never becomes a policy question — it is rejected at the
// wire boundary, where the value fails to be a number at all.
func TestMount_NonNumericFirstIs400(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{}, reader, openapi.NewRegistry())

	status, _, body := get(t, app, timelineURL("many"))
	if status != 400 {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if !strings.Contains(body, "first") {
		t.Errorf("the rejection must name the offending control, got %s", body)
	}
	if reader.calls != 0 {
		t.Errorf("the read must not reach the port, got %d call(s)", reader.calls)
	}
}

func TestMount_NegativeFirstIs400(t *testing.T) {
	app := mountOn(t, Config{}, &stubReader{}, openapi.NewRegistry())

	if status, _, body := get(t, app, timelineURL("-1")); status != 400 {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
}

// ─── binding + failure paths ─────────────────────────────────────────────────

func TestMount_MalformedAggregateIDIs400(t *testing.T) {
	reader := &stubReader{}
	app := mountOn(t, Config{}, reader, openapi.NewRegistry())

	status, _, body := get(t, app, "/audit/User/not-a-uuid")
	if status != 400 {
		t.Fatalf("status = %d, want 400 (body: %s)", status, body)
	}
	if reader.calls != 0 {
		t.Errorf("a malformed id must never reach the port, got %d call(s)", reader.calls)
	}
}

// A transport failure is an exception, not a consumer error: 500 with no
// internals leaked.
func TestMount_ReaderFailureIs500(t *testing.T) {
	app := mountOn(t, Config{}, &stubReader{err: errors.New("conn reset")}, openapi.NewRegistry())

	status, _, body := get(t, app, "/audit/User/"+uuid.NewString())
	if status != 500 {
		t.Fatalf("status = %d, want 500 (body: %s)", status, body)
	}
	if strings.Contains(body, "conn reset") {
		t.Errorf("the underlying cause must not leak to the wire, got %s", body)
	}
}

// ─── label rendering ─────────────────────────────────────────────────────────

func TestMount_RenderLabelsOffKeepsTheRawKey(t *testing.T) {
	reader := &stubReader{events: []*audit.AuditEvent{{
		EntityType: "User",
		Changes:    []audit.FieldChange{{Field: "Name", FieldLabelKey: "RequiredFieldNotification"}},
	}}}
	app := mountOn(t, Config{RenderLabels: false}, reader, openapi.NewRegistry())

	_, _, body := get(t, app, "/audit/User/"+uuid.NewString())
	if !strings.Contains(body, "fieldLabelKey") {
		t.Errorf("the raw key must reach the wire, got %s", body)
	}
}

func TestMount_RenderLabelsOnResolvesTheKey(t *testing.T) {
	reader := &stubReader{events: []*audit.AuditEvent{{
		EntityType: "User",
		Changes:    []audit.FieldChange{{Field: "Name", FieldLabelKey: "RequiredFieldNotification"}},
	}}}
	app := mountOn(t, Config{RenderLabels: true}, reader, openapi.NewRegistry())

	_, _, body := get(t, app, "/audit/User/"+uuid.NewString())
	if strings.Contains(body, "fieldLabelKey") {
		t.Errorf("the raw key must be consumed, got %s", body)
	}
	if !strings.Contains(body, "fieldLabel") {
		t.Errorf("the rendered label must reach the wire, got %s", body)
	}
}

// ─── registration ────────────────────────────────────────────────────────────

func TestMount_RegistersTheOperationInTheSpec(t *testing.T) {
	reg := openapi.NewRegistry()
	mountOn(t, Config{}, &stubReader{}, reg)

	ops := reg.Operations()
	if len(ops) != 1 {
		t.Fatalf("want exactly one registered operation, got %d", len(ops))
	}
	if ops[0].Path != "/audit/:entityType/:aggregateId" {
		t.Errorf("registered path = %q", ops[0].Path)
	}
	if !strings.EqualFold(ops[0].Method, "GET") {
		t.Errorf("registered method = %q, want GET", ops[0].Method)
	}
	if ops[0].Doc.Public {
		t.Error("the audit trail must never be registered as a public route")
	}
}

// A service that opted out of the OpenAPI surface still gets working routes —
// the registry is optional, the routing is not.
func TestMount_NilRegistryStillRoutes(t *testing.T) {
	app := mountOn(t, Config{}, &stubReader{}, nil)

	if status, _, body := get(t, app, "/audit/User/"+uuid.NewString()); status != 200 {
		t.Errorf("status = %d, want 200 with no registry (body: %s)", status, body)
	}
}

// The declared permission reaches the registered operation, which is what the
// boot's authorization scan reads.
func TestMount_DeclaresTheConfiguredPermission(t *testing.T) {
	reg := openapi.NewRegistry()
	openapi.SetGate(func(h fiber.Handler, _ string) fiber.Handler { return h })
	defer openapi.SetGate(nil)

	mountOn(t, Config{Permission: "trail:read"}, &stubReader{}, reg)

	ops := reg.Operations()
	if len(ops) != 1 || ops[0].Spec.RequiredPermission != "trail:read" {
		t.Errorf("the configured permission must reach the operation, got %+v", ops)
	}
}

// An explicitly blank permission mounts ungated — the operator decision the
// boot only refuses when authorization is on.
func TestMount_BlankPermissionMountsUngated(t *testing.T) {
	reg := openapi.NewRegistry()
	mountOn(t, Config{Permission: ""}, &stubReader{}, reg)

	if ops := reg.Operations(); len(ops) != 1 || ops[0].Spec.RequiredPermission != "" {
		t.Errorf("a blank permission must mount no gate, got %+v", ops)
	}
}
