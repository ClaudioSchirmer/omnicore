package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
)

// ── fixture: a read whose ToCriteria restricts the `Age` field for non-admins ──

type restrictRequest struct {
	Name *string `query:"name" filter:"eq"`
}

func (r restrictRequest) ToQuery(crit queries.ReadCriteria) *restrictQuery {
	return &restrictQuery{crit: crit}
}

type restrictQuery struct {
	pipeline.QueryBase
	crit queries.ReadCriteria
}

func (q *restrictQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	crit := q.crit
	if id := ctx.Identity(); id == nil || !id.HasPermission("data:admin") {
		if err := crit.Restrict("Age"); err != nil {
			return crit, err
		}
	}
	return crit, nil
}

func (q *restrictQuery) FromQueryResult(_ *configuration.AppContext, r execResult) (execResult, error) {
	return r, nil
}

// restrictHandler mirrors the real FindByParamsQueryHandler: it runs
// ToCriteria(ctx) (surfacing the Restrict 403) before "reading".
type restrictHandler struct{ page queries.PageOf[execResult] }

func (h *restrictHandler) Handle(ctx *configuration.AppContext, q *restrictQuery) (queries.PageOf[execResult], error) {
	if _, err := q.ToCriteria(ctx); err != nil {
		return queries.PageOf[execResult]{}, err
	}
	return h.page, nil
}

func restrictRegistry() *Registry {
	h := &restrictHandler{}
	return New(pipeline.New(translation.Default())).
		Register(QueryWithParams[restrictRequest]("items", "Item", execResponse{}.FromResult, h))
}

