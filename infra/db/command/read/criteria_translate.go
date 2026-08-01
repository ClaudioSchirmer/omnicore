package read

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// sqlVisitor walks a criteria.Expr and accumulates a WHERE fragment + bound
// args. Unexported — the developer never constructs or sees it. Identifiers
// pass through validIdentifier (columns are framework/TableSchema-derived, never
// user input); values are parameterized; domain.ID args are unwrapped to their
// string value. The Go-field → column lookup is core.FieldResolver (built from
// the TableSchema in the core foundation); idKind is the field's identity
// typing (core.TableSchema.IDKindOf, derived from the Go struct), so a probe
// against a domain.ID-typed field binds in the dialect's native id form even
// when the caller hands a bare string.
type sqlVisitor struct {
	resolve core.FieldResolver
	dialect Dialect
	idKind  func(goField string) core.IDKind // nil = no id lifting
	sb      strings.Builder
	args    []any
}

// place binds one probe value for the (Go-named) field and returns its
// placeholder. A probe on an identity-typed field (domain.ID / *domain.ID —
// the field's Go TYPE is the declaration) is lifted into domain.ID first, so
// the dialect's typed codec renders it (BINARY(16) on MySQL, uuid text on PG);
// non-string probes (LIKE %patterns% never reach here as ids in practice —
// they stay strings on string-typed fields) and already-typed values pass to
// EncodeArg untouched.
func (v *sqlVisitor) place(goField string, val any) string {
	if v.idKind != nil {
		switch v.idKind(goField) {
		case core.IDValue, core.IDPointer:
			val = liftIDProbe(val)
		}
	}
	v.args = append(v.args, v.dialect.EncodeArg(val))
	return v.dialect.Placeholder(len(v.args))
}

// liftIDProbe lifts a bare probe value into the identity type: string →
// domain.ID, *string → *domain.ID (nil stays a typed nil → SQL NULL);
// already-typed values (domain.ID, *domain.ID, uuid.UUID) and anything else
// pass through for EncodeArg's own handling.
func liftIDProbe(val any) any {
	switch v := val.(type) {
	case string:
		return domain.NewID(v)
	case *string:
		if v == nil {
			return (*domain.ID)(nil)
		}
		return domain.NewID(*v)
	default:
		return val
	}
}

func (v *sqlVisitor) VisitComparison(c criteria.Comparison) error {
	col, ok := v.resolve(c.Field)
	if !ok {
		return fmt.Errorf("criteria: unknown field %q (not a persisted field of the entity)", c.Field)
	}
	col = v.dialect.QuoteIdent(col)

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
			// An empty set is a well-defined predicate, not an error (SQL forbids
			// the literal `IN ()`): `IN ()` matches nothing, `NOT IN ()` matches
			// everything — the same semantics MongoDB gives `$in:[]` / `$nin:[]`,
			// so the relational and Mongo read paths agree.
			if c.Op == criteria.OpNin {
				v.sb.WriteString("1=1")
			} else {
				v.sb.WriteString("1=0")
			}
			return nil
		}
		ph := make([]string, len(c.Values))
		for i, val := range c.Values {
			ph[i] = v.place(c.Field, val)
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
		if len(c.Values) != 1 {
			return fmt.Errorf("criteria: operator %q on %q requires exactly one value, got %d", c.Op, c.Field, len(c.Values))
		}
		if c.Op == criteria.OpILike {
			// Case-insensitive LIKE is dialect-specific and rendered as a whole
			// clause: native ILIKE on Postgres, LOWER(col) LIKE LOWER(?) on MySQL
			// (case-insensitive on any collation).
			v.sb.WriteString(v.dialect.ILikeClause(col, v.place(c.Field, c.Values[0])))
			break
		}
		if c.Op == criteria.OpLike {
			// Case-SENSITIVE LIKE is dialect-specific too: a bare LIKE is only
			// reliably case-sensitive on PG/Oracle, so MySQL/SQL Server force
			// byte-exact comparison via Dialect.LikeClause. Rendered as a whole
			// clause like ILike (not a plain binary operator).
			v.sb.WriteString(v.dialect.LikeClause(col, v.place(c.Field, c.Values[0])))
			break
		}
		op, ok := binaryOps[c.Op]
		if !ok {
			return fmt.Errorf("criteria: unsupported operator %q", c.Op)
		}
		v.sb.WriteString(col)
		v.sb.WriteByte(' ')
		v.sb.WriteString(op)
		v.sb.WriteByte(' ')
		v.sb.WriteString(v.place(c.Field, c.Values[0]))
	}
	return nil
}

