package mongo

import (
	"testing"
)

// removeChildArchivedColumn strips the auto-included ArchivedAt column at every
// path shape the reader produces: a child collection at the root, a
// SharedBaseView role segment (a single map) and a role's own child collection
// (dotted). Absent or differently-shaped nodes are no-ops.
func TestRemoveChildSDColumn(t *testing.T) {
	doc := map[string]any{
		"dependents": []any{
			map[string]any{"name": "Rita", "archived_at": nil},
			map[string]any{"name": "Ana", "archived_at": "2026-01-01"},
		},
		"user": map[string]any{"user_name": "ana", "archived_at": nil},
		"employee": map[string]any{
			"employee_number": "M1",
			"dependents": []any{
				map[string]any{"name": "Bia", "archived_at": nil},
			},
		},
	}

	removeChildArchivedColumn(doc, []string{"dependents"}, "archived_at")
	for i, e := range doc["dependents"].([]any) {
		if _, has := e.(map[string]any)["archived_at"]; has {
			t.Errorf("collection entry %d must lose the archivedCol column", i)
		}
	}

	removeChildArchivedColumn(doc, []string{"user"}, "archived_at")
	if _, has := doc["user"].(map[string]any)["archived_at"]; has {
		t.Error("a role segment (single map) must lose the archivedCol column")
	}

	removeChildArchivedColumn(doc, []string{"employee", "dependents"}, "archived_at")
	deps := doc["employee"].(map[string]any)["dependents"].([]any)
	if _, has := deps[0].(map[string]any)["archived_at"]; has {
		t.Error("a dotted role-child path must lose the archivedCol column")
	}

	// An intermediate segment that is an ARRAY: a 1:N embed of a local view
	// (query.JoinView) whose elements carry their own child collections — the
	// array-in-array shape. Every element must be descended into.
	doc["sales"] = []any{
		map[string]any{"total": 10, "SaleItems": []any{map[string]any{"label": "a", "archived_at": nil}}},
		map[string]any{"total": 20, "SaleItems": []any{map[string]any{"label": "b", "archived_at": "2026-01-01"}}},
	}
	removeChildArchivedColumn(doc, []string{"sales", "SaleItems"}, "archived_at")
	for i, sale := range doc["sales"].([]any) {
		for j, item := range sale.(map[string]any)["SaleItems"].([]any) {
			if _, has := item.(map[string]any)["archived_at"]; has {
				t.Errorf("sales[%d].SaleItems[%d] must lose the archivedCol column inside a 1:N view segment", i, j)
			}
		}
	}

	// No-ops: absent field, non-map container, nil segment list, scalar leaf.
	removeChildArchivedColumn(doc, []string{"missing"}, "archived_at")
	removeChildArchivedColumn(doc, []string{"missing", "deeper"}, "archived_at")
	removeChildArchivedColumn("not-a-map", []string{"x"}, "archived_at")
	removeChildArchivedColumn(doc, nil, "archived_at")
	removeChildArchivedColumn(map[string]any{"leaf": 42}, []string{"leaf"}, "archived_at")
}
