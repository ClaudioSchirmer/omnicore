package infra

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// fieldResolver maps a Go field name to its SQL column. The loader builds it
// from the entity's TableSchema (TableSchema.fieldResolver). ok=false for an
// unknown / non-persisted field → the translator fails fast (developer bug).
type fieldResolver func(goField string) (column string, ok bool)

// pgVisitor walks a criteria.Expr and accumulates a WHERE fragment + bound
// args. Unexported — the developer never constructs or sees it. Identifiers
// pass through validIdentifier (columns are framework/TableSchema-derived, never
// user input); values are parameterized; domain.ID args are unwrapped to their
// string value.
type pgVisitor struct {
	resolve fieldResolver
	sb      strings.Builder
	args    []any
}

func (v *pgVisitor) place(val any) string {
	v.args = append(v.args, normalizeArg(val))
	return fmt.Sprintf("$%d", len(v.args))
}

// normalizeArg unwraps framework value types pgx cannot bind directly.
// domain.ID exposes Value() string (not driver.Valuer), so it is converted
// here — keeping the pure criteria package free of any domain import.
func normalizeArg(val any) any {
	if id, ok := val.(domain.ID); ok {
		return id.Value()
	}
	return val
}

func (v *pgVisitor) VisitComparison(c criteria.Comparison) error {
	col, ok := v.resolve(c.Field)
	if !ok {
		return fmt.Errorf("criteria: unknown field %q (not a persisted field of the entity)", c.Field)
	}
	col = validIdentifier(col)

	switch c.Op {
	case criteria.OpIsNull:
		if len(c.Values) != 0 {
			return fmt.Errorf("criteria: operator %q on %q takes no values, got %d", c.Op, c.Field, len(c.Values))
		}
		v.sb.WriteString(col)
		v.sb.WriteString(" IS NULL")
	case criteria.OpNotNull:
		if len(c.Values) != 0 {
			return fmt.Errorf("criteria: operator %q on %q takes no values, got %d", c.Op, c.Field, len(c.Values))
		}
		v.sb.WriteString(col)
		v.sb.WriteString(" IS NOT NULL")
	case criteria.OpIn, criteria.OpNin:
		if len(c.Values) == 0 {
			return fmt.Errorf("criteria: operator %q on %q requires at least one value", c.Op, c.Field)
		}
		ph := make([]string, len(c.Values))
		for i, val := range c.Values {
			ph[i] = v.place(val)
		}
		v.sb.WriteString(col)
		if c.Op == criteria.OpNin {
			v.sb.WriteString(" NOT IN (")
		} else {
			v.sb.WriteString(" IN (")
		}
		v.sb.WriteString(strings.Join(ph, ", "))
		v.sb.WriteByte(')')
	default:
		op, ok := binaryOps[c.Op]
		if !ok {
			return fmt.Errorf("criteria: unsupported operator %q", c.Op)
		}
		if len(c.Values) != 1 {
			return fmt.Errorf("criteria: operator %q on %q requires exactly one value, got %d", c.Op, c.Field, len(c.Values))
		}
		v.sb.WriteString(col)
		v.sb.WriteByte(' ')
		v.sb.WriteString(op)
		v.sb.WriteByte(' ')
		v.sb.WriteString(v.place(c.Values[0]))
	}
	return nil
}

var binaryOps = map[criteria.Operator]string{
	criteria.OpEq:    "=",
	criteria.OpNe:    "<>",
	criteria.OpGt:    ">",
	criteria.OpGte:   ">=",
	criteria.OpLt:    "<",
	criteria.OpLte:   "<=",
	criteria.OpLike:  "LIKE",
	criteria.OpILike: "ILIKE",
}

func (v *pgVisitor) VisitLogical(l criteria.Logical) error {
	if len(l.Operands) == 0 {
		return fmt.Errorf("criteria: %s with no operands", l.Op)
	}
	joiner := " AND "
	if l.Op == criteria.LogicalOr {
		joiner = " OR "
	}
	v.sb.WriteByte('(')
	for i, op := range l.Operands {
		if i > 0 {
			v.sb.WriteString(joiner)
		}
		if err := op.Accept(v); err != nil {
			return err
		}
	}
	v.sb.WriteByte(')')
	return nil
}

func (v *pgVisitor) VisitNot(n criteria.Negation) error {
	if n.Inner == nil {
		return fmt.Errorf("criteria: NOT with no inner expression")
	}
	v.sb.WriteString("NOT (")
	if err := n.Inner.Accept(v); err != nil {
		return err
	}
	v.sb.WriteByte(')')
	return nil
}

// compileWhere renders the predicate into a SQL fragment + ordered args. A nil
// predicate yields an empty fragment (no WHERE).
func compileWhere(e criteria.Expr, resolve fieldResolver) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	v := &pgVisitor{resolve: resolve}
	if err := e.Accept(v); err != nil {
		return "", nil, err
	}
	return v.sb.String(), v.args, nil
}

// scopeGate returns the soft-delete condition for the scope on the source's
// resolved soft-delete column ("" = no gate). A source with soft-delete
// disabled has no marker column, so every scope yields no gate.
func scopeGate(s criteria.Scope, schema *TableSchema) string {
	col, ok := schema.softDeleteColumn()
	if !ok {
		return ""
	}
	switch s {
	case criteria.ScopeOnlyArchived:
		return validIdentifier(col) + " IS NOT NULL"
	case criteria.ScopeIncludeArchived:
		return ""
	default:
		return validIdentifier(col) + " IS NULL"
	}
}

// childScopeFilter maps the scope to the trailing child filter clause on the
// child source's soft-delete column: active children are gated on
// <col> IS NULL; under any archived scope children load unfiltered so the
// unarchive cascade sees every child via AllAggregateItems(). A child with
// soft-delete disabled is never gated.
func childScopeFilter(s criteria.Scope, schema *TableSchema) string {
	col, ok := schema.softDeleteColumn()
	if !ok {
		return ""
	}
	if s == criteria.ScopeActive {
		return "AND " + validIdentifier(col) + " IS NULL"
	}
	return ""
}

// compileOrder renders the ORDER BY clause ("" when no order). Each field is
// resolved + validated like the predicate columns.
func compileOrder(order []criteria.OrderField, resolve fieldResolver) (string, error) {
	if len(order) == 0 {
		return "", nil
	}
	parts := make([]string, len(order))
	for i, o := range order {
		col, ok := resolve(o.Field)
		if !ok {
			return "", fmt.Errorf("criteria: unknown order field %q", o.Field)
		}
		col = validIdentifier(col)
		if o.Desc {
			parts[i] = col + " DESC"
		} else {
			parts[i] = col + " ASC"
		}
	}
	return "ORDER BY " + strings.Join(parts, ", "), nil
}

// buildWhereClause joins the predicate fragment and the scope gate with AND,
// prefixing "WHERE " when anything is present.
func buildWhereClause(where, gate string) string {
	parts := make([]string, 0, 2)
	if where != "" {
		parts = append(parts, where)
	}
	if gate != "" {
		parts = append(parts, gate)
	}
	if len(parts) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(parts, " AND ")
}
