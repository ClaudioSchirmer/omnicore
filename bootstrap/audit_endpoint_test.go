package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// ─── harness ─────────────────────────────────────────────────────────────────

// stubAuditReader records the coordinates the route resolved and replays a
// scripted timeline, so the whole chain (path binding → window resolution →
// dispatch → projection) is observable without a database.
type stubAuditReader struct {
	events []*appaudit.AuditEvent
	err    error

	gotEntityType  string
	gotAggregateID string
	gotLimit       int
}

func (s *stubAuditReader) FindByID(context.Context, uuid.UUID) (*appaudit.AuditEvent, error) {
	return nil, appaudit.ErrAuditNotFound
}

func (s *stubAuditReader) FindByAggregate(_ context.Context, entityType, aggregateID string, limit int) ([]*appaudit.AuditEvent, error) {
	s.gotEntityType, s.gotAggregateID, s.gotLimit = entityType, aggregateID, limit
	return s.events, s.err
}

// auditDeps assembles the Deps a mounted audit endpoint needs, with the
// supplied REST block already defaulted the way LoadConfigFrom would.
func auditDeps(t *testing.T, reader appaudit.Reader, rest *AuditRESTConfig, maxLimit int, renderLabels bool) Deps {
	t.Helper()
	d := silentDepsWithRegistry()
	d.Translator = translation.Default()
	d.AuditReader = reader
	d.Config.Audit.Destinations = []audit.Destination{audit.DestinationSlog, audit.DestinationDatabase}
	d.Config.Audit.Endpoint = &AuditEndpointConfig{
		MaxLimit:     maxLimit,
		RenderLabels: &renderLabels,
		REST:         rest,
	}
	d.Config.Audit.ApplyDefaults()
	return d
}

// defaultedREST is an empty rest block — every knob at its default.
func defaultedREST() *AuditRESTConfig { return &AuditRESTConfig{} }

func buildAuditApp(t *testing.T, d Deps) *fiber.App {
	t.Helper()
	app, err := buildApp(context.Background(), d, Wiring{
		Features: []Feature{&writeOnlyFeature{}},
		OpenAPI:  &openapi.Config{Title: "T", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	return app
}

// getJSON issues a GET and returns the status plus the decoded envelope.
func getJSON(t *testing.T, app *fiber.App, url string) (int, map[string]any) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest("GET", url, nil))
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	var envelope map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &envelope)
	}
	return resp.StatusCode, envelope
}

func deltaEvent(labelKey string) *appaudit.AuditEvent {
	return &appaudit.AuditEvent{
		EntityType: "User",
		EntityID:   "agg-1",
		Verb:       "update",
		ActionName: "GetUpdatable",
		Kind:       "delta",
		TraceID:    "4bf92f3577b34da6a3ce929d0e0e4736",
		Changes:    []appaudit.FieldChange{{Field: "Name", FieldLabelKey: labelKey}},
	}
}

// ─── mounting ────────────────────────────────────────────────────────────────

// Without the yaml block the framework mounts nothing — the posture every
// service that predates the endpoint keeps.
func TestBuildApp_AuditEndpointAbsent_NoRoute(t *testing.T) {
	d := silentDepsWithRegistry()
	d.AuditReader = &stubAuditReader{}
	app := buildAuditApp(t, d)

	status, _ := getJSON(t, app, "/audit/User/"+uuid.NewString())
	if status != 404 {
		t.Errorf("GET /audit/... = %d, want 404 with no audit.endpoint block", status)
	}
}

func TestBuildApp_AuditEndpointMounted_ServesTheTimeline(t *testing.T) {
	reader := &stubAuditReader{events: []*appaudit.AuditEvent{deltaEvent("")}}
	aggID := uuid.NewString()
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 20, true))

	status, envelope := getJSON(t, app, "/audit/User/"+aggID)
	if status != 200 {
		t.Fatalf("GET the timeline = %d, want 200 (envelope: %v)", status, envelope)
	}
	if reader.gotEntityType != "User" || reader.gotAggregateID != aggID {
		t.Errorf("path segments did not reach the reader: entityType=%q aggregateID=%q",
			reader.gotEntityType, reader.gotAggregateID)
	}
	data, ok := envelope["data"].([]any)
	if !ok || len(data) != 1 {
		t.Fatalf("data must carry the timeline, got %v", envelope["data"])
	}
	row, _ := data[0].(map[string]any)
	if row["entityType"] != "User" || row["verb"] != "update" {
		t.Errorf("wire row drifted: %v", row)
	}
	// The pivot column reaches the wire — the read path stopped dropping it.
	if row["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("traceId missing from the wire row: %v", row["traceId"])
	}
}

// An aggregate with no rows is a legitimate state: 200 with an empty array,
// never a 404 (a 404 would claim the AGGREGATE does not exist).
func TestBuildApp_AuditEndpoint_EmptyTimelineIs200(t *testing.T) {
	app := buildAuditApp(t, auditDeps(t, &stubAuditReader{}, defaultedREST(), 20, true))

	status, envelope := getJSON(t, app, "/audit/User/"+uuid.NewString())
	if status != 200 {
		t.Fatalf("empty timeline = %d, want 200", status)
	}
	if data, ok := envelope["data"].([]any); !ok || len(data) != 0 {
		t.Errorf("want an empty array, got %v", envelope["data"])
	}
}

