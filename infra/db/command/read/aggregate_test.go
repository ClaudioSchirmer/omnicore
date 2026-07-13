package read

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The aggregate DSL contract: any combination of scalar facts over the same
// criteria compiles to ONE SELECT (same resolution + scope gate as
// FindOne/FindAll), and each typed spec absorbs its own scalar — with the
// money doctrine (exact int64, fractions rejected) on the integer specs and
// the Found flag distinguishing "the value is 0" from "no row matched".

type ecMoneyEntity struct {
	domain.BaseEntity
	Cents int64
	Area  float64
}

func (e *ecMoneyEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *ecMoneyEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// ecAggLoader fakes the single aggregate row: scalars[i] lands in the i-th
// SELECT expression (nil = SQL NULL). It reports the executed SQL and how many
// queries ran — the DSL's whole point is that the answer is exactly one.
func ecAggLoader(scalars []any) (*AggregateLoader[*ecMoneyEntity], *string, *int) {
	var gotSQL string
	var queries int
	l := NewAggregateLoader[*ecMoneyEntity](fakeEngine(func(sql string, _ []any) (Rows, error) {
		gotSQL = sql
		queries++
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			if len(dest) != len(scalars) {
				return fmt.Errorf("scan arity %d, want %d", len(dest), len(scalars))
			}
			for i := range dest {
				*(dest[i].(*any)) = scalars[i]
			}
			return nil
		}}, nil
	}), func() *ecMoneyEntity { return &ecMoneyEntity{} }).
		WithSchema(NewTableSchema[*ecMoneyEntity]("listings").
			PK("id").Field("Cents", "monthly_rent").Field("Area", "built_area").SoftDelete("deleted_at"))
	return l, &gotSQL, &queries
}

func TestAggregate_ManyFactsOneQuery(t *testing.T) {
	// COUNT native int64; SUM/AVG as the decimal TEXT both drivers deliver.
	l, gotSQL, queries := ecAggLoader([]any{int64(3), []byte("1670.00"), []byte("20.25")})

	total := Count()
	cents := SumInt("Cents")
	area := Avg("Area")
	if err := l.Aggregate(context.Background(), nil, total, cents, area); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if *queries != 1 {
		t.Fatalf("every requested fact must ride ONE SELECT, ran %d", *queries)
	}
	if !strings.HasPrefix(*gotSQL, "SELECT COUNT(*), SUM(monthly_rent), AVG(built_area) FROM listings WHERE ") {
		t.Errorf("specs must compile in call order over resolved columns with a separated WHERE, got %q", *gotSQL)
	}
	if !strings.Contains(*gotSQL, "deleted_at IS NULL") {
		t.Errorf("the default scope gate (active-only) must apply, got %q", *gotSQL)
	}
	if total.Value != 3 {
		t.Errorf("Count = %d, want 3", total.Value)
	}
	// 10.50 + 1.20 + 5.00 in minor units: exact integer arithmetic end to end.
	if cents.Value != 1670 || !cents.Found {
		t.Errorf("SumInt = (%d, %v), want (1670, true)", cents.Value, cents.Found)
	}
	if area.Value != 20.25 || !area.Found {
		t.Errorf("Avg = (%v, %v), want (20.25, true)", area.Value, area.Found)
	}
}

func TestAggregate_MinMax(t *testing.T) {
	l, gotSQL, _ := ecAggLoader([]any{int64(120), int64(1050), []byte("10.5"), []byte("30")})

	lo, hi := MinInt("Cents"), MaxInt("Cents")
	loA, hiA := Min("Area"), Max("Area")
	if err := l.Aggregate(context.Background(), nil, lo, hi, loA, hiA); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !strings.HasPrefix(*gotSQL, "SELECT MIN(monthly_rent), MAX(monthly_rent), MIN(built_area), MAX(built_area) FROM listings") {
		t.Errorf("Min/Max must compile over the resolved columns, got %q", *gotSQL)
	}
	if lo.Value != 120 || !lo.Found || hi.Value != 1050 || !hi.Found {
		t.Errorf("MinInt/MaxInt = (%d,%v)/(%d,%v), want (120,true)/(1050,true)", lo.Value, lo.Found, hi.Value, hi.Found)
	}
	if loA.Value != 10.5 || hiA.Value != 30 {
		t.Errorf("Min/Max = %v/%v, want 10.5/30", loA.Value, hiA.Value)
	}
}

