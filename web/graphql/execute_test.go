package graphql

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
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
	Limit *int64  `query:"limit"`
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
}

func (h *fakeReadHandler) Handle(_ *configuration.AppContext, q *execQuery) (queries.Page, error) {
	h.captured = q.crit
	return h.page, nil
}

func newExecRegistry(h *fakeReadHandler) (*Registry, *configuration.AppContext) {
	pipe := pipeline.New(translation.Default())
	reg := New(pipe).Register(
		Query[execRequest, *execQuery, execResponse]("users", "User", h),
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
		HasNext:     true,
		HasPrev:     false,
		NextCursor:  "cur2",
		PrevCursor:  "cur1",
		Total:       2,
	}}
	reg, ctx := newExecRegistry(h)

	query := `query {
	  users(where: { name: { startswith: "al" } }, first: 10, orderBy: ["-name"]) {
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
	clause, ok := h.captured.Filter["Name"].(map[string]any)
	if !ok || clause["$regex"] != "^al" {
		t.Fatalf("where startswith did not fold to {$regex:^al}, got %v", h.captured.Filter["Name"])
	}
	if h.captured.Limit != 10 {
		t.Errorf("first=10 → Limit, got %d", h.captured.Limit)
	}
	if len(h.captured.Sort) != 1 || h.captured.Sort[0].Field != "Name" || !h.captured.Sort[0].Desc {
		t.Errorf("orderBy [-name] → Sort{Name desc}, got %+v", h.captured.Sort)
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
		Total:       1,
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
	h := &fakeReadHandler{page: queries.Page{Total: 0}}
	reg, ctx := newExecRegistry(h)

	resp := reg.Execute(ctx, `{ users(where: { name: { in: ["a", "b"] } }) { totalCount } }`, nil, "")
	if len(resp.Errors) != 0 {
		t.Fatalf("errors: %+v", resp.Errors)
	}
	clause, ok := h.captured.Filter["Name"].(map[string]any)
	if !ok {
		t.Fatalf("name in-list did not produce a clause, got %v", h.captured.Filter)
	}
	list, ok := clause["$in"].([]any)
	if !ok || len(list) != 2 || list[0] != "a" || list[1] != "b" {
		t.Errorf("$in = %v, want [a b]", clause["$in"])
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
