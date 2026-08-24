package read

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The aggregate DSL: one Aggregate call computes any combination of scalar
// facts — COUNT, SUM, AVG, MIN, MAX — over the SAME criteria in ONE SELECT.
// Each requested fact is a typed spec (constructed by Count/SumInt/Sum/Avg/
// MinInt/MaxInt/Min/Max) that carries its own result after the call:
//
//	total := read.Count()
//	cents := read.SumInt("PriceCents")
//	area  := read.Avg("BuiltArea")
//	if err := loader.Aggregate(ctx, q, total, cents, area); err != nil { … }
//	total.Value, cents.Value, area.Value, area.Found
//
// Specs resolve Go fields through the same schema resolution as FindOne/
// FindAll (sibling and shared-base fields pull their LEFT JOINs; the ID is
// addressable as "ID") and run under the same scope gate (active rows by
// default). A spec instance is stateful: create fresh specs per call site and
// do not share one across goroutines.

// AggregateSpec is one requested scalar fact inside an Aggregate call. Sealed:
// the read package's constructors (Count, SumInt, Sum, Avg, MinInt, MaxInt,
// Min, Max) are the only implementations — each carries its typed result.
type AggregateSpec interface {
	// expr renders the SELECT expression, resolving the spec's Go field into
	// its column (recording any sibling/shared-base join on the resolver) and
	// qualifying it the way the statement's FROM demands (colQual).
	expr(resolve core.FieldResolver, dialect Dialect, qual colQual) (string, error)
	// absorb receives the scalar as the driver delivered it (nil = SQL NULL,
	// i.e. no row matched) and writes the spec's typed result.
	absorb(v any) error
	// fresh returns a zero-result copy of the same fact — AggregateBy absorbs
	// one copy per group so every group carries its own typed result.
	fresh() AggregateSpec
}

// CountAgg is the result carrier of a Count() spec. Value is the number of
// matching roots — 0 on an empty set (COUNT never returns NULL, so no Found
// flag is needed: zero IS the answer).
type CountAgg struct {
	Value int64
}

// Count requests the number of roots matching the criteria — COUNT(*). The
// LEFT JOINs criteria may pull in are all 1:1 (sibling, shared base), so the
// row count is the root count.
func Count() *CountAgg { return &CountAgg{} }

func (c *CountAgg) expr(core.FieldResolver, Dialect, colQual) (string, error) { return "COUNT(*)", nil }

func (c *CountAgg) fresh() AggregateSpec { return &CountAgg{} }

func (c *CountAgg) absorb(v any) error {
	c.Value = 0
	if v == nil {
		return nil
	}
	n, err := scalarToInt64(v)
	if err != nil {
		return fmt.Errorf("COUNT(*): %w", err)
	}
	c.Value = n
	return nil
}

// IntAgg is the result carrier of the exact-integer specs (SumInt, MinInt,
// MaxInt). Value is exact int64 arithmetic — the money shape (minor units) and
// any count-like quantity; a fractional result errors loudly (the field is not
// an integer column — use the float64 specs). Found reports whether any row
// matched: for SumInt, Value 0 with Found false is the empty sum; for
// MinInt/MaxInt the distinction is essential (0 can be a real extreme).
type IntAgg struct {
	fn    string
	field string
	Value int64
	Found bool
}

// SumInt requests the exact integer sum of goField — SUM over an integer
// column. Both drivers deliver it as an exact decimal (Postgres NUMERIC, MySQL
// DECIMAL); the framework normalizes without ever passing through float64.
func SumInt(goField string) *IntAgg { return &IntAgg{fn: "SUM", field: goField} }

// MinInt requests the smallest value of an integer goField — MIN, exact.
func MinInt(goField string) *IntAgg { return &IntAgg{fn: "MIN", field: goField} }

// MaxInt requests the largest value of an integer goField — MAX, exact.
func MaxInt(goField string) *IntAgg { return &IntAgg{fn: "MAX", field: goField} }

