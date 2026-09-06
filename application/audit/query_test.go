package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ─── fake reader ─────────────────────────────────────────────────────────────

// fakeReader records what the handler asked for and replays a scripted answer,
// so the window cascade is observable at the port boundary — the only place
// the decision becomes visible to the database.
type fakeReader struct {
	events []*AuditEvent
	err    error

	calls          int
	gotEntityType  string
	gotAggregateID string
	gotLimit       int
}

func (f *fakeReader) FindByID(context.Context, uuid.UUID) (*AuditEvent, error) {
	panic("not used by the timeline handler")
}

func (f *fakeReader) FindByAggregate(_ context.Context, entityType, aggregateID string, limit int) ([]*AuditEvent, error) {
	f.calls++
	f.gotEntityType, f.gotAggregateID, f.gotLimit = entityType, aggregateID, limit
	return f.events, f.err
}

func testCtx() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func changeEvent(labelKey string) *AuditEvent {
	return &AuditEvent{
		EntityType: "User",
		Verb:       "update",
		Kind:       "delta",
		Changes:    []FieldChange{{Field: "Name", FieldLabelKey: labelKey}},
	}
}

func handlerOver(r Reader, renderLabels bool) *FindByAggregateQueryHandler {
	return &FindByAggregateQueryHandler{
		Reader:       r,
		Translator:   translation.New(),
		RenderLabels: renderLabels,
	}
}

func query(first, max int) *FindByAggregateQuery {
	return &FindByAggregateQuery{EntityType: "User", AggregateID: "agg-1", First: first, MaxLimit: max}
}

// ─── the window cascade ──────────────────────────────────────────────────────