var binaryOps = map[criteria.Operator]string{
	criteria.OpEq:  "=",
	criteria.OpNe:  "<>",
	criteria.OpGt:  ">",
	criteria.OpGte: ">=",
	criteria.OpLt:  "<",
	criteria.OpLte: "<=",
	// OpLike and OpILike are NOT here — each renders as a whole clause via
	// Dialect.LikeClause / Dialect.ILikeClause (a bare LIKE is not reliably
	// case-sensitive across engines; a bare ILIKE is Postgres-only).
}

func (v *sqlVisitor) VisitLogical(l criteria.Logical) error {
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

func (v *sqlVisitor) VisitNot(n criteria.Negation) error {
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
func compileWhere(e criteria.Expr, resolve core.FieldResolver, dialect Dialect, idKind func(string) core.IDKind) (string, []any, error) {
	if e == nil {
		return "", nil, nil
	}
	v := &sqlVisitor{resolve: resolve, dialect: dialect, idKind: idKind}
	if err := e.Accept(v); err != nil {
		return "", nil, err
	}
	return v.sb.String(), v.args, nil
}

// scopeGate returns the DeletedAt condition for the scope on the source's
// resolved DeletedAt column ("" = no gate). A source with DeletedAt
// disabled has no marker column, so every scope yields no gate.
// qualifier is the table-qualified prefix (already quoted) to prepend to the
// DeletedAt column, or "" to leave it bare. It MUST be non-empty when the
// query JOINs another archivable table (a role's SharedBase, whose own
// deleted_at would otherwise make the bare column reference ambiguous), matching
// how the leading ID is qualified under the same joins.
func scopeGate(s criteria.Scope, schema *TableSchema, dialect Dialect, qualifier string) string {
	col, ok := schema.DeletedAtColumn()
	if !ok {
		return ""
	}
	qcol := dialect.QuoteIdent(col)
	if qualifier != "" {
		qcol = qualifier + "." + qcol
	}
	switch s {
	case criteria.ScopeOnlyArchived:
		return qcol + " IS NOT NULL"
	case criteria.ScopeIncludeArchived:
		return ""
	default:
		return qcol + " IS NULL"
	}
}

// childScopeFilter maps the scope to the trailing child filter clause on the
// child source's DeletedAt column: active children are gated on
// <col> IS NULL; under any archived scope children load unfiltered so the
// unarchive cascade sees every child via AllAggregateItems(). A child with
// DeletedAt disabled is never gated.
// qualifier follows the same rule as scopeGate's: pass the (quoted) owning table
// when the child query JOINs another archivable table — as the base-child
// loader does (base child JOINed to the role, both carrying deleted_at) — and ""
// for a single-table child SELECT where the bare column is unambiguous.
func childScopeFilter(s criteria.Scope, schema *TableSchema, dialect Dialect, qualifier string) string {
	col, ok := schema.DeletedAtColumn()
	if !ok {
		return ""
	}
	if s == criteria.ScopeActive {
		qcol := dialect.QuoteIdent(col)
		if qualifier != "" {
			qcol = qualifier + "." + qcol
		}
		return "AND " + qcol + " IS NULL"
	}
	return ""
}

// compileOrder renders the ORDER BY clause ("" when no order). Each field is
// resolved + validated like the predicate columns.
func compileOrder(order []criteria.OrderField, resolve core.FieldResolver, dialect Dialect) (string, error) {
	if len(order) == 0 {
		return "", nil
	}
	parts := make([]string, len(order))
	for i, o := range order {
		col, ok := resolve(o.Field)
		if !ok {
			return "", fmt.Errorf("criteria: unknown order field %q", o.Field)
		}
		col = dialect.QuoteIdent(col)
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
