package handlers

import (
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

func TestFindByIDQueryHandler_DelegatesAndPropagatesCriteria(t *testing.T) {
	reader := &spyReader{
		docToReturn: map[string]any{"ID": "abc", "Name": "Jane"},
		docFound:    true,
	}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{includeArchived: true}
	q.SetPathID("abc")

	ctx := testCtx()
	r, err := h.Handle(ctx, q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reader.readByIDCalled != 1 {
		t.Errorf("expected ReadByID called once, got %d", reader.readByIDCalled)
	}
	if reader.gotID != "abc" {
		t.Errorf("expected id 'abc', got %q", reader.gotID)
	}
	if !reader.gotCriteria.IncludeArchived {
		t.Error("expected IncludeArchived=true to propagate via ToCriteria(ctx)")
	}
	if q.gotCtx != ctx {
		t.Error("expected ToCriteria to receive the request ctx")
	}
	if r.ID != "abc" || r.Name != "Jane" {
		t.Errorf("expected the doc filled into the typed Result, got %+v", r)
	}
}

// On a hit the handler must fill the TResult from the doc and pass it through
// the Query's FromQueryResult hook exactly once — the hook's return value is what
// the handler surfaces.
func TestFindByIDQueryHandler_FromQueryInvokedOnHit(t *testing.T) {
	reader := &spyReader{
		docToReturn: map[string]any{"ID": "abc", "Name": "jane"},
		docFound:    true,
	}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{
		mutate: func(r testFindResult) testFindResult {
			r.Name = r.Name + "-derived"
			return r
		},
	}
	q.SetPathID("abc")

	r, err := h.Handle(testCtx(), q)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if q.fromQueryCalls != 1 {
		t.Errorf("expected FromQueryResult called exactly once on a hit, got %d", q.fromQueryCalls)
	}
	if len(q.fromQuerySeen) != 1 || q.fromQuerySeen[0].ID != "abc" || q.fromQuerySeen[0].Name != "jane" {
		t.Errorf("expected FromQueryResult to receive the filled Result, got %+v", q.fromQuerySeen)
	}
	if r.Name != "jane-derived" {
		t.Errorf("expected FromQueryResult's return value surfaced, got %+v", r)
	}
}

func TestFindByIDQueryHandler_FromQueryErrorPropagates(t *testing.T) {
	want := errors.New("from-query boom")
	reader := &spyReader{
		docToReturn: map[string]any{"ID": "abc"},
		docFound:    true,
	}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{fromQueryErr: want}
	q.SetPathID("abc")

	_, err := h.Handle(testCtx(), q)
	if !errors.Is(err, want) {
		t.Errorf("expected FromQueryResult error to propagate, got %v", err)
	}
}

func TestFindByIDQueryHandler_PropagatesOverlayFilter(t *testing.T) {
	reader := &spyReader{
		docToReturn: map[string]any{"ID": "abc"},
		docFound:    true,
	}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{overlay: map[string]any{"tenant_id": "acme"}}
	q.SetPathID("abc")

	if _, err := h.Handle(testCtx(), q); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := reader.gotCriteria.Filter["tenant_id"]; got != "acme" {
		t.Errorf("expected tenant_id overlay to reach the reader, got %v", got)
	}
}

func TestFindByIDQueryHandler_NotFoundProducesDomainError(t *testing.T) {
	reader := &spyReader{docFound: false}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{}
	q.SetPathID("missing")

	_, err := h.Handle(testCtx(), q)
	if err == nil {
		t.Fatal("expected DomainError for not found, got nil")
	}
	if q.fromQueryCalls != 0 {
		t.Errorf("expected FromQueryResult NOT invoked on a miss, got %d calls", q.fromQueryCalls)
	}
	var de *domain.DomainError
	if !errors.As(err, &de) {
		t.Fatalf("expected *domain.DomainError, got %T", err)
	}
	if len(de.Contexts) == 0 {
		t.Fatal("expected at least one NotificationContext")
	}
	msgs := de.Contexts[0].Messages()
	if len(msgs) == 0 {
		t.Fatal("expected at least one message")
	}
	if got := domain.NotificationKey(msgs[0].Notification); got != "RecordNotFoundNotification" {
		t.Errorf("expected RecordNotFoundNotification, got %q", got)
	}
	if got := msgs[0].FieldValue; got != "missing" {
		t.Errorf("expected FieldValue carrying the missing id, got %q", got)
	}
}

func TestFindByIDQueryHandler_ContextNameDefaultsToViewWhenQueryEmpty(t *testing.T) {
	reader := &spyReader{docFound: false}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{} // contextName left empty → fallback to view
	q.SetPathID("missing")

	_, err := h.Handle(testCtx(), q)
	var de *domain.DomainError
	if !errors.As(err, &de) || len(de.Contexts) == 0 {
		t.Fatal("expected DomainError with context")
	}
	if got := de.Contexts[0].Context(); got != "users" {
		t.Errorf("expected NotificationContext to default to view 'users', got %q", got)
	}
}

func TestFindByIDQueryHandler_ContextNameComesFromQuery(t *testing.T) {
	reader := &spyReader{docFound: false}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{contextName: "User"}
	q.SetPathID("missing")

	_, err := h.Handle(testCtx(), q)
	var de *domain.DomainError
	if !errors.As(err, &de) || len(de.Contexts) == 0 {
		t.Fatal("expected DomainError with context")
	}
	if got := de.Contexts[0].Context(); got != "User" {
		t.Errorf("expected NotificationContext from query 'User', got %q", got)
	}
}

func TestFindByIDQueryHandler_PropagatesReaderError(t *testing.T) {
	want := errors.New("mongo timeout")
	reader := &spyReader{docErr: want}
	h := &FindByIDQueryHandler[*testFindIDQuery, testFindResult]{Reader: reader, View: "users"}

	q := &testFindIDQuery{}
	q.SetPathID("abc")

	_, err := h.Handle(testCtx(), q)
	if !errors.Is(err, want) {
		t.Errorf("expected reader error to propagate, got %v", err)
	}
}
