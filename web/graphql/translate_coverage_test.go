package graphql

import (
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// ── fixtures: a Response carrying a well-known scalar struct (time.Time) and a
// nested slice of structs, so the wire translation exercises translateValue's
// struct/slice branches, translateSlice, and scalarStruct ──────────────────

type addrWire struct {
	City *string `json:"city,omitempty"`
}

type richResponse struct {
	ID        *string    `json:"id,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Addresses []addrWire `json:"addresses,omitempty"`
}

// TestExecute_TranslatesNestedStructsAndScalars — a node selection of a
// time.Time scalar struct (passthrough via scalarStruct) plus a nested slice of
// structs (translateSlice → translateToWire recursion). The doc is Go-field
// keyed, as the reader emits it.
func TestExecute_TranslatesNestedStructsAndScalars(t *testing.T) {
	created := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	h := &fakeReadHandler{page: queries.Page{
		Items: []map[string]any{{
			"ID":        "u1",
			"CreatedAt": created,
			"Addresses": []map[string]any{{"City": "Cupertino"}},
		}},
		ItemCursors: []string{"c1"},
		Total:       1,
	}}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(Query[execRequest, richResponse]("rich", "Rich", h))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ rich { edges { node { id createdAt addresses { city } } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	node := resp.Data["rich"].(map[string]any)["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if node["createdAt"] == nil {
		t.Error("time.Time scalar struct must pass through to the wire (scalarStruct)")
	}
	addrs, ok := node["addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("nested struct slice did not translate; got %v", node["addresses"])
	}
	if city := addrs[0].(map[string]any)["city"]; city != "Cupertino" {
		t.Errorf("nested addresses[0].city = %v, want Cupertino", city)
	}
}

// TestExecute_TranslatesAnySlice — Mongo also decodes a nested array as []any
// (of maps), not just []map[string]any; translateSlice must reshape that shape
// too.
func TestExecute_TranslatesAnySlice(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		Items: []map[string]any{{
			"ID":        "u2",
			"Addresses": []any{map[string]any{"City": "London"}},
		}},
		ItemCursors: []string{"c2"},
		Total:       1,
	}}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(Query[execRequest, richResponse]("rich", "Rich", h))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ rich { edges { node { addresses { city } } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	node := resp.Data["rich"].(map[string]any)["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	addrs, ok := node["addresses"].([]any)
	if !ok || len(addrs) != 1 || addrs[0].(map[string]any)["city"] != "London" {
		t.Errorf("[]any nested slice did not translate; got %v", node["addresses"])
	}
}

// TestExecute_UnsortableOrderByErrors — an orderBy token that is not a sortable
// field passes gqlparser (orderBy is [String!]) but fails ParseSortWithSchema,
// so buildCriteria returns a single executor-level error via errf.
func TestExecute_UnsortableOrderByErrors(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(orderBy: ["bogus"]) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("an unsortable orderBy field must surface an error")
	}
	if !strings.Contains(resp.Errors[0].Message, "orderBy") {
		t.Errorf("error message = %q, want it to mention orderBy", resp.Errors[0].Message)
	}
	if h.captured.OnlyTotal {
		t.Error("orderBy with an edges selection must not be count-only")
	}
}

// ── a handler that panics → pipeline.Run recovers → Result.Exception → the
// resolver's internalError() opaque envelope ────────────────────────────────

type panicReadHandler struct{}

func (h *panicReadHandler) Handle(_ *configuration.AppContext, _ *execQuery) (queries.Page, error) {
	panic("boom")
}

// TestExecute_PanicMapsToInternalError — a panicking handler surfaces as the
// opaque Internal error (the panic value stays server-side), parity with the
// REST 500 posture.
func TestExecute_PanicMapsToInternalError(t *testing.T) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(Query[execRequest, execResponse]("users", "User", &panicReadHandler{}))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ users { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a panicking handler must surface an error")
	}
	if got := resp.Errors[0].Extensions["semantic"]; got != "Internal" {
		t.Errorf("extensions.semantic = %v, want Internal", got)
	}
	if resp.Errors[0].Message != "internal server error" {
		t.Errorf("message = %q, want the opaque internal message", resp.Errors[0].Message)
	}
}
