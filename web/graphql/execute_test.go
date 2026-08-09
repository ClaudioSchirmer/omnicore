package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ── fixtures: a read endpoint quad (Request / Query / Response / handler) ────

type execQuery struct {
	pipeline.QueryBase
	crit queries.ReadCriteria
}

func (q *execQuery) ToCriteria(_ *configuration.AppContext) (queries.ReadCriteria, error) {
	return q.crit, nil
}

type execRequest struct {
	Name  *string `query:"name" filter:"eq,in,startswith"`
	Age   *int64  `query:"age" filter:"eq,gte"`
	First           *int64  `query:"first"`
	Last            *int64  `query:"last"`
	After           *string `query:"after"`
	Before          *string `query:"before"`
	OrderBy         *string `query:"orderBy"`
	Search          *string `query:"search"`
	IncludeArchived *bool   `query:"includeArchived"`
	OnlyTotal       *bool   `query:"onlyTotal"`
}

func (r execRequest) ToQuery(crit queries.ReadCriteria) *execQuery {
	return &execQuery{crit: crit}
}

type execResponse struct {
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
	Age  *int64  `json:"age,omitempty"`
}

type fakeReadHandler struct {
	captured queries.ReadCriteria
	page     queries.Page
	err      error // when non-nil, Handle returns it (exercises the failure path)
}

func (h *fakeReadHandler) Handle(_ *configuration.AppContext, q *execQuery) (queries.Page, error) {
	h.captured = q.crit
	if h.err != nil {
		return queries.Page{}, h.err
	}
	return h.page, nil
}

func newExecRegistry(h *fakeReadHandler) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		QueryWithParams[execRequest, execResponse]("users", "User", h),
	)
	return reg, configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func TestExecute_ReadConnectionEndToEnd(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		Items: []map[string]any{
			{"ID": "u1", "Name": "alice", "Age": int64(30)},
			{"ID": "u2", "Name": "bob"},
		},
		ItemCursors: []string{"cur1", "cur2"},
		HasNextPage:     true,
		HasPreviousPage:     false,
		EndCursor:  "cur2",
		StartCursor:  "cur1",
		TotalCount:       2,
	}}
	reg, ctx := newExecRegistry(h)

	query := `query {
	  users(where: { name: { startswith: "al" } }, first: 10, orderBy: [{field: NAME, direction: DESC}]) {
	    edges { node { id name age } cursor }
	    pageInfo { hasNextPage startCursor endCursor }
	    totalCount
	  }
	}`
	resp := reg.Execute(ctx, query, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}

	// ── criteria reached the handler, folded identically to the REST path ──
	clause, ok := h.captured.Filter["Name"].(queries.TextMatch)
	if !ok || clause.Value != "al" || clause.Kind != queries.TextPrefix {
		t.Fatalf("where startswith did not fold to TextMatch{Value:al, Kind:Prefix}, got %#v", h.captured.Filter["Name"])
	}
	if h.captured.Limit != 10 {
		t.Errorf("first=10 → Limit, got %d", h.captured.Limit)
	}
	if len(h.captured.OrderBy) != 1 || h.captured.OrderBy[0].Field != "Name" || !h.captured.OrderBy[0].Desc {
		t.Errorf("orderBy [-name] → Sort{Name desc}, got %+v", h.captured.OrderBy)
	}

	// ── connection shape (Relay) ──
	users := resp.Data["users"].(map[string]any)
	if users["totalCount"] != int64(2) {
		t.Errorf("totalCount = %v, want 2", users["totalCount"])
	}
	edges := users["edges"].([]any)
	if len(edges) != 2 {
		t.Fatalf("edges len = %d, want 2", len(edges))
	}
	e0 := edges[0].(map[string]any)
	if e0["cursor"] != "cur1" {
		t.Errorf("edges[0].cursor = %v, want cur1 (per-item cursor)", e0["cursor"])
	}
	node0 := e0["node"].(map[string]any)
	if node0["id"] != "u1" || node0["name"] != "alice" || node0["age"] != int64(30) {
		t.Errorf("edges[0].node = %v", node0)
	}
	// bob carries no Age in the doc → wire age is nil, but the key is present
	// because the selection requested it.
	node1 := edges[1].(map[string]any)["node"].(map[string]any)
	if node1["age"] != nil {
		t.Errorf("edges[1].node.age = %v, want nil", node1["age"])
	}
	pageInfo := users["pageInfo"].(map[string]any)
	if pageInfo["hasNextPage"] != true || pageInfo["startCursor"] != "cur1" || pageInfo["endCursor"] != "cur2" {
		t.Errorf("pageInfo = %v", pageInfo)
	}
}