func (a *IntAgg) expr(resolve core.FieldResolver, dialect Dialect, qual colQual) (string, error) {
	return aggExpr(a.fn, a.field, resolve, dialect, qual)
}

func (a *IntAgg) fresh() AggregateSpec { return &IntAgg{fn: a.fn, field: a.field} }

func (a *IntAgg) absorb(v any) error {
	a.Value, a.Found = 0, false
	if v == nil {
		return nil
	}
	n, err := scalarToInt64(v)
	if err != nil {
		return fmt.Errorf("%s(%q): %w", a.fn, a.field, err)
	}
	a.Value, a.Found = n, true
	return nil
}

// FloatAgg is the result carrier of the fractional specs (Sum, Avg, Min, Max).
// Found reports whether any row matched — a rule on an average NEEDS to
// distinguish "the average is 0" from "there is nothing to average" (SQL
// returns NULL on an empty set).
type FloatAgg struct {
	fn    string
	field string
	Value float64
	Found bool
}

// Sum requests the sum of a FRACTIONAL goField (areas, rates, measurements) as
// float64. For money — stored as int64 minor units per the framework's money
// doctrine — use SumInt, which keeps the arithmetic exact.
func Sum(goField string) *FloatAgg { return &FloatAgg{fn: "SUM", field: goField} }

// Avg requests the average of goField as float64 (an average is conceptually
// fractional even over integer columns).
func Avg(goField string) *FloatAgg { return &FloatAgg{fn: "AVG", field: goField} }

// Min requests the smallest value of a fractional goField as float64.
func Min(goField string) *FloatAgg { return &FloatAgg{fn: "MIN", field: goField} }

// Max requests the largest value of a fractional goField as float64.
func Max(goField string) *FloatAgg { return &FloatAgg{fn: "MAX", field: goField} }

func (a *FloatAgg) expr(resolve core.FieldResolver, dialect Dialect, qual colQual) (string, error) {
	return aggExpr(a.fn, a.field, resolve, dialect, qual)
}

func (a *FloatAgg) fresh() AggregateSpec { return &FloatAgg{fn: a.fn, field: a.field} }

func (a *FloatAgg) absorb(v any) error {
	a.Value, a.Found = 0, false
	if v == nil {
		return nil
	}
	f, err := scalarToFloat64(v)
	if err != nil {
		return fmt.Errorf("%s(%q): %w", a.fn, a.field, err)
	}
	a.Value, a.Found = f, true
	return nil
}

// aggExpr renders one aggregate call over a resolved field. The column goes
// through qualifyCol — the same rendering the WHERE, the ORDER BY and
// AggregateBy's grouping keys use — because a FROM holding a declared read join
// makes BOTH sides ambiguous: two joined aggregates may both have a "nome", and
// so may the anchor. The anchor id needs no special handling here (an aggregate
// never names it: Count renders COUNT(*)), so the qual the caller passes carries
// only the owner rule.
func aggExpr(fn, goField string, resolve core.FieldResolver, dialect Dialect, qual colQual) (string, error) {
	rf, ok := resolve(goField)
	if !ok {
		return "", fmt.Errorf("aggregate: unknown field %q (not a persisted field of the entity)", goField)
	}
	return fn + "(" + qualifyCol(rf, qual, dialect) + ")", nil
}

