package read

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The grouped-aggregate contract: AggregateBy compiles ONE SELECT with GROUP BY
// over the same resolution + scope gate as Aggregate, orders groups by key for
// determinism, and hands every group its own fresh typed carriers — the
// passed specs stay pure templates.

type ecGroupEntity struct {
	domain.BaseEntity
	Category string
	Cents    int64
	Area     float64
}

func (e *ecGroupEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (e *ecGroupEntity) BuildRules(string, domain.Service, *domain.Rules) {}

// ecGroupLoader fakes the grouped result set: rows[i] is one GROUP BY row,
// scanned positionally (keys first, then one scalar per spec). It reports the
// executed SQL and the query count.
func ecGroupLoader(rows [][]any) (*AggregateLoader[*ecGroupEntity], *string, *int) {
	var gotSQL string
	var queries int
	l := NewAggregateLoader[*ecGroupEntity](fakeEngine(func(sql string, _ []any) (Rows, error) {
		gotSQL = sql
		queries++
		return &fakeDBRows{rows: len(rows), scan: func(idx int, dest []any) error {
			for i := range dest {
				*(dest[i].(*any)) = rows[idx][i]
			}
			return nil
		}}, nil
	}), func() *ecGroupEntity { return &ecGroupEntity{} }).
		WithSchema(NewTableSchema[*ecGroupEntity]("listings").
			ID("id").Field("Category", "category").Field("Cents", "monthly_rent").Field("Area", "built_area").SoftDelete("deleted_at"))
	return l, &gotSQL, &queries
}

func TestAggregateBy_GroupedFactsOneQuery(t *testing.T) {
	// Two categories; keys arrive as the []byte MySQL delivers for text — the
	// framework must normalize them to string.
	l, gotSQL, queries := ecGroupLoader([][]any{
		{[]byte("books"), int64(2), []byte("1170.00")},
		{"tools", int64(1), []byte("500")},
	})

	total := Count()
	cents := SumInt("Cents")
	groups, err := l.AggregateBy(context.Background(), nil, By("Category"), total, cents)
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if *queries != 1 {
		t.Fatalf("every group and every fact must ride ONE SELECT, ran %d", *queries)
	}
	if !strings.HasPrefix(*gotSQL, "SELECT category, COUNT(*), SUM(monthly_rent) FROM listings WHERE ") {
		t.Errorf("keys then specs must compile in call order over resolved columns, got %q", *gotSQL)
	}
	if !strings.Contains(*gotSQL, "deleted_at IS NULL") {
		t.Errorf("the default scope gate (active-only) must apply, got %q", *gotSQL)
	}
	if !strings.HasSuffix(*gotSQL, " GROUP BY category ORDER BY category") {
		t.Errorf("grouping must GROUP BY and ORDER BY the key for deterministic output, got %q", *gotSQL)
	}
	if len(groups) != 2 {
		t.Fatalf("len(groups) = %d, want 2", len(groups))
	}
	if groups[0].Key("Category") != "books" || groups[0].KeyString("Category") != "books" {
		t.Errorf("a []byte key must normalize to string, got %#v", groups[0].Key("Category"))
	}
	if n := GroupResult(groups[0], total); n.Value != 2 {
		t.Errorf("books COUNT = %d, want 2", n.Value)
	}
	if s := GroupResult(groups[0], cents); s.Value != 1170 || !s.Found {
		t.Errorf("books SUM = (%d, %v), want (1170, true) — exact minor units", s.Value, s.Found)
	}
	if n := GroupResult(groups[1], total); n.Value != 1 {
		t.Errorf("tools COUNT = %d, want 1", n.Value)
	}
	if s := GroupResult(groups[1], cents); s.Value != 500 || !s.Found {
		t.Errorf("tools SUM = (%d, %v), want (500, true)", s.Value, s.Found)
	}
	// The passed specs are templates: they must carry NO result after the call.
	if total.Value != 0 || cents.Value != 0 || cents.Found {
		t.Errorf("templates must stay pure — got Count=%d SumInt=(%d,%v)", total.Value, cents.Value, cents.Found)
	}
}

func TestAggregateBy_MultiKeyAndFloatSpecs(t *testing.T) {
	l, gotSQL, _ := ecGroupLoader([][]any{
		{"books", int64(1050), int64(1050), []byte("20.25")},
	})

	hi := MaxInt("Cents")
	avg := Avg("Area")
	groups, err := l.AggregateBy(context.Background(), nil, By("Category", "Cents"), hi, avg)
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if !strings.HasPrefix(*gotSQL, "SELECT category, monthly_rent, MAX(monthly_rent), AVG(built_area) FROM listings") {
		t.Errorf("multi-key grouping must list every key before the specs, got %q", *gotSQL)
	}
	if !strings.HasSuffix(*gotSQL, " GROUP BY category, monthly_rent ORDER BY category, monthly_rent") {
		t.Errorf("every key must appear in GROUP BY and ORDER BY, in declaration order, got %q", *gotSQL)
	}
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].Key("Cents") != int64(1050) {
		t.Errorf("a native integer key must pass through untouched, got %#v", groups[0].Key("Cents"))
	}
	if groups[0].KeyString("Cents") != "1050" {
		t.Errorf("KeyString must render a non-string key, got %q", groups[0].KeyString("Cents"))
	}
	if v := GroupResult(groups[0], hi); v.Value != 1050 || !v.Found {
		t.Errorf("MAX = (%d, %v), want (1050, true)", v.Value, v.Found)
	}
	if v := GroupResult(groups[0], avg); v.Value != 20.25 || !v.Found {
		t.Errorf("AVG = (%v, %v), want (20.25, true)", v.Value, v.Found)
	}
}