// An absent window means "one full window": the ceiling reaches the reader, so
// the statement it builds always carries a bound.
func TestHandle_AbsentFirstUsesTheCeiling(t *testing.T) {
	r := &fakeReader{}
	if _, err := handlerOver(r, false).Handle(testCtx(), query(0, 20)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r.gotLimit != 20 {
		t.Errorf("limit reaching the reader = %d, want the ceiling 20", r.gotLimit)
	}
}

func TestHandle_FirstBelowCeilingIsHonored(t *testing.T) {
	r := &fakeReader{}
	if _, err := handlerOver(r, false).Handle(testCtx(), query(5, 20)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r.gotLimit != 5 {
		t.Errorf("limit reaching the reader = %d, want the requested 5", r.gotLimit)
	}
}

// A window above the ceiling is REFUSED, never silently clamped: a consumer
// learning about the ceiling from a short array cannot tell truncation from
// "that is all there is".
func TestHandle_FirstAboveCeilingIsRefusedNotClamped(t *testing.T) {
	r := &fakeReader{}
	_, err := handlerOver(r, false).Handle(testCtx(), query(21, 20))
	if err == nil {
		t.Fatal("expected the over-ceiling window to be refused")
	}
	if r.calls != 0 {
		t.Errorf("the read must not reach the database, got %d call(s)", r.calls)
	}
	msg := singleMessage(t, err)
	if _, ok := msg.Notification.(domain.LimitExceededNotification); !ok {
		t.Errorf("notification = %T, want LimitExceededNotification", msg.Notification)
	}
	if msg.ResolveFieldName() != "first" {
		t.Errorf("FieldName = %q, want the wire control %q", msg.ResolveFieldName(), "first")
	}
	// The effective ceiling rides the envelope so a consumer renders "max is N"
	// without parsing a translated message — the shape the view-side rejection
	// already produces.
	if msg.FieldValue != "20" {
		t.Errorf("FieldValue = %q, want the effective ceiling %q", msg.FieldValue, "20")
	}
}

func TestHandle_NegativeFirstIsASchemaViolation(t *testing.T) {
	r := &fakeReader{}
	_, err := handlerOver(r, false).Handle(testCtx(), query(-1, 20))
	if err == nil {
		t.Fatal("expected a negative window to be refused")
	}
	if r.calls != 0 {
		t.Errorf("the read must not reach the database, got %d call(s)", r.calls)
	}
	if _, ok := singleMessage(t, err).Notification.(domain.SchemaViolationNotification); !ok {
		t.Errorf("notification = %T, want SchemaViolationNotification", singleMessage(t, err).Notification)
	}
}

// A non-positive ceiling means the transport handed over an unresolved
// configuration — a programming error, not a consumer one, so it surfaces as a
// plain error (→ 500) rather than a typed notification (→ 4xx).
func TestHandle_NonPositiveCeilingIsAnInternalError(t *testing.T) {
	r := &fakeReader{}
	_, err := handlerOver(r, false).Handle(testCtx(), query(0, 0))
	if err == nil {
		t.Fatal("expected a non-positive ceiling to fail")
	}
	var carrier domain.NotificationCarrier
	if errors.As(err, &carrier) {
		t.Errorf("a misconfiguration must not read as a consumer-facing notification, got %v", err)
	}
}

// ─── pass-through + label rendering ──────────────────────────────────────────

func TestHandle_ForwardsTheAggregateCoordinates(t *testing.T) {
	r := &fakeReader{}
	if _, err := handlerOver(r, false).Handle(testCtx(), query(0, 20)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if r.gotEntityType != "User" || r.gotAggregateID != "agg-1" {
		t.Errorf("coordinates drifted: entityType=%q aggregateID=%q", r.gotEntityType, r.gotAggregateID)
	}
}

func TestHandle_EmptyTimelineIsNotAnError(t *testing.T) {
	out, err := handlerOver(&fakeReader{}, false).Handle(testCtx(), query(0, 20))
	if err != nil {
		t.Fatalf("an aggregate with no audit rows is a legitimate state, got %v", err)
	}
	if len(out) != 0 {
		t.Errorf("want an empty timeline, got %d event(s)", len(out))
	}
}

func TestHandle_ReaderFailureIsPropagated(t *testing.T) {
	boom := errors.New("conn reset")
	_, err := handlerOver(&fakeReader{err: boom}, false).Handle(testCtx(), query(0, 20))
	if !errors.Is(err, boom) {
		t.Errorf("reader failure must propagate, got %v", err)
	}
}

// With rendering on, the raw catalog key is consumed and the translated label
// takes its place — the shape a human-facing surface wants.
func TestHandle_RenderLabelsOnResolvesTheCatalogKey(t *testing.T) {
	r := &fakeReader{events: []*AuditEvent{changeEvent("RequiredFieldNotification")}}
	out, err := handlerOver(r, true).Handle(testCtx(), query(0, 20))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := out[0].Changes[0]
	if got.FieldLabelKey != "" {
		t.Errorf("the raw key must be consumed, got %q", got.FieldLabelKey)
	}
	if got.FieldLabel == "" {
		t.Error("the rendered label must be populated")
	}
}

// With rendering off, the stable key survives untouched — the shape a machine
// consumer wants, and the reason the knob exists.
func TestHandle_RenderLabelsOffKeepsTheRawKey(t *testing.T) {
	r := &fakeReader{events: []*AuditEvent{changeEvent("RequiredFieldNotification")}}
	out, err := handlerOver(r, false).Handle(testCtx(), query(0, 20))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := out[0].Changes[0]
	if got.FieldLabelKey != "RequiredFieldNotification" {
		t.Errorf("the raw key must survive, got %q", got.FieldLabelKey)
	}
	if got.FieldLabel != "" {
		t.Errorf("no label may be rendered, got %q", got.FieldLabel)
	}
}

func TestHandle_NilReaderFailsFast(t *testing.T) {
	h := &FindByAggregateQueryHandler{}
	if _, err := h.Handle(testCtx(), query(0, 20)); err == nil {
		t.Fatal("expected a nil reader to fail fast")
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// singleMessage unwraps the one notification message a rejection carries.
func singleMessage(t *testing.T, err error) domain.NotificationMessage {
	t.Helper()
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("error does not carry notifications: %v", err)
	}
	ctxs := carrier.NotificationContexts()
	if len(ctxs) != 1 || len(ctxs[0].Messages()) != 1 {
		t.Fatalf("want exactly one context with one message, got %+v", ctxs)
	}
	return ctxs[0].Messages()[0]
}