// Aggregate executes ONE SELECT computing every requested spec over the same
// criteria — predicate, scope gate (active rows by default; the Query's scope
// can include archived) and the sibling/shared-base LEFT JOINs field
// resolution pulls in — then writes each scalar into its spec. It hydrates
// nothing: with Exists, this is the write-path primitive for business rules
// over scalar facts (cardinality caps, totals, thresholds) — loading whole
// aggregates to answer a scalar question is the anti-pattern it exists to
// kill. Order/limit on the Query are ignored (they don't apply to aggregates).
func (l *AggregateLoader[T]) Aggregate(ctx context.Context, q *criteria.Query, specs ...AggregateSpec) error {
	if len(specs) == 0 {
		return fmt.Errorf("Aggregate: at least one aggregate spec is required")
	}
	joins := &joinedTables{siblings: map[string]*TableSchema{}, hasDeclared: len(rootJoins(l.joins)) > 0}
	resolve := l.resolverRecordingJoins(joins)
	dialect := l.eng.Dialect()
	qual := colQual{owner: len(rootJoins(l.joins)) > 0}
	exprs := make([]string, len(specs))
	for i, s := range specs {
		e, err := s.expr(resolve, dialect, qual)
		if err != nil {
			return err
		}
		exprs[i] = e
	}
	fromJoin, clause, args, err := l.compileFilterJoins(q, joins)
	if err != nil {
		return err
	}
	rows, err := l.eng.Querier().Query(ctx,
		"SELECT "+strings.Join(exprs, ", ")+" FROM "+fromJoin+clause, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	// An ungrouped aggregate SELECT yields exactly one row; raw stays all-nil
	// (= empty set) if the driver defensively yields none.
	raw := make([]any, len(specs))
	if rows.Next() {
		dest := make([]any, len(specs))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, s := range specs {
		if err := s.absorb(raw[i]); err != nil {
			return err
		}
	}
	return nil
}

// scalarToInt64 normalizes a driver-delivered aggregate into an exact int64.
// Drivers hand integer aggregates back as native integers, as decimal TEXT
// (MySQL DECIMAL), or as a driver-native decimal struct (pgx's pgtype.Numeric
// for Postgres NUMERIC) — the latter reached through the stdlib driver.Valuer
// it implements, which renders the exact decimal text. A fractional value is
// rejected — exactness is the whole point of the integer path.
func scalarToInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int32:
		return int64(n), nil
	case int:
		return int64(n), nil
	case []byte:
		return decimalTextToInt64(string(n))
	case string:
		return decimalTextToInt64(n)
	default:
		if u, err := unwrapValuer(v); err != nil {
			return 0, err
		} else if u != nil {
			return scalarToInt64(u)
		}
		return 0, fmt.Errorf("unexpected driver type %T for an exact integer aggregate (use the float64 spec for fractional columns)", v)
	}
}

func decimalTextToInt64(s string) (int64, error) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		for _, c := range s[i+1:] {
			if c != '0' {
				return 0, fmt.Errorf("fractional result %q — the field is not an integer column (use the float64 spec)", s)
			}
		}
		s = s[:i]
	}
	return strconv.ParseInt(s, 10, 64)
}

// scalarToFloat64 normalizes a driver-delivered aggregate into float64
// (native numerics or the decimal TEXT both drivers use for NUMERIC/DECIMAL).
func scalarToFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case []byte:
		return strconv.ParseFloat(string(n), 64)
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		if u, err := unwrapValuer(v); err != nil {
			return 0, err
		} else if u != nil {
			return scalarToFloat64(u)
		}
		return 0, fmt.Errorf("unexpected driver type %T", v)
	}
}

// unwrapValuer surfaces the primitive behind a driver-native scalar wrapper
// (pgx's pgtype.Numeric et al.) via the stdlib driver.Valuer they implement,
// keeping this package free of any driver import. (nil, nil) means the value is
// not a Valuer — the caller falls through to its own unexpected-type error; a
// Valuer returning another Valuer is not followed (one level is the contract:
// Value() yields a plain driver.Value).
func unwrapValuer(v any) (any, error) {
	val, ok := v.(driver.Valuer)
	if !ok {
		return nil, nil
	}
	u, err := val.Value()
	if err != nil {
		return nil, fmt.Errorf("aggregate: driver value %T: %w", v, err)
	}
	if _, again := u.(driver.Valuer); again || u == nil {
		return nil, fmt.Errorf("aggregate: driver value %T did not resolve to a primitive", v)
	}
	return u, nil
}