// TestFieldAccess_ExplicitSelectionOfRestrictedFieldIsForbidden — selecting a
// restricted field in the GraphQL selection set now maps to Projection, so
// ReadCriteria.Restrict's active-reference branch fires: the field resolves to a
// FieldAccessForbiddenNotification (semantic Forbidden) instead of silently
// omitting it. This is the GraphQL parity with the REST ?fields=phone → 403.
func TestFieldAccess_ExplicitSelectionOfRestrictedFieldIsForbidden(t *testing.T) {
	reg := restrictRegistry()
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG) // non-admin (nil identity)
	resp := reg.Execute(ctx, `query { items { edges { node { name age } } } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("explicitly selecting a restricted field must be forbidden")
	}
	e := resp.Errors[0]
	if got := e.Extensions["semantic"]; got != "Forbidden" {
		t.Errorf("extensions.semantic = %v, want Forbidden", got)
	}
	if got := e.Extensions["notificationKey"]; got != "FieldAccessForbiddenNotification" {
		t.Errorf("extensions.notificationKey = %v, want FieldAccessForbiddenNotification", got)
	}
	if got := e.Extensions["field"]; got != "age" {
		t.Errorf("extensions.field = %v, want age", got)
	}
}

// TestFieldAccess_PassiveOmissionResolves — NOT selecting the restricted field
// resolves normally (the field is scrubbed, never a 403). Confirms the active
// branch is keyed on explicit selection, not blanket rejection.
func TestFieldAccess_PassiveOmissionResolves(t *testing.T) {
	reg := restrictRegistry()
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	resp := reg.Execute(ctx, `query { items { edges { node { name } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("not selecting the restricted field must resolve; got %+v", resp.Errors)
	}
}

// TestFieldAccess_AdminSelectionResolves — a principal carrying the gating
// permission selects the field freely (ToCriteria does not restrict it).
func TestFieldAccess_AdminSelectionResolves(t *testing.T) {
	reg := restrictRegistry()
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{
		Claims: map[string]any{"permissions": []string{"data:admin"}},
	})
	resp := reg.Execute(ctx, `query { items { edges { node { name age } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("admin selecting the field must resolve; got %+v", resp.Errors)
	}
}

// ── the same rule, on the singular field ─────────────────────────────────────

// A by-id read and its listing twin answer ONE question about a restricted
// field, and the answer is the Query's — never the field the consumer happened
// to call. The fixtures below are the by-id half of the pair above: the same
// ToCriteria, the same Restrict("Age"), reached through `item(id:)`.

type restrictByIDRequest struct{}

func (restrictByIDRequest) ToQuery(crit queries.ReadCriteria) *restrictByIDQuery {
	return &restrictByIDQuery{crit: crit}
}

type restrictByIDQuery struct {
	queries.QueryByIDBase
	crit queries.ReadCriteria
}

func (q *restrictByIDQuery) ToCriteria(ctx *configuration.AppContext) (queries.ReadCriteria, error) {
	crit := q.crit
	if id := ctx.Identity(); id == nil || !id.HasPermission("data:admin") {
		if err := crit.Restrict("Age"); err != nil {
			return crit, err
		}
	}
	return crit, nil
}

func (q *restrictByIDQuery) FromQueryResult(_ *configuration.AppContext, r execResult) (execResult, error) {
	return r, nil
}

func (q *restrictByIDQuery) ContextName() string { return "Item" }

// restrictByIDHandler mirrors the real FindByIDQueryHandler: ToCriteria(ctx)
// runs (surfacing the Restrict 403) before the "read", and the criteria it
// received is captured so a test can assert what reached the reader.
type restrictByIDHandler struct{ saw queries.ReadCriteria }

func (h *restrictByIDHandler) Handle(ctx *configuration.AppContext, q *restrictByIDQuery) (execResult, error) {
	crit, err := q.ToCriteria(ctx)
	if err != nil {
		return execResult{}, err
	}
	h.saw = crit
	return execResult{ID: sp("11111111-1111-4111-8111-111111111111"), Name: sp("Ana")}, nil
}

func restrictByIDRegistry(h *restrictByIDHandler) *Registry {
	return New(pipeline.New(translation.Default())).
		Register(QueryWithParams[restrictRequest]("items", "Item", execResponse{}.FromResult, &restrictHandler{})).
		Register(QueryByID[restrictByIDRequest]("item", "Item", execResponse{}.FromResult, h))
}

// TestFieldAccess_ByIDExplicitSelectionOfRestrictedFieldIsForbidden — the
// singular field must refuse what the listing refuses. Without a projection
// derived from its selection set, Restrict saw no active reference here and
// scrubbed the field in silence: the same query, two verdicts, decided by which
// field the consumer called.
func TestFieldAccess_ByIDExplicitSelectionOfRestrictedFieldIsForbidden(t *testing.T) {
	reg := restrictByIDRegistry(&restrictByIDHandler{})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG) // non-admin
	resp := reg.Execute(ctx, `query { item(id: "11111111-1111-4111-8111-111111111111") { name age } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("explicitly selecting a restricted field on the by-id field must be forbidden")
	}
	e := resp.Errors[0]
	if got := e.Extensions["semantic"]; got != "Forbidden" {
		t.Errorf("extensions.semantic = %v, want Forbidden", got)
	}
	if got := e.Extensions["notificationKey"]; got != "FieldAccessForbiddenNotification" {
		t.Errorf("extensions.notificationKey = %v, want FieldAccessForbiddenNotification", got)
	}
	if got := e.Extensions["field"]; got != "age" {
		t.Errorf("extensions.field = %v, want age", got)
	}
}

// TestFieldAccess_ByIDPassiveOmissionResolves — the other half of the rule,
// unchanged: not naming the field is a passive read and resolves.
func TestFieldAccess_ByIDPassiveOmissionResolves(t *testing.T) {
	reg := restrictByIDRegistry(&restrictByIDHandler{})
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	resp := reg.Execute(ctx, `query { item(id: "11111111-1111-4111-8111-111111111111") { name } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("not selecting the restricted field must resolve; got %+v", resp.Errors)
	}
}

// TestFieldAccess_ByIDSelectionReachesTheReaderAsAProjection — the pushdown
// half. Both read backings honor ReadCriteria.Projection on a by-id route, so
// the selection is what the store is asked for, not merely what the executor
// trims afterwards.
func TestFieldAccess_ByIDSelectionReachesTheReaderAsAProjection(t *testing.T) {
	h := &restrictByIDHandler{}
	reg := restrictByIDRegistry(h)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	ctx.SetIdentity(&configuration.Identity{
		Claims: map[string]any{"permissions": []string{"data:admin"}},
	})
	if resp := reg.Execute(ctx, `query { item(id: "11111111-1111-4111-8111-111111111111") { name } }`, nil, ""); len(resp.Errors) != 0 {
		t.Fatalf("admin read must resolve; got %+v", resp.Errors)
	}
	if !h.saw.Projection.Selects("Name") {
		t.Errorf("the selected field must reach the reader as a projection; got %v", h.saw.Projection)
	}
	if h.saw.Projection.Selects("Age") {
		t.Errorf("an unselected field must not be projected; got %v", h.saw.Projection)
	}
}