func TestBuildApp_AuditEndpoint_HonorsTheConfiguredPath(t *testing.T) {
	app := buildAuditApp(t, auditDeps(t, &stubAuditReader{}, &AuditRESTConfig{Path: "/trail"}, 20, true))

	if status, _ := getJSON(t, app, "/trail/User/"+uuid.NewString()); status != 200 {
		t.Errorf("GET the configured path = %d, want 200", status)
	}
	if status, _ := getJSON(t, app, "/audit/User/"+uuid.NewString()); status != 404 {
		t.Errorf("GET the default path = %d, want 404 once the path was overridden", status)
	}
}

// ─── the window ──────────────────────────────────────────────────────────────

func TestBuildApp_AuditEndpoint_AbsentFirstSendsTheCeiling(t *testing.T) {
	reader := &stubAuditReader{}
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 7, true))

	if status, _ := getJSON(t, app, "/audit/User/"+uuid.NewString()); status != 200 {
		t.Fatalf("want 200, got %d", status)
	}
	if reader.gotLimit != 7 {
		t.Errorf("limit reaching the reader = %d, want the configured ceiling 7", reader.gotLimit)
	}
}

func TestBuildApp_AuditEndpoint_FirstIsHonored(t *testing.T) {
	reader := &stubAuditReader{}
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 20, true))

	if status, _ := getJSON(t, app, "/audit/User/"+uuid.NewString()+"?first=3"); status != 200 {
		t.Fatalf("want 200, got %d", status)
	}
	if reader.gotLimit != 3 {
		t.Errorf("limit reaching the reader = %d, want the requested 3", reader.gotLimit)
	}
}

func TestBuildApp_AuditEndpoint_FirstAboveCeilingIs400(t *testing.T) {
	app := buildAuditApp(t, auditDeps(t, &stubAuditReader{}, defaultedREST(), 5, true))

	status, envelope := getJSON(t, app, "/audit/User/"+uuid.NewString()+"?first=6")
	if status != 400 {
		t.Fatalf("over-ceiling window = %d, want 400 (envelope: %v)", status, envelope)
	}
	if !strings.Contains(flatten(envelope), "LimitExceededNotification") {
		t.Errorf("envelope must carry LimitExceededNotification, got %v", envelope)
	}
}

func TestBuildApp_AuditEndpoint_NonNumericFirstIs400(t *testing.T) {
	app := buildAuditApp(t, auditDeps(t, &stubAuditReader{}, defaultedREST(), 20, true))

	status, envelope := getJSON(t, app, "/audit/User/"+uuid.NewString()+"?first=many")
	if status != 400 {
		t.Fatalf("non-numeric window = %d, want 400 (envelope: %v)", status, envelope)
	}
}

// ─── binding + rendering ─────────────────────────────────────────────────────

// A malformed aggregate id is rejected at the wire boundary, so a driver never
// sees it and the consumer gets a 400 instead of a 500.
func TestBuildApp_AuditEndpoint_MalformedAggregateIDIs400(t *testing.T) {
	reader := &stubAuditReader{}
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 20, true))

	status, _ := getJSON(t, app, "/audit/User/not-a-uuid")
	if status != 400 {
		t.Fatalf("malformed aggregate id = %d, want 400", status)
	}
	if reader.gotAggregateID != "" {
		t.Errorf("the read must never reach the reader, got %q", reader.gotAggregateID)
	}
}

func TestBuildApp_AuditEndpoint_RenderLabelsOffKeepsTheRawKey(t *testing.T) {
	reader := &stubAuditReader{events: []*appaudit.AuditEvent{deltaEvent("RequiredFieldNotification")}}
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 20, false))

	_, envelope := getJSON(t, app, "/audit/User/"+uuid.NewString())
	body := flatten(envelope)
	if !strings.Contains(body, "fieldLabelKey") || !strings.Contains(body, "RequiredFieldNotification") {
		t.Errorf("renderLabels=false must keep the raw catalog key, got %s", body)
	}
}

func TestBuildApp_AuditEndpoint_RenderLabelsOnResolvesTheKey(t *testing.T) {
	reader := &stubAuditReader{events: []*appaudit.AuditEvent{deltaEvent("RequiredFieldNotification")}}
	app := buildAuditApp(t, auditDeps(t, reader, defaultedREST(), 20, true))

	_, envelope := getJSON(t, app, "/audit/User/"+uuid.NewString())
	body := flatten(envelope)
	if strings.Contains(body, "fieldLabelKey") {
		t.Errorf("renderLabels=true must consume the raw key, got %s", body)
	}
	if !strings.Contains(body, "fieldLabel") {
		t.Errorf("renderLabels=true must emit the rendered label, got %s", body)
	}
}

// ─── the spec surface ────────────────────────────────────────────────────────

// The route is registered through openapi.Mount like any service route, so it
// is documented AND it satisfies the boot's route-registration scan (which
// buildApp above already ran — a route registered outside Mount would have
// panicked before this assertion).
func TestBuildApp_AuditEndpoint_AppearsInTheSpec(t *testing.T) {
	app := buildAuditApp(t, auditDeps(t, &stubAuditReader{}, defaultedREST(), 20, true))

	resp, err := app.Test(httptest.NewRequest("GET", "/openapi.json", nil))
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	spec := string(raw)
	if !strings.Contains(spec, "/audit/{entityType}/{aggregateId}") {
		t.Errorf("the audit route must be documented, spec:\n%s", spec)
	}
	// The window control is documented as OPTIONAL — a required `first` would
	// contradict the "absent means one full window" contract.
	if !strings.Contains(spec, `"name":"first"`) && !strings.Contains(spec, `"name": "first"`) {
		t.Errorf("the first control must appear as a query parameter, spec:\n%s", spec)
	}
}

// flatten renders a decoded envelope back to JSON for substring assertions.
func flatten(envelope map[string]any) string {
	raw, _ := json.Marshal(envelope)
	return string(raw)
}
