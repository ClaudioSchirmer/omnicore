package graphql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
)

// ── coercion helpers ─────────────────────────────────────────────────────────

func TestToInt64_Coercions(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{int64(9), 9, true},
		{7, 7, true},
		{3.0, 3, true},
		{"12", 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		got, ok := toInt64(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("toInt64(%v) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ── buildCriteria defensive branches ─────────────────────────────────────────

type covCritRequest struct {
	Name *string `query:"name" filter:"eq,in"`
}

type covCritQueryResponse struct {
	ID   *string `json:"id,omitempty"`
	Name *string `json:"name,omitempty"`
}

func covPlan() *criteriaPlan {
	return newCriteriaPlan("CovCrit", reflect.TypeOf(covCritRequest{}), reflect.TypeOf(covCritQueryResponse{}))
}

func TestBuildCriteria_UnknownWhereFieldRejected(t *testing.T) {
	_, _, gerr := covPlan().buildCriteria(map[string]any{
		"where": map[string]any{"bogus": map[string]any{"eq": "x"}},
	})
	if gerr == nil {
		t.Fatal("an unknown where field must be rejected")
	}
	if !strings.Contains(gerr.Message, "bogus") {
		t.Errorf("error must name the bad field, got %q", gerr.Message)
	}
}

func TestBuildCriteria_NonObjectWhereValueIgnored(t *testing.T) {
	crit, _, gerr := covPlan().buildCriteria(map[string]any{
		"where": map[string]any{"name": "not-an-operator-object"},
	})
	if gerr != nil {
		t.Fatalf("a non-object where value is skipped defensively, got %v", gerr)
	}
	if len(crit.Filter) != 0 {
		t.Errorf("no clause must be emitted, got %v", crit.Filter)
	}
}

// ── projectionFromSelection: crafted ASTs for the defensive branches ─────────

func covNodeSelection(nodeFields ...*ast.Field) ast.SelectionSet {
	node := &ast.Field{Name: "node", SelectionSet: ast.SelectionSet{}}
	for _, f := range nodeFields {
		node.SelectionSet = append(node.SelectionSet, f)
	}
	return ast.SelectionSet{
		&ast.Field{Name: "edges", SelectionSet: ast.SelectionSet{node}},
	}
}

func TestProjectionFromSelection_LeafSelectionProjects(t *testing.T) {
	proj := covPlan().projectionFromSelection(
		covNodeSelection(&ast.Field{Name: "id"}), nil)
	if len(proj) != 1 || proj["ID"] != 1 {
		t.Errorf("node { id } must project the Go path ID, got %v", proj)
	}
}

func TestProjectionFromSelection_EmptyNodeSelectionIsNil(t *testing.T) {
	if proj := covPlan().projectionFromSelection(covNodeSelection(), nil); proj != nil {
		t.Errorf("an empty node selection must drop the projection, got %v", proj)
	}
}

func TestProjectionFromSelection_UnknownLeafDropsProjection(t *testing.T) {
	proj := covPlan().projectionFromSelection(
		covNodeSelection(&ast.Field{Name: "bogus"}), nil)
	if proj != nil {
		t.Errorf("a stray leaf must drop the projection (defensive), got %v", proj)
	}
}

func TestProjectionFromSelection_NoNodeIsNil(t *testing.T) {
	sel := ast.SelectionSet{&ast.Field{Name: "totalCount"}}
	if proj := covPlan().projectionFromSelection(sel, nil); proj != nil {
		t.Errorf("no edges/node selection must yield nil, got %v", proj)
	}
}