func TestAggregate_EmptySetSemantics(t *testing.T) {
	// COUNT is 0, never NULL; every field aggregate comes back NULL.
	l, _, _ := ecAggLoader([]any{int64(0), nil, nil, nil})

	total := Count()
	cents := SumInt("Cents")
	avg := Avg("Area")
	hi := MaxInt("Cents")
	if err := l.Aggregate(context.Background(), nil, total, cents, avg, hi); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if total.Value != 0 {
		t.Errorf("Count over an empty set = %d, want 0", total.Value)
	}
	if cents.Value != 0 || cents.Found {
		t.Errorf("SumInt over an empty set = (%d, %v), want (0, false) — the empty sum", cents.Value, cents.Found)
	}
	if avg.Found || avg.Value != 0 {
		t.Errorf("Avg over an empty set = (%v, %v) — 'average is 0' and 'nothing to average' are different business facts; want (0, false)", avg.Value, avg.Found)
	}
	if hi.Found || hi.Value != 0 {
		t.Errorf("MaxInt over an empty set must report Found=false (0 can be a real extreme), got (%d, %v)", hi.Value, hi.Found)
	}
}

func TestAggregate_SpecReuseResets(t *testing.T) {
	cents := SumInt("Cents")
	l1, _, _ := ecAggLoader([]any{[]byte("1170")})
	if err := l1.Aggregate(context.Background(), nil, cents); err != nil || cents.Value != 1170 || !cents.Found {
		t.Fatalf("first run = (%d, %v, %v), want (1170, true, nil)", cents.Value, cents.Found, err)
	}
	l2, _, _ := ecAggLoader([]any{nil})
	if err := l2.Aggregate(context.Background(), nil, cents); err != nil || cents.Value != 0 || cents.Found {
		t.Fatalf("a reused spec must reset on absorb, got (%d, %v, %v)", cents.Value, cents.Found, err)
	}
}

func TestAggregate_MoneyDoctrine_ExactAcrossDriverTypes(t *testing.T) {
	// 1050 + 120 = 1170 minor units, whatever shape the driver picks.
	for _, driverValue := range []any{int64(1170), "1170", []byte("1170"), []byte("1170.00")} {
		l, _, _ := ecAggLoader([]any{driverValue})
		cents := SumInt("Cents")
		if err := l.Aggregate(context.Background(), nil, cents); err != nil {
			t.Fatalf("SumInt(%T): %v", driverValue, err)
		}
		if cents.Value != 1170 {
			t.Errorf("SumInt(%T) = %d, want 1170", driverValue, cents.Value)
		}
	}
}

func TestAggregate_FractionalIntoIntSpecErrorsLoudly(t *testing.T) {
	l, _, _ := ecAggLoader([]any{[]byte("10.5")})
	err := l.Aggregate(context.Background(), nil, SumInt("Cents"))
	if err == nil {
		t.Fatal("a fractional scalar into an exact-integer spec must error, not truncate")
	}
	if !strings.Contains(err.Error(), `SUM("Cents")`) {
		t.Errorf("the error must name the offending spec, got %q", err.Error())
	}
}

func TestAggregate_NoSpecsErrors(t *testing.T) {
	l, _, queries := ecAggLoader(nil)
	if err := l.Aggregate(context.Background(), nil); err == nil {
		t.Fatal("Aggregate with no specs is a programming error — must error loudly")
	}
	if *queries != 0 {
		t.Error("no specs must not reach the database")
	}
}

func TestAggregate_UnknownFieldErrors(t *testing.T) {
	l, _, queries := ecAggLoader(nil)
	if err := l.Aggregate(context.Background(), nil, SumInt("Nope")); err == nil {
		t.Fatal("an unresolvable aggregate field must error loudly")
	}
	if *queries != 0 {
		t.Error("a resolution error must not reach the database")
	}
}

