package read

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Criteria-translator error branches (VisitNot / VisitLogical / unsupported
// operator / NotNull-with-values / child scope filter). Relocated from the
// former infra-root coverage grab-bag once criteria_translate.go moved to db.
// The dialect is the package-local testPGDialect ($n placeholders).

func TestPgVisitor_NotErrors(t *testing.T) {
	// NOT with a nil inner expression.
	if _, _, err := compileWhere(criteria.Not(nil), testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected error: NOT with nil inner")
	}
	// NOT propagates an inner error (unknown field).
	if _, _, err := compileWhere(criteria.Not(criteria.Eq("Nope", "x")), testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected NOT to propagate inner unknown-field error")
	}
}

func TestPgVisitor_LogicalInnerErrorPropagates(t *testing.T) {
	if _, _, err := compileWhere(criteria.And(criteria.Eq("Name", "ok"), criteria.Eq("Nope", "x")), testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected AND to propagate inner unknown-field error")
	}
	// OR joiner branch.
	sql, _, err := compileWhere(criteria.Or(criteria.Eq("Name", "a"), criteria.Eq("Email", "b")), testResolver(), testPGDialect{}, nil)
	if err != nil || !strings.Contains(sql, " OR ") {
		t.Errorf("OR sql = %q err=%v", sql, err)
	}
}

func TestPgVisitor_UnsupportedOperator(t *testing.T) {
	// A Comparison with an operator outside binaryOps/in/null hits the default
	// "unsupported operator" branch.
	bad := criteria.Comparison{Field: "Name", Op: criteria.Operator("bogus"), Values: []any{"x"}}
	if _, _, err := compileWhere(bad, testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected unsupported-operator error")
	}
}

func TestPgVisitor_NotNullWithValuesErrors(t *testing.T) {
	bad := criteria.Comparison{Field: "Phone", Op: criteria.OpNotNull, Values: []any{"x"}}
	if _, _, err := compileWhere(bad, testResolver(), testPGDialect{}, nil); err == nil {
		t.Error("expected error: IS NOT NULL with values")
	}
}

func TestChildScopeFilter_NoDeletedAt(t *testing.T) {
	off := NewExternalSchema("t") // no DeletedAt
	if got := childScopeFilter(criteria.ScopeActive, off, testPGDialect{}, ""); got != "" {
		t.Errorf("no-DeletedAt child must yield no filter, got %q", got)
	}
}
