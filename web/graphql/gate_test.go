package graphql

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// bareGateRequest declares ONLY filter leaves — no reserved control keys.
// The DTO is the single source of truth: on this surface the cut lands in
// the SCHEMA itself, so undeclared args are absent from the SDL and
// gqlparser rejects them as unknown arguments before any resolver runs.
type bareGateRequest struct {
	Name *string `query:"name" filter:"eq,startswith"`
}

func (r bareGateRequest) ToQuery(crit queries.ReadCriteria) *execQuery {
	return &execQuery{crit: crit}
}

func newBareRegistry(h *fakeReadHandler) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		QueryWithParams[bareGateRequest, execResponse]("users", "User", h),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// TestGate_SDLOmitsUndeclaredArgs proves the port-level cut: the generated
// SDL carries only the args the Request DTO declares (the `where:` input
// still follows the filter tags).
func TestGate_SDLOmitsUndeclaredArgs(t *testing.T) {
	reg, _ := newBareRegistry(&fakeReadHandler{})
	sdl, err := reg.SDL()
	if err != nil {
		t.Fatalf("build schema: %v", err)
	}
	line := ""
	for _, l := range strings.Split(sdl, "\n") {
		if strings.Contains(l, "users(") || strings.Contains(l, "users:") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("users field not found in SDL:\n%s", reg.sdl)
	}
	for _, forbidden := range []string{"first", "last", "after", "before", "orderBy", "search", "includeArchived"} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("SDL must not advertise undeclared arg %q: %s", forbidden, line)
		}
	}
	if !strings.Contains(line, "where:") {
		t.Fatalf("where input (filter tags) must survive the cut: %s", line)
	}
}

// TestGate_UndeclaredArgRejectedByValidation proves the enforcement that
// comes with the cut: an arg absent from the schema is rejected by
// gqlparser as an unknown argument — the GraphQL self-translation of the
// canonical NotDeclared violation.
func TestGate_UndeclaredArgRejectedByValidation(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{}}
	reg, ctx := newBareRegistry(h)
	resp := reg.Execute(ctx, `query { users(first: 5) { totalCount } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("undeclared arg must be rejected before the resolver")
	}
	if !strings.Contains(resp.Errors[0].Message, "first") {
		t.Fatalf("rejection must name the arg: %+v", resp.Errors[0])
	}
	if h.captured.Limit != 0 {
		t.Fatal("resolver must not have run")
	}
}

// TestGate_OnlyTotalOptInGovernsShortCircuit proves the only-total posture:
// the selection shape is always valid, but the count short-circuit
// (ReadCriteria.OnlyTotal) engages only when the DTO declares
// `query:"onlyTotal"`. Without the opt-in the same selection runs the
// un-optimized paged read and still serves totalCount.
func TestGate_OnlyTotalOptInGovernsShortCircuit(t *testing.T) {
	// Opted-in DTO (execRequest declares onlyTotal).
	optIn := &fakeReadHandler{page: queries.Page{OnlyTotal: true, TotalCount: 9}}
	reg, ctx := newExecRegistry(optIn)
	resp := reg.Execute(ctx, `query { users { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	if !optIn.captured.OnlyTotal {
		t.Fatal("opted-in DTO must engage the only-total short-circuit")
	}

	// Bare DTO: same selection, no short-circuit, still valid.
	bare := &fakeReadHandler{page: queries.Page{TotalCount: 9}}
	regBare, ctxBare := newBareRegistry(bare)
	respBare := regBare.Execute(ctxBare, `query { users { totalCount } }`, nil, "")
	if len(respBare.Errors) != 0 {
		t.Fatalf("selection must stay valid without the opt-in: %+v", respBare.Errors)
	}
	if bare.captured.OnlyTotal {
		t.Fatal("without the opt-in the read must run un-optimized (OnlyTotal=false)")
	}
}

// TestGate_PageInfoProbeNarrowsProjection proves the pagination probe: a
// pageInfo-without-edges selection narrows the read to the keyset
// essentials (ordering fields; bare probe degenerates to {_id: 1}) so the
// reader materializes only what cursors and the beyond-edge flags need.
func TestGate_PageInfoProbeNarrowsProjection(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		HasNextPage: true, StartCursor: "a", EndCursor: "b", TotalCount: 4,
	}}
	reg, ctx := newExecRegistry(h)

	// Unordered probe → {_id: 1}.
	resp := reg.Execute(ctx, `query { users { pageInfo { hasNextPage endCursor } totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("probe must pass: %+v", resp.Errors)
	}
	if got := h.captured.Projection; len(got) != 1 || got["_id"] != 1 {
		t.Fatalf("bare probe must project only _id, got %v", got)
	}

	// Ordered probe → the ordering fields.
	resp = reg.Execute(ctx, `query { users(orderBy: [{field: NAME, direction: DESC}]) { pageInfo { hasNextPage } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("ordered probe must pass: %+v", resp.Errors)
	}
	if got := h.captured.Projection; len(got) != 1 || got["Name"] != 1 {
		t.Fatalf("ordered probe must project the ordering fields, got %v", got)
	}

	// A node selection keeps the full projection path (no probe).
	resp = reg.Execute(ctx, `query { users { edges { node { name } } pageInfo { hasNextPage } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("edges+pageInfo must pass: %+v", resp.Errors)
	}
	if got := h.captured.Projection; got["Name"] != 1 {
		t.Fatalf("node selection must drive the projection, got %v", got)
	}
}
