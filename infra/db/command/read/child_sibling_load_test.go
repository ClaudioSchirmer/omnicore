package read

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// A2b loader coverage: childScanSQL LEFT JOINs a child's siblings and folds their
// columns into the scan plan (so they scan into the same child struct).

type csLoadChild struct {
	domain.Managed
	Label string
	Note  string // sibling field
}

func (c csLoadChild) BuildRules(string, domain.Service, *domain.Rules) {}

func TestChildScanSQL_WithSibling(t *testing.T) {
	child := NewTableSchema[csLoadChild]("cs_child").ID("id").ParentID("root_id").Field("Label", "label").
		DeletedAt("deleted_at").
		Sibling(NewSiblingSchema[csLoadChild]("cs_child_ext").Field("Note", "note"))
	cols, byCol := child.ScanPlan()
	ms := newChildManagedScan(child)
	sql, scanCols, scanByCol := childScanSQL(child, "root_id", cols, byCol, []string{"$1"}, criteria.ScopeActive, testPGDialect{}, ms.cols, nil)

	if !strings.Contains(sql, "LEFT JOIN") || !strings.Contains(sql, "cs_child_ext") {
		t.Errorf("a child with a sibling must LEFT JOIN it: %q", sql)
	}
	// The soft-delete gate is qualified the moment anything else is in the FROM —
	// a bare deleted_at is ambiguous against any joined table that has one.
	if !strings.Contains(sql, "cs_child.deleted_at IS NULL") {
		t.Errorf("the scope gate must be qualified under a join: %q", sql)
	}
	// The child's own id rides as a trailing column, qualified to the child table.
	if !strings.Contains(sql, "cs_child.id") {
		t.Errorf("the child's trailing id column must appear qualified: %q", sql)
	}
	if _, ok := scanByCol["note"]; !ok {
		t.Errorf("scan plan must fold the sibling column \"note\": %v", scanByCol)
	}
	hasNote := false
	for _, c := range scanCols {
		if c == "note" {
			hasNote = true
		}
	}
	if !hasNote {
		t.Errorf("scan columns must include the sibling column: %v", scanCols)
	}
}

func TestChildScanSQL_NoSibling(t *testing.T) {
	child := NewTableSchema[csLoadChild]("cs_child").ID("id").ParentID("root_id").Field("Label", "label")
	cols, byCol := child.ScanPlan()
	ms := newChildManagedScan(child)
	sql, _, _ := childScanSQL(child, "root_id", cols, byCol, []string{"$1"}, criteria.ScopeActive, testPGDialect{}, ms.cols, nil)
	if strings.Contains(sql, "LEFT JOIN") {
		t.Errorf("a child without siblings must not join: %q", sql)
	}
	// No join → the trailing id column is unqualified but present.
	if !strings.Contains(sql, "id") {
		t.Errorf("the child's trailing id column must appear: %q", sql)
	}
}
