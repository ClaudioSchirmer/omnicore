package infra

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/criteria"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// fieldResolver maps a Go field name to its SQL column. The loader builds it
// from the entity's struct index + RepoConfig.FieldOverrides. ok=false for an
// unknown / non-persisted field → the translator fails fast (developer bug).
type fieldResolver func(goField string) (column string, ok bool)

// newFieldResolver builds a resolver for entity type t. Keys are Go field
// names (PascalCase); values are the resolved columns. transient/anonymous/
// private fields are excluded (they are not in the struct index), so a
// criterion referencing them is rejected as "unknown field". RepoConfig
// FieldOverrides (GoField → column) take priority over the snake_case
// convention.
func newFieldResolver(t reflect.Type, overrides map[string]string) fieldResolver {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	// Seed the fixed primary-key convention: the Go field "ID" ↔ column "id".
	// The id is managed by BaseEntity (a private field), so it does not appear
	// in the struct index — but criteria.ByID("ID") must always resolve, and
	// the framework hardcodes `id` as the PK column everywhere. An entity that
	// also exposes an exported ID field resolves to the same "id" via the loop.
	byField := map[string]string{"ID": "id"}
	if t.Kind() == reflect.Struct {
		idx := loadStructIndex(t)
		for _, fi := range idx.order {
			name := t.Field(fi.fieldIndex).Name
			col := fi.col
			if overrides != nil {
				if o, ok := overrides[name]; ok && o != "" {
					col = o
				}
			}
			byField[name] = col
		}
	}
	return func(goField string) (string, bool) {
		c, ok := byField[goField]
		return c, ok
	}
}

// pgVisitor walks a criteria.Expr and accumulates a WHERE fragment + bound
// args. Unexported — the developer never constructs or sees it. Identifiers
// pass through validIdentifier (columns are framework/RepoConfig-derived, never
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

// scopeGate returns the soft-delete condition for the scope ("" = no gate).
func scopeGate(s criteria.Scope) string {
	switch s {
	case criteria.ScopeOnlyArchived:
		return "deleted_at IS NOT NULL"
	case criteria.ScopeIncludeArchived:
		return ""
	default:
		return "deleted_at IS NULL"
	}
}

// childScopeFilter maps the scope to the trailing child filter clause: active
// children are gated on deleted_at IS NULL; under any archived scope children
// load unfiltered so the unarchive cascade sees every child via
// AllAggregateItems().
func childScopeFilter(s criteria.Scope) string {
	if s == criteria.ScopeActive {
		return "AND deleted_at IS NULL"
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
