package read

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// White-box tests of AggregateLoader.hydrateChildren. They drive the child
// SELECT through a scriptable db.Querier (the neutral read seam), so they live
// with the relational model rather than a concrete engine.

func activeScope() criteria.Scope { return criteria.Where(nil).Scope() }

func fakeEngine(queryFn func(sql string, args []any) (Rows, error)) RelationalEngine {
	return fakeRelEngine{q: fakeQuerier{queryFn: queryFn}}
}

func newCovAggLoader(eng RelationalEngine, schema *TableSchema) *AggregateLoader[*covAgg] {
	return NewAggregateLoader[*covAgg](eng, func() *covAgg { return &covAgg{} }).
		WithSchema(schema)
}

// A flat (non-aggregate) entity short-circuits hydrateChildren with nil even
// when the schema declares children.
func TestHydrateChildren_FlatEntityReturnsNil(t *testing.T) {
	schema := NewTableSchema[*aggLoaderTestEntity]("agg_loader").
		PK("id").SoftDelete("deleted_at").
		Child(NewTableSchema[covChild]("cov_children").
			PK("id").FK("agg_loader_id").Field("Label", "label").SoftDelete("deleted_at"))
	l := NewAggregateLoader[*aggLoaderTestEntity](fakeEngine(nil), newAggLoaderTestEntity).
		WithSchema(schema)

	e := &aggLoaderTestEntity{}
	if err := l.hydrateChildren(context.Background(), []*aggLoaderTestEntity{e}, []string{"id-1"}, activeScope()); err != nil {
		t.Fatalf("hydrateChildren on a flat entity must be nil, got %v", err)
	}
}

// A child type with a scanner but no .Child(...) schema is a configuration bug
// surfaced as an error.
func TestHydrateChildren_UndeclaredChildSchemaErrors(t *testing.T) {
	schema := NewTableSchema[*covAgg]("cov_aggs").PK("id").Field("Name", "name").SoftDelete("deleted_at")
	l := newCovAggLoader(fakeEngine(nil), schema).
		WithChildScanner("Ghost", func(Rows) (domain.AggregateValueObject, error) { return nil, nil })

	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
		t.Fatal("expected undeclared-child-schema error")
	}
}

func TestHydrateChildren_ManualChildScanner_QueryAndRowsErrors(t *testing.T) {
	manual := func(Rows) (domain.AggregateValueObject, error) { return covChild{ID: "c1"}, nil }
	for _, tc := range []struct {
		name  string
		query func(sql string, args []any) (Rows, error)
	}{
		{"queryError", func(string, []any) (Rows, error) { return nil, errFakeDB }},
		{"rowsErr", func(string, []any) (Rows, error) { return &fakeDBRows{nextErr: errFakeDB}, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newCovAggLoader(fakeEngine(tc.query), covAggSchema).WithChildScanner("covChild", manual)
			root := &covAgg{Name: "a"}
			root.SetID(domain.NewID(uuid.NewString()))
			if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
				t.Fatalf("%s: expected error", tc.name)
			}
		})
	}
}

func TestHydrateChildren_AutoChildScanner_RowsErr(t *testing.T) {
	query := func(string, []any) (Rows, error) { return &fakeDBRows{nextErr: errFakeDB}, nil }
	l := newCovAggLoader(fakeEngine(query), covAggSchema) // no manual scanner → auto-scan path
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
		t.Fatal("expected auto-scan child rows.Err()")
	}
}
