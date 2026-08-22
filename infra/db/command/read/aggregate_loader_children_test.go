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

// fakeEngineWithMaps wires both the row seam (queryFn, used by the auto-scan
// path) and the map seam (mapsFn, used by the manual-scanner path via QueryMaps).
func fakeEngineWithMaps(
	queryFn func(sql string, args []any) (Rows, error),
	mapsFn func(sql string, args []any) ([]map[string]any, error),
) RelationalEngine {
	return fakeRelEngine{q: fakeQuerier{queryFn: queryFn, mapsFn: mapsFn}}
}

func newCovAggLoader(eng RelationalEngine, schema *TableSchema) *AggregateLoader[*covAgg] {
	return NewAggregateLoader[*covAgg](eng, func() *covAgg { return &covAgg{} }).
		WithSchema(schema)
}

// A flat (non-aggregate) entity short-circuits hydrateChildren with nil even
// when the schema declares children.
func TestHydrateChildren_FlatEntityReturnsNil(t *testing.T) {
	schema := NewTableSchema[*aggLoaderTestEntity]("agg_loader").
		ID("id").DeletedAt("deleted_at").
		Child(NewTableSchema[covChild]("cov_children").
			ID("id").ParentID("agg_loader_id").Field("Label", "label").DeletedAt("deleted_at"))
	l := NewAggregateLoader[*aggLoaderTestEntity](fakeEngine(nil), newAggLoaderTestEntity).
		WithSchema(schema)

	e := &aggLoaderTestEntity{}
	if err := l.hydrateChildren(context.Background(), []*aggLoaderTestEntity{e}, []string{"id-1"}, activeScope()); err != nil {
		t.Fatalf("hydrateChildren on a flat entity must be nil, got %v", err)
	}
}

func TestHydrateChildren_RowsErrPropagates(t *testing.T) {
	query := func(string, []any) (Rows, error) { return &fakeDBRows{nextErr: errFakeDB}, nil }
	l := newCovAggLoader(fakeEngine(query), covAggSchema)
	root := &covAgg{Name: "a"}
	root.SetID(domain.NewID(uuid.NewString()))
	if err := l.hydrateChildren(context.Background(), []*covAgg{root}, []string{"r1"}, activeScope()); err == nil {
		t.Fatal("expected auto-scan child rows.Err()")
	}
}