func TestExecute_SelectionTrimsUnrequestedFields(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{
		Items:       []map[string]any{{"ID": "u1", "Name": "alice", "Age": int64(30)}},
		ItemCursors: []string{"c1"},
		TotalCount:       1,
	}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users { edges { node { name } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	node := resp.Data["users"].(map[string]any)["edges"].([]any)[0].(map[string]any)["node"].(map[string]any)
	if _, has := node["name"]; !has {
		t.Error("requested field 'name' missing")
	}
	if _, has := node["id"]; has {
		t.Error("unrequested field 'id' must be trimmed from the node")
	}
	if _, has := node["age"]; has {
		t.Error("unrequested field 'age' must be trimmed from the node")
	}
}

func TestExecute_InListOperatorFoldsToMongoIn(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{TotalCount: 0}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(where: { name: { in: ["a", "b"] } }) { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	clause, ok := h.captured.Filter["Name"].(queries.Clause)
	if !ok || clause.Op != queries.FilterIn {
		t.Fatalf("name in-list did not produce an in-Clause, got %#v", h.captured.Filter["Name"])
	}
	list := clause.Values
	if len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Errorf("in = %v, want [a b]", list)
	}
}

func TestExecute_ValidationErrorSurfaces(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{}}
	reg, ctx := newExecRegistry(h)

	// `contains` is not declared on name (only eq,in,startswith) → validation error.
	resp := reg.Execute(ctx, `{ users(where: { name: { contains: "x" } }) { totalCount } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("expected a validation error for an undeclared operator")
	}
}

func TestExecute_UnknownTopLevelFieldRejected(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ bogus { totalCount } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("expected validation to reject an unknown root field")
	}
}

// ── domain failure → errors[].extensions{notificationKey, semantic, field} ──

type failReadHandler struct{}

func (h *failReadHandler) Handle(_ *configuration.AppContext, _ *execQuery) (queries.Page, error) {
	return queries.Page{}, domain.SingleNotificationError("User", "email", domain.RecordNotFoundNotification{})
}

func TestExecute_DomainFailureMapsToErrorExtensions(t *testing.T) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		QueryWithParams[execRequest, execResponse]("users", "User", &failReadHandler{}),
	)
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)

	resp := reg.Execute(ctx, `{ users { totalCount } }`, nil, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a domain Result.Failure must surface in errors[]")
	}
	ext := resp.Errors[0].Extensions
	if ext["notificationKey"] != "RecordNotFoundNotification" {
		t.Errorf("extensions.notificationKey = %v, want RecordNotFoundNotification", ext["notificationKey"])
	}
	if ext["semantic"] != "NotFound" {
		t.Errorf("extensions.semantic = %v, want NotFound", ext["semantic"])
	}
	if ext["field"] != "email" {
		t.Errorf("extensions.field = %v, want email", ext["field"])
	}
	if resp.Errors[0].Message == "" {
		t.Error("error message should be translated, not empty")
	}
}

// TestExecute_OrderByMultiTermAndDefaultDirection — a multi-term typed orderBy
// folds in order, and an absent direction defaults to ASC (whether or not the
// executor materializes the SDL default).
func TestExecute_OrderByMultiTermAndDefaultDirection(t *testing.T) {
	h := &fakeReadHandler{page: queries.Page{}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(orderBy: [{field: NAME, direction: DESC}, {field: AGE}]) { edges { node { id } } } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("unexpected errors: %+v", resp.Errors)
	}
	ob := h.captured.OrderBy
	if len(ob) != 2 {
		t.Fatalf("OrderBy terms = %d, want 2 (%+v)", len(ob), ob)
	}
	if ob[0].Field != "Name" || !ob[0].Desc {
		t.Errorf("term 1 = %+v, want {Name desc}", ob[0])
	}
	if ob[1].Field != "Age" || ob[1].Desc {
		t.Errorf("term 2 = %+v, want {Age asc} (default direction)", ob[1])
	}
}