func TestAggregateBy_EmptySetYieldsNoGroups(t *testing.T) {
	l, _, _ := ecGroupLoader(nil)
	groups, err := l.AggregateBy(context.Background(), nil, By("Category"), Count())
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("an empty set has ZERO groups (no NULL-keyed placeholder), got %d", len(groups))
	}
}

func TestAggregateBy_NullKeyAndNullScalar(t *testing.T) {
	// A NULL group key is a legitimate group (rows where the column IS NULL);
	// a NULL scalar inside an existing group (e.g. SUM over all-NULL values)
	// must report Found=false for that group only.
	l, _, _ := ecGroupLoader([][]any{
		{nil, nil},
		{"tools", []byte("500")},
	})

	cents := SumInt("Cents")
	groups, err := l.AggregateBy(context.Background(), nil, By("Category"), cents)
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if groups[0].Key("Category") != nil || groups[0].KeyString("Category") != "" {
		t.Errorf("a NULL key must stay nil (KeyString renders \"\"), got %#v", groups[0].Key("Category"))
	}
	if v := GroupResult(groups[0], cents); v.Found || v.Value != 0 {
		t.Errorf("a NULL scalar in a group = (%d, %v), want (0, false)", v.Value, v.Found)
	}
	if v := GroupResult(groups[1], cents); !v.Found || v.Value != 500 {
		t.Errorf("the sibling group must keep its own result, got (%d, %v)", v.Value, v.Found)
	}
}

func TestAggregateBy_CriteriaFilterApplies(t *testing.T) {
	var gotArgs []any
	var gotSQL string
	l := NewAggregateLoader[*ecGroupEntity](fakeEngine(func(sql string, args []any) (Rows, error) {
		gotSQL, gotArgs = sql, args
		return &fakeDBRows{}, nil
	}), func() *ecGroupEntity { return &ecGroupEntity{} }).
		WithSchema(NewTableSchema[*ecGroupEntity]("listings").
			ID("id").Field("Category", "category").Field("Cents", "monthly_rent").SoftDelete("deleted_at"))

	q := criteria.Where(criteria.Gt("Cents", 100))
	if _, err := l.AggregateBy(context.Background(), q, By("Category"), Count()); err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if !strings.Contains(gotSQL, "monthly_rent > $1") {
		t.Errorf("the predicate must resolve and parameterize before the GROUP BY, got %q", gotSQL)
	}
	if !strings.Contains(gotSQL, "WHERE") || strings.Index(gotSQL, "WHERE") > strings.Index(gotSQL, "GROUP BY") {
		t.Errorf("WHERE must precede GROUP BY, got %q", gotSQL)
	}
	if len(gotArgs) != 1 {
		t.Errorf("expected 1 arg, got %v", gotArgs)
	}
}

func TestAggregateBy_ValidationErrors(t *testing.T) {
	l, _, queries := ecGroupLoader(nil)
	ctx := context.Background()

	if _, err := l.AggregateBy(ctx, nil, nil, Count()); err == nil {
		t.Error("a nil grouping must error loudly (Aggregate is the ungrouped form)")
	}
	if _, err := l.AggregateBy(ctx, nil, By(), Count()); err == nil {
		t.Error("zero grouping fields must error loudly")
	}
	if _, err := l.AggregateBy(ctx, nil, By("Category")); err == nil {
		t.Error("zero specs must error loudly")
	}
	if _, err := l.AggregateBy(ctx, nil, By("Nope"), Count()); err == nil {
		t.Error("an unresolvable grouping field must error loudly")
	}
	if _, err := l.AggregateBy(ctx, nil, By("Category"), SumInt("Nope")); err == nil {
		t.Error("an unresolvable spec field must error loudly")
	}
	shared := Count()
	if _, err := l.AggregateBy(ctx, nil, By("Category"), shared, shared); err == nil {
		t.Error("the same spec instance twice must error loudly — per-group lookup is by instance")
	}
	if *queries != 0 {
		t.Error("validation errors must not reach the database")
	}
}

func TestAggregateBy_AccessorsPanicOnForeignHandles(t *testing.T) {
	l, _, _ := ecGroupLoader([][]any{{"books", int64(1)}})
	total := Count()
	groups, err := l.AggregateBy(context.Background(), nil, By("Category"), total)
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Key on a non-grouping field must panic — nil would masquerade as a NULL key")
			}
		}()
		groups[0].Key("Cents")
	}()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("GroupResult with a spec from another call must panic loudly")
			}
		}()
		GroupResult(groups[0], Count())
	}()
}

func TestAggregateBy_KeyNormalization_Valuer(t *testing.T) {
	// A driver-native wrapper key (pgtype.Numeric-like) unwraps through its
	// stdlib Valuer, same one-level contract as the scalar converters.
	l, _, _ := ecGroupLoader([][]any{{fakeNumericValuer{text: "42"}, int64(1)}})
	groups, err := l.AggregateBy(context.Background(), nil, By("Category"), Count())
	if err != nil {
		t.Fatalf("AggregateBy: %v", err)
	}
	if groups[0].Key("Category") != "42" {
		t.Errorf("a Valuer key must unwrap to its primitive, got %#v", groups[0].Key("Category"))
	}

	l2, _, _ := ecGroupLoader([][]any{{fakeNilValuer{}, int64(1)}})
	if _, err := l2.AggregateBy(context.Background(), nil, By("Category"), Count()); err == nil {
		t.Error("a Valuer key resolving to nil is not a primitive — must error")
	}
}
