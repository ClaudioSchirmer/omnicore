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

// restrictHandler mirrors the real FindByParamsQueryHandler: it runs
// ToCriteria(ctx) (surfacing the Restrict 403) before "reading".
type restrictHandler struct{ page queries.Page }

func (h *restrictHandler) Handle(ctx *configuration.AppContext, q *restrictQuery) (queries.Page, error) {
	if _, err := q.ToCriteria(ctx); err != nil {
		return queries.Page{}, err
	}
	return h.page, nil
}

func restrictRegistry() *Registry {
	h := &restrictHandler{page: queries.Page{}}
	return New(pipeline.New(translation.Default())).
		Register(Query[restrictRequest, execResponse]("items", "Item", h))
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
	if got := e.Extensions["field"]; got != "Age" {
		t.Errorf("extensions.field = %v, want Age", got)
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
