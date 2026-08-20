package graphql

import (
	"reflect"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/web/queryschema"
)

// ── fixtures: a read quad whose Response carries a well-known scalar struct
// (time.Time) and a nested slice of structs, so the wire rendering exercises
// wireValueOf's scalar passthrough and its nested-object/array branches ─────

type addrResult struct {
	City *string
}

type richResult struct {
	ID        *string
	CreatedAt *time.Time
	Addresses []addrResult
}

type richQuery struct {
	pipeline.QueryBase
	crit queries.ReadCriteria
}

func (q *richQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.crit, nil
}

func (q *richQuery) FromQueryResult(_ *configuration.AppContext, r richResult) (richResult, error) {
	return r, nil
}

type richRequest struct {
	Name  *string `query:"name" filter:"eq"`
	First *int64  `query:"first"`
}

func (r richRequest) ToQuery(crit queries.ReadCriteria) *richQuery {
	return &richQuery{crit: crit}
}

type addrWire struct {
	City *string `json:"city,omitempty"`
}

type richResponse struct {
	ID        *string    `json:"id,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	Addresses []addrWire `json:"addresses,omitempty"`
}

// FromResult is the wire seat every surface shares — GraphQL renders the
// PROJECTED Response, so a hand-written mapping shows up here exactly as it
// does on REST.
func (richResponse) FromResult(r richResult) richResponse {
	out := richResponse{ID: r.ID, CreatedAt: r.CreatedAt}
	for _, a := range r.Addresses {
		out.Addresses = append(out.Addresses, addrWire{City: a.City})
	}
	return out
}

type fakeRichHandler struct {
	page queries.PageOf[richResult]
}

func (h *fakeRichHandler) Handle(_ *configuration.AppContext, _ *richQuery) (queries.PageOf[richResult], error) {
	return h.page, nil
}

// TestExecute_RendersNestedStructsAndScalars — a node selection of a
// time.Time scalar struct (rendered as its RFC3339 wire form, never walked as
// an object) plus a nested slice of structs (rendered as an array of wire
// objects). The items are typed Results, as the application handler emits.
func TestExecute_RendersNestedStructsAndScalars(t *testing.T) {
	created := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	city := "Cupertino"
	id := "u1"
	h := &fakeRichHandler{page: queries.PageOf[richResult]{
		Items: []richResult{{
			ID:        &id,
			CreatedAt: &created,
			Addresses: []addrResult{{City: &city}},
		}},
		ItemCursors: []string{"c1"},
		TotalCount:  1,
	}}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(QueryWithParams[richRequest]("rich", "Rich", richResponse{}.FromResult, h))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ rich { edges { node { id createdAt addresses { city } } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	node := resp.Data["rich"].(map[string]any)["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if node["createdAt"] == nil {
		t.Error("a time.Time must render as a scalar leaf, not be walked as an object")
	}
	addrs, ok := node["addresses"].([]any)
	if !ok || len(addrs) != 1 {
		t.Fatalf("nested struct slice did not render; got %v", node["addresses"])
	}
	if got := addrs[0].(map[string]any)["city"]; got != "Cupertino" {
		t.Errorf("nested addresses[0].city = %v, want Cupertino", got)
	}
}

// TestExecute_OmitsAbsentSparseFields — a sparse Result leaves the pointer
// nil, the Response elides it (omitempty), and the executor resolves the
// selected-but-absent field to null rather than a zero value.
func TestExecute_OmitsAbsentSparseFields(t *testing.T) {
	city := "London"
	h := &fakeRichHandler{page: queries.PageOf[richResult]{
		Items:       []richResult{{Addresses: []addrResult{{City: &city}}}},
		ItemCursors: []string{"c2"},
		TotalCount:  1,
	}}
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(QueryWithParams[richRequest]("rich", "Rich", richResponse{}.FromResult, h))
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ rich { edges { node { id addresses { city } } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	node := resp.Data["rich"].(map[string]any)["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if v, present := node["id"]; !present || v != nil {
		t.Errorf("an absent sparse field must resolve to null, got %#v (present=%v)", v, present)
	}
	addrs := node["addresses"].([]any)
	if got := addrs[0].(map[string]any)["city"]; got != "London" {
		t.Errorf("nested addresses[0].city = %v, want London", got)
	}
}

// TestWireValueOf_UnmarshalableValueYieldsNil — a value json.Marshal cannot
// encode degrades to a nil map rather than panicking; the executor then
// resolves every selected field to null.
func TestWireValueOf_UnmarshalableValueYieldsNil(t *testing.T) {
	if got := wireValueOf(map[string]any{"bad": make(chan int)}); got != nil {
		t.Errorf("wireValueOf on an unmarshalable value = %#v, want nil", got)
	}
}

// TestExecute_UnsortableOrderByErrors — with the typed orderBy the cut moved
// INTO the schema: an unknown enum value ({field: BOGUS}) and the pre-enum
// string form (["-name"]) are both rejected by gqlparser validation before any
// resolver runs. The resolver's own lookup miss stays as defense in depth,
// exercised directly through buildCriteria.
func TestExecute_UnsortableOrderByErrors(t *testing.T) {
	h := &fakeReadHandler{page: queries.PageOf[execResult]{}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(orderBy: [{field: BOGUS}]) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("an undeclared orderBy enum value must be rejected by validation")
	}
	resp = reg.Execute(ctx, `{ users(orderBy: ["-name"]) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("the pre-enum string form must be rejected by validation")
	}

	// Defense in depth: a value that somehow bypassed validation still errors.
	plan := newCriteriaPlan("User", reflect.TypeOf(execRequest{}), reflect.TypeOf(execResponse{}))
	_, badField, gerr := planRead(plan, map[string]any{
		"orderBy": []any{map[string]any{"field": "BOGUS"}},
	})
	if gerr != nil {
		t.Fatalf("an unknown order field is a typed schema violation, not a prose error: %+v", gerr)
	}
	// The refusal names the ENUM MEMBER the consumer sent — this surface's
	// spelling of REST's `orderBy[<token>]` — so the consumer reads WHICH term
	// was refused instead of "something about orderBy".
	if badField != "orderBy[BOGUS]" {
		t.Fatalf("the read path must report the offending order term, got %q", badField)
	}

	// The enum can name the orderable members but not say each appears at most
	// once, and a duplicated key makes the reader's sort document malformed —
	// so that cut lands here too, on the second occurrence.
	_, badField, gerr = planRead(plan, map[string]any{
		"orderBy": []any{
			map[string]any{"field": "NAME"},
			map[string]any{"field": "AGE"},
			map[string]any{"field": "NAME", "direction": "DESC"},
		},
	})
	if gerr != nil {
		t.Fatalf("a repeated order term is a typed schema violation, not a prose error: %+v", gerr)
	}
	if badField != "orderBy[NAME]" {
		t.Fatalf("a repeated order term must be refused naming the member, got %q", badField)
	}

	// Distinct members in one ordering stay legal.
	crit, badField, gerr := planRead(plan, map[string]any{
		"orderBy": []any{
			map[string]any{"field": "NAME"},
			map[string]any{"field": "AGE", "direction": "DESC"},
		},
	})
	if gerr != nil || badField != "" {
		t.Fatalf("a multi-key ordering over distinct members must pass: %v / %q", gerr, badField)
	}
	if len(crit.OrderBy) != 2 {
		t.Fatalf("both terms must reach the criteria, got %+v", crit.OrderBy)
	}
}

