package relational

import (
	"fmt"
	"sort"

	"github.com/ClaudioSchirmer/omnicore/application/exception"
	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/ClaudioSchirmer/omnicore/application/queries"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// unsupported reports a capability a relational view cannot serve (free-text
// search, or a filter or sort on a child field). It returns a
// RelationalCapabilityNotification carried on an *exception.ApplicationError —
// Semantic SemanticSchema, so the web wrappers turn it into a 400 with the escape
// hatch (drop RelationalSource) in the translated message. `what` is the offending
// capability or field, surfaced as the notification's FieldName.
func unsupported(what string) error {
	return exception.SingleNotificationError("Query", what, notifications.RelationalCapabilityNotification{})
}

// servable reports whether a filter/sort field is one the loader's root SELECT
// can express. The resolution surface MIRRORS the loader's specResolver exactly:
// the ROOT schema's own columns (business fields + id), then each 1:1 sibling
// (shared-PK satellite), then the shared base (role → base). Those three are 1:1
// with the root, so the loader reaches them with a LEFT JOIN and the WHERE/ORDER
// resolves against the joined column. Everything else — a dotted child path, a
// dotted child-level sibling, or an unknown field — is a 1:N pushdown a single
// root SELECT cannot express, and stays unservable (→ 400).
func servable(schema *core.TableSchema, field string) bool {
	if schema == nil {
		return false
	}
	if _, ok := schema.ColumnOf(field); ok {
		return true
	}
	for _, sib := range schema.Siblings() {
		if _, ok := sib.ColumnOf(field); ok {
			return true
		}
	}
	if base, _, ok := schema.SharedBaseRef(); ok {
		if _, ok := base.ColumnOf(field); ok {
			return true
		}
	}
	return false
}

// toExpr translates the wire-neutral ReadCriteria.Filter (Go-field-keyed, the
// Clause / TextMatch / TextMatchList / MultiClause sentinels) into a criteria
// expression the loader compiles to root SQL. Keys are visited in sorted order
// so the AND is deterministic. A field the root SELECT cannot express (child,
// sibling, unknown) is rejected here as an unsupported capability (400).
func toExpr(schema *core.TableSchema, filter map[string]any) (criteria.Expr, error) {
	if len(filter) == 0 {
		return nil, nil
	}
	fields := make([]string, 0, len(filter))
	for field := range filter {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	ands := make([]criteria.Expr, 0, len(fields))
	for _, field := range fields {
		if !servable(schema, field) {
			return nil, unsupported(field)
		}
		e, err := clauseToExpr(field, filter[field])
		if err != nil {
			return nil, err
		}
		if e != nil {
			ands = append(ands, e)
		}
	}
	switch len(ands) {
	case 0:
		return nil, nil
	case 1:
		return ands[0], nil
	default:
		return criteria.And(ands...), nil
	}
}

// clauseToExpr translates one field's clause value. A bare scalar is equality; a
// MultiClause ANDs its parts on the same field; the sentinels map to their
// criteria operator.
func clauseToExpr(field string, v any) (criteria.Expr, error) {
	switch x := v.(type) {
	case queries.Clause:
		return ordinalToExpr(field, x)
	case queries.TextMatch:
		return textToExpr(field, x), nil
	case queries.TextMatchList:
		return textListToExpr(field, x), nil
	case queries.MultiClause:
		ands := make([]criteria.Expr, 0, len(x.Clauses))
		for _, c := range x.Clauses {
			e, err := clauseToExpr(field, c)
			if err != nil {
				return nil, err
			}
			if e != nil {
				ands = append(ands, e)
			}
		}
		if len(ands) == 0 {
			return nil, nil
		}
		if len(ands) == 1 {
			return ands[0], nil
		}
		return criteria.And(ands...), nil
	default:
		return criteria.Eq(field, v), nil
	}
}

// ordinalToExpr maps the neutral ordinal/set operators to criteria comparisons.
func ordinalToExpr(field string, c queries.Clause) (criteria.Expr, error) {
	switch c.Op {
	case queries.FilterIn:
		return criteria.In(field, c.Values...), nil
	case queries.FilterNin:
		return criteria.Nin(field, c.Values...), nil
	}
	if len(c.Values) == 0 {
		return nil, fmt.Errorf("relational view: operator %q carries no value", c.Op)
	}
	v := c.Values[0]
	switch c.Op {
	case queries.FilterNe:
		return criteria.Ne(field, v), nil
	case queries.FilterGt:
		return criteria.Gt(field, v), nil
	case queries.FilterGte:
		return criteria.Gte(field, v), nil
	case queries.FilterLt:
		return criteria.Lt(field, v), nil
	case queries.FilterLte:
		return criteria.Lte(field, v), nil
	default:
		return nil, fmt.Errorf("relational view: unknown operator %q", c.Op)
	}
}

// textToExpr maps a neutral TextMatch to a LIKE (case-sensitive) or ILIKE
// (case-insensitive) with the value escaped and anchored per kind — the raw
// value is turned into a store pattern HERE, mirroring the anchoring the Mongo
// reader applies to build its regex.
func textToExpr(field string, t queries.TextMatch) criteria.Expr {
	esc := criteria.EscapeLike(t.Value)
	var pattern string
	switch t.Kind {
	case queries.TextPrefix:
		pattern = esc + "%"
	case queries.TextContains:
		pattern = "%" + esc + "%"
	default: // queries.TextExact
		pattern = esc
	}
	var e criteria.Expr
	if t.CaseInsensitive {
		e = criteria.ILike(field, pattern)
	} else {
		e = criteria.Like(field, pattern)
	}
	if t.Negate {
		e = criteria.Not(e)
	}
	return e
}

// textListToExpr maps a neutral TextMatchList (case-insensitive membership) to an
// OR of exact (I)LIKE matches, negated to a NOT-OR for the nin variant.
func textListToExpr(field string, t queries.TextMatchList) criteria.Expr {
	ors := make([]criteria.Expr, 0, len(t.Values))
	for _, v := range t.Values {
		p := criteria.EscapeLike(v)
		if t.CaseInsensitive {
			ors = append(ors, criteria.ILike(field, p))
		} else {
			ors = append(ors, criteria.Like(field, p))
		}
	}
	e := criteria.Or(ors...)
	if t.Negate {
		e = criteria.Not(e)
	}
	return e
}

// applySort appends the request's root-field sort terms to the query. A sort on
// a field the root ORDER BY cannot express (child, sibling, unknown) is rejected
// as an unsupported capability (400).
func applySort(schema *core.TableSchema, q *criteria.Query, sorts []queries.OrderByField) error {
	for _, s := range sorts {
		if !servable(schema, s.Field) {
			return unsupported(s.Field)
		}
		if s.Desc {
			q.OrderByDesc(s.Field)
		} else {
			q.OrderBy(s.Field)
		}
	}
	return nil
}
