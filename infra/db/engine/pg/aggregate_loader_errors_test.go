package pg

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// The db.AggregateLoader runs its SELECTs through the engine's read seam, backed
// here by the in-process fakePool — no live database. These cover the manual
// root-scanner (findRoots) branches; the hydrateChildren branches are covered
// from the db package against the neutral Querier seam.

// --- manual root scanner (findRoots) -----------------------------------------

func manualRootScanner(_ db.Row) (*aggLoaderTestEntity, error) {
	e := &aggLoaderTestEntity{}
	e.SetID(domain.NewID(uuid.NewString()))
	return e, nil
}

func TestFindRoots_ManualScanner_HappyPath(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 1}, nil }
	l := db.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
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
	l := db.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(manualRootScanner)

	if _, err := l.FindOne(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected manual-scanner Query error")
	}
}

func TestFindRoots_ManualScanner_RowsErr(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 0, nextErr: errFake}, nil }
	l := db.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(manualRootScanner)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected manual-scanner rows.Err()")
	}
}

func TestFindRoots_ManualScanner_EmptyIDErrors(t *testing.T) {
	pool := newFakePool()
	pool.queryHandler = func(string, []any) (pgx.Rows, error) { return &fakeRows{rows: 1}, nil }
	// Scanner that forgets to populate the id — findRoots must reject it loudly.
	scanner := func(db.Row) (*aggLoaderTestEntity, error) { return &aggLoaderTestEntity{}, nil }
	l := db.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(pool), newAggLoaderTestEntity).
		WithSchema(loaderSchema()).WithRootScanner(scanner)

	if _, err := l.FindAll(context.Background(), criteria.Where(nil)); err == nil {
		t.Fatal("expected empty-id error from the manual scanner")
	}
}
