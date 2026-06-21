package infra

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
)

// The AggregateLoader runs its SELECTs through l.pg.querier() (the pgxPool
// seam), so the manual-scanner root path and the hydrateChildren branches are
// reachable from the in-process fakePool — no live database.

func activeScope() criteria.Scope { return criteria.Where(nil).Scope() }

// --- manual root scanner (findRoots) -----------------------------------------

func manualRootScanner(_ pgx.Row) (*aggLoaderTestEntity, error) {
	e := &aggLoaderTestEntity{}
	e.SetID(domain.NewID(uuid.NewString()))
	return e, nil
}

func TestFindRoots_ManualScanner_HappyPath(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 1}, nil }
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(manualRootScanner)

	got, err := l.FindOne(context.Background(), criteria.Where(nil))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got == nil || got.GetID() == nil {
		t.Error("manual scanner must yield an entity with an id")
	}
}

func TestFindRoots_ManualScanner_QueryError(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return nil, errFake }
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(manualRootScanner)

	if _, err := l.FindOne(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected manual-scanner Query error")
	}
}

func TestFindRoots_ManualScanner_RowsErr(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 0, nextErr: errFake}, nil }
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(manualRootScanner)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected manual-scanner rows.Err()")
	}
}

func TestFindRoots_ManualScanner_EmptyIDErrors(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 1}, nil }
	// Scanner that forgets to populate the id — findRoots must reject it loudly.
	scanner := func(pgx.Row) (*aggLoaderTestEntity, error) { return &aggLoaderTestEntity{}, nil }
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(scanner)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected empty-id error from the manual scanner")
	}
}

// --- hydrateChildren ----------------------------------------------------------

// A flat (non-aggregate) entity short-circuits hydrateChildren with nil even
// when the schema declares children.
func TestHydrateChildren_FlatEntityReturnsNil(t *testing.T) {
	schema := NewTableSchema[*aggLoaderTestEntity]("agg_loader").
		PK("id").SoftDelete("deleted_at").
		Child(NewTableSchema[covChild]("cov_children").
			PK("id").FK("agg_loader_id").Field("Label", "label").SoftDelete("deleted_at"))
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(schema)

	e := &aggLoaderTestEntity{}
	if err := l.hydrateChildren(context.Background(), []*aggLoaderTestEntity{e}, []string{"id-1"}, activeScope()); err != nil {
		t.Fatalf("hydrateChildren on a flat entity must be nil, got %v", err)
	}
}

func newCovAggLoader(pool *fakePool, schema *TableSchema) *AggregateLoader[*covAgg] {
	return NewAggregateLoader[*covAgg](newFakePostgres(pool), func() *covAgg { return &covAgg{} }).
		WithSchema(schema)
}

// A child type with a scanner but no .Child(...) schema is a configuration bug
// surfaced as an error.
func TestHydrateChildren_UndeclaredChildSchemaErrors(t *testing.T) {
	schema := NewTableSchema[*covAgg]("cov_aggs").PK("id").Field("Name", "name").SoftDelete("deleted_at")
	l := newCovAggLoader(newFakePool(), schema).
		WithChildScanner("Ghost", func(pgx.Rows) (domain.AggregateValueObject, error) { return nil, nil })

	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
		t.Fatal("expected undeclared-child-schema error")
	}
}

func TestHydrateChildren_ManualChildScanner_QueryAndRowsErrors(t *testing.T) {
	manual := func(pgx.Rows) (domain.AggregateValueObject, error) { return covChild{ID: "c1"}, nil }
	for _, tc := range []struct {
		name string
		rows func() (pgx.Rows, error)
	}{
		{"queryError", func() (pgx.Rows, error) { return nil, errFake }},
		{"rowsErr", func() (pgx.Rows, error) { return &fakeRows{rows: 0, nextErr: errFake}, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newFakePool()
			pool.queryHandler = func(string, []any) (pgx.Rows, error) { return tc.rows() }
			l := newCovAggLoader(pool, covAggSchema).WithChildScanner("covChild", manual)
			root := &covAgg{Name: "a"}
			root.SetID(domain.NewID(uuid.NewString()))
			if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
				t.Fatalf("%s: expected error", tc.name)
			}
		})
	}
}

func TestHydrateChildren_AutoChildScanner_RowsErr(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 0, nextErr: errFake}, nil }
	l := newCovAggLoader(pool, covAggSchema) // no manual scanner → auto-scan path
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
		t.Fatal("expected auto-scan child rows.Err()")
	}
}
