package read

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The grouped half of the aggregate DSL: AggregateBy computes the SAME typed
// scalar facts as Aggregate, but per group — one SELECT with GROUP BY over the
// same criteria, resolution and scope gate. The specs passed in act as
// TEMPLATES naming each requested fact; every returned Group carries its own
// fresh copy of each template, read back type-safely via GroupResult:
//
//	perCat := read.By("Category")
//	total  := read.Count()
//	cents  := read.SumInt("PriceCents")
//	groups, err := loader.AggregateBy(ctx, q, perCat, total, cents)
//	if err != nil { … }
//	for _, g := range groups {
//	    g.KeyString("Category")               // this group's key
//	    read.GroupResult(g, total).Value      // this group's COUNT(*)
//	    read.GroupResult(g, cents).Value      // this group's exact SUM
//	}
//
// A group EXISTS because at least one row matched, so the per-group carriers
// report Found=true for every non-NULL scalar; an empty set yields zero groups
// (never a NULL-keyed placeholder row). len(groups) is itself a business fact —
// the number of distinct key combinations among the matching rows.

// GroupBy declares the grouping key(s) of an AggregateBy call — one or more Go
// fields of the entity, resolved through the same schema resolution as the
// criteria (sibling and shared-base fields pull their LEFT JOINs; grouping by
// the ID is nonsensical — every group would hold one row).
type GroupBy struct {
	fields []string
}

// By names the grouping field(s) of an AggregateBy call, in key order.
func By(goFields ...string) *GroupBy { return &GroupBy{fields: goFields} }

// Group is one GROUP BY result row: the group's key value(s) plus this group's
// result for every requested spec. Key access is by the Go field name passed
// to By; spec access is via GroupResult with the template spec.
type Group struct {
	keys   map[string]any
	bySpec map[AggregateSpec]AggregateSpec
}

// Key returns the group's raw key value for a By field — normalized to
// driver-neutral Go types (text lands as string on both backends; NULL stays
// nil). Panics on a field that was not part of the By declaration: a mistyped
// key name is a programming error, and returning nil would be indistinguishable
// from a legitimate NULL group key.
func (g *Group) Key(goField string) any {
	v, ok := g.keys[goField]
	if !ok {
		panic(fmt.Sprintf("read.Group.Key: %q is not a grouping field of the AggregateBy call that produced this group", goField))
	}
	return v
}

// KeyString returns the group's key rendered as a string — the convenient form
// for the dominant use case (grouping by a text-ish business field). A NULL
// key renders as "".
func (g *Group) KeyString(goField string) string {
	v := g.Key(goField)
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// GroupResult returns this group's result carrier for the given template spec,
// with the template's own concrete type — read.GroupResult(g, cents).Value with
// no type assertion at the call site. (A generic method is not legal Go, hence
// the top-level function.) Panics when the spec was not part of the AggregateBy
// call that produced the group — a programming error surfaced loudly.
func GroupResult[S AggregateSpec](g *Group, template S) S {
	r, ok := g.bySpec[template]
	if !ok {
		panic("read.GroupResult: the given spec is not part of the AggregateBy call that produced this group")
	}
	return r.(S)
}

// AggregateBy executes ONE SELECT computing every requested spec PER GROUP —
// GROUP BY the By fields, over the same criteria semantics as Aggregate:
// predicate, scope gate (active rows by default; the Query's scope can include
// archived) and the sibling/shared-base LEFT JOINs field resolution pulls in.
// It hydrates nothing: this is the write-path primitive for business rules
// over per-group facts (per-category caps, distinct-key cardinality, balanced
// distributions) — fetching the rows to bucket them in Go is the anti-pattern
// it exists to kill.
//
// Groups come back ordered by the key column(s) ascending — a deterministic
// order for tests and stable rule behavior (NULL-key placement follows the
// backend's default). Order/limit on the Query are ignored, exactly like
// Aggregate. The passed specs are templates and carry NO result after the
// call; per-group values live on the returned Groups (via GroupResult).
func (l *AggregateLoader[T]) AggregateBy(ctx context.Context, q *criteria.Query, by *GroupBy, specs ...AggregateSpec) ([]*Group, error) {
	if by == nil || len(by.fields) == 0 {
		return nil, fmt.Errorf("AggregateBy: at least one grouping field is required (Aggregate is the ungrouped form)")
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("AggregateBy: at least one aggregate spec is required")
	}
	for i, s := range specs {
		for _, prev := range specs[:i] {
			if prev == s {
				return nil, fmt.Errorf("AggregateBy: the same spec instance was passed twice — construct one spec per requested fact")
			}
		}
	}
	joins := &relSpecJoins{siblings: map[string]*TableSchema{}}
	resolve := l.specResolver(joins)
	dialect := l.eng.Dialect()

	keyCols := make([]string, len(by.fields))
	for i, f := range by.fields {
		col, ok := resolve(f)
		if !ok {
			return nil, fmt.Errorf("AggregateBy: unknown grouping field %q (not a persisted field of the entity)", f)
		}
		keyCols[i] = dialect.QuoteIdent(col)
	}
	exprs := make([]string, len(specs))
	for i, s := range specs {
		e, err := s.expr(resolve, dialect)
		if err != nil {
			return nil, err
		}
		exprs[i] = e
	}
	fromJoin, clause, args, err := l.compileFilterJoins(q, joins)
	if err != nil {
		return nil, err
	}
	keyList := strings.Join(keyCols, ", ")
	rows, err := l.eng.Querier().Query(ctx,
		"SELECT "+keyList+", "+strings.Join(exprs, ", ")+" FROM "+fromJoin+clause+
			" GROUP BY "+keyList+" ORDER BY "+keyList, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groups := []*Group{}
	for rows.Next() {
		raw := make([]any, len(keyCols)+len(specs))
		dest := make([]any, len(raw))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		g := &Group{
			keys:   make(map[string]any, len(by.fields)),
			bySpec: make(map[AggregateSpec]AggregateSpec, len(specs)),
		}
		for i, f := range by.fields {
			k, err := normalizeGroupKey(raw[i])
			if err != nil {
				return nil, fmt.Errorf("AggregateBy: group key %q: %w", f, err)
			}
			g.keys[f] = k
		}
		for j, s := range specs {
			c := s.fresh()
			if err := c.absorb(raw[len(keyCols)+j]); err != nil {
				return nil, err
			}
			g.bySpec[s] = c
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return groups, nil
}

// normalizeGroupKey converts a driver-delivered group key into a neutral Go
// value: byte slices become strings (MySQL delivers text columns as []byte;
// Postgres already yields string) and driver-native wrappers are unwrapped via
// their stdlib driver.Valuer (the same one-level contract as the scalar
// converters). NULL stays nil; every other primitive passes through untouched.
func normalizeGroupKey(v any) (any, error) {
	switch k := v.(type) {
	case []byte:
		return string(k), nil
	default:
		if u, err := unwrapValuer(v); err != nil {
			return nil, err
		} else if u != nil {
			return normalizeGroupKey(u)
		}
		return v, nil
	}
}
