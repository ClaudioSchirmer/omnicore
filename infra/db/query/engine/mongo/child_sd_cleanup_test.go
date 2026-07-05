package mongo

import (
	"testing"
)

// removeChildSDColumn strips the auto-included soft-delete column at every
// path shape the reader produces: a child collection at the root, a
// SharedBaseView role segment (a single map) and a role's own child collection
// (dotted). Absent or differently-shaped nodes are no-ops.
func TestRemoveChildSDColumn(t *testing.T) {
	doc := map[string]any{
		"dependents": []any{
			map[string]any{"name": "Rita", "deleted_at": nil},
			map[string]any{"name": "Ana", "deleted_at": "2026-01-01"},
		},
		"user": map[string]any{"user_name": "ana", "deleted_at": nil},
		"employee": map[string]any{
			"employee_number": "M1",
			"dependents": []any{
				map[string]any{"name": "Bia", "deleted_at": nil},
			},
		},
	}

	removeChildSDColumn(doc, []string{"dependents"}, "deleted_at")
	for i, e := range doc["dependents"].([]any) {
		if _, has := e.(map[string]any)["deleted_at"]; has {
			t.Errorf("collection entry %d must lose the sd column", i)
		}
	}

	removeChildSDColumn(doc, []string{"user"}, "deleted_at")
	if _, has := doc["user"].(map[string]any)["deleted_at"]; has {
		t.Error("a role segment (single map) must lose the sd column")
	}

	removeChildSDColumn(doc, []string{"employee", "dependents"}, "deleted_at")
	deps := doc["employee"].(map[string]any)["dependents"].([]any)
	if _, has := deps[0].(map[string]any)["deleted_at"]; has {
		t.Error("a dotted role-child path must lose the sd column")
	}

	// No-ops: absent field, non-map container, nil segment list, scalar leaf.
	removeChildSDColumn(doc, []string{"missing"}, "deleted_at")
	removeChildSDColumn(doc, []string{"missing", "deeper"}, "deleted_at")
	removeChildSDColumn("not-a-map", []string{"x"}, "deleted_at")
	removeChildSDColumn(doc, nil, "deleted_at")
	removeChildSDColumn(map[string]any{"leaf": 42}, []string{"leaf"}, "deleted_at")
}