// ── a handler that panics → pipeline.Run recovers → Result.Exception → the
// resolver's internalError() opaque envelope ────────────────────────────────

type panicReadHandler struct{}

func (h *panicReadHandler) Handle(_ *configuration.AppContext, _ *execQuery) (queries.PageOf[execResult], error) {
	panic("boom")
}

// TestExecute_PanicMapsToInternalError — a panicking handler surfaces as the
// opaque Internal error (the panic value stays server-side), parity with the
// REST 500 posture.
func TestExecute_PanicMapsToInternalError(t *testing.T) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(QueryWithParams[execRequest]("users", "User", execResponse{}.FromResult, &panicReadHandler{}))
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

// planRead mirrors the resolver: decode this surface's arguments, then let the
// shared assembler decide what they mean. The vocabulary, direction and
// duplicate rules live there, so a test that only decoded would prove nothing
// about what the consumer actually gets.
func planRead(p *criteriaPlan, args map[string]any) (queries.ReadCriteria, string, *GraphQLError) {
	in, badField, gerr := p.decodeArgs(args)
	if gerr != nil || badField != "" {
		return queries.ReadCriteria{}, badField, gerr
	}
	in.Natural = graphqlNaturalControls
	crit, _, violation, ok := queryschema.BuildCriteria(p.reqSchema, p.projSchema, in)
	if !ok {
		return crit, violation.Field, nil
	}
	return crit, "", nil
}