func TestAggregate_CriteriaFilterAndArgs(t *testing.T) {
	var gotArgs []any
	var gotSQL string
	l := NewAggregateLoader[*ecMoneyEntity](fakeEngine(func(sql string, args []any) (Rows, error) {
		gotSQL, gotArgs = sql, args
		return &fakeDBRows{rows: 1, scan: func(_ int, dest []any) error {
			*(dest[0].(*any)) = int64(1)
			return nil
		}}, nil
	}), func() *ecMoneyEntity { return &ecMoneyEntity{} }).
		WithSchema(NewTableSchema[*ecMoneyEntity]("listings").
			PK("id").Field("Cents", "monthly_rent").Field("Area", "built_area").SoftDelete("deleted_at"))

	total := Count()
	q := criteria.Where(criteria.Gt("Cents", 100))
	if err := l.Aggregate(context.Background(), q, total); err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if !strings.Contains(gotSQL, "monthly_rent > $1") {
		t.Errorf("the predicate must resolve and parameterize, got %q", gotSQL)
	}
	if len(gotArgs) != 1 {
		t.Errorf("expected 1 arg, got %v", gotArgs)
	}
}

// ─── scalar converters — driver-type normalization ───────────────────────────

func TestScalarConverters_DriverTypeTable(t *testing.T) {
	// int64 exact path
	for _, tc := range []struct {
		in   any
		want int64
	}{{int64(7), 7}, {int32(7), 7}, {int(7), 7}, {"42", 42}, {[]byte("42.000"), 42}} {
		if got, err := scalarToInt64(tc.in); err != nil || got != tc.want {
			t.Errorf("scalarToInt64(%T %v) = (%d, %v), want %d", tc.in, tc.in, got, err, tc.want)
		}
	}
	if _, err := scalarToInt64(3.14); err == nil {
		t.Error("scalarToInt64 must reject a float driver value (wrong column for the exact path)")
	}
	// float path
	for _, tc := range []struct {
		in   any
		want float64
	}{{float64(1.5), 1.5}, {float32(0.5), 0.5}, {int64(2), 2}, {int32(2), 2}, {int(2), 2}, {"2.25", 2.25}} {
		if got, err := scalarToFloat64(tc.in); err != nil || got != tc.want {
			t.Errorf("scalarToFloat64(%T %v) = (%v, %v), want %v", tc.in, tc.in, got, err, tc.want)
		}
	}
	if _, err := scalarToFloat64(struct{}{}); err == nil {
		t.Error("scalarToFloat64 must reject an unexpected driver type")
	}
}

// fakeNumericValuer mimics a driver-native decimal struct (pgx's pgtype.Numeric
// is the live case: scanning Postgres NUMERIC into `any` yields the struct, and
// its stdlib driver.Valuer renders the exact decimal text).
type fakeNumericValuer struct {
	text string
	err  error
}

func (v fakeNumericValuer) Value() (driver.Value, error) { return v.text, v.err }

type fakeNilValuer struct{}

func (fakeNilValuer) Value() (driver.Value, error) { return nil, nil }

func TestScalarConverters_DriverValuer(t *testing.T) {
	if got, err := scalarToInt64(fakeNumericValuer{text: "1670"}); err != nil || got != 1670 {
		t.Errorf("scalarToInt64(Valuer 1670) = (%d, %v), want 1670 — the pgtype.Numeric path", got, err)
	}
	if got, err := scalarToFloat64(fakeNumericValuer{text: "20.25"}); err != nil || got != 20.25 {
		t.Errorf("scalarToFloat64(Valuer 20.25) = (%v, %v), want 20.25", got, err)
	}
	if _, err := scalarToInt64(fakeNumericValuer{text: "20.25"}); err == nil {
		t.Error("a fractional Valuer must still be rejected by the exact path")
	}
	if _, err := scalarToInt64(fakeNumericValuer{err: fmt.Errorf("boom")}); err == nil {
		t.Error("a Valuer error must surface, not fall through")
	}
	if _, err := scalarToFloat64(fakeNilValuer{}); err == nil {
		t.Error("a Valuer resolving to nil is not a primitive — must error")
	}
}
