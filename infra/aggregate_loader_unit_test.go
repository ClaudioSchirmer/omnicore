package infra

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/criteria"
)

// The AggregateLoader runs its SELECTs through l.pg.Pool(), which returns the
// concrete *pgxpool.Pool. A fake pgxPool yields nil there (Pool() asserts the
// real type), so the live-query path of findRoots/hydrateChildren cannot be
// driven from the in-process seam. These tests cover what IS reachable without
// a pool: the pure tailClause/quoteIdentifiers helpers and the criteria
// compilation error branches of findRoots, which return before any Query call.

func loaderSchema() *TableSchema {
	return NewTableSchema[*aggLoaderTestEntity]("agg_loader").
		PK("ID", "id").
		SoftDelete("deleted_at")
}

func TestTailClause_AllParts(t *testing.T) {
	cases := []struct {
		name   string
		clause string
		order  string
		limit  int64
		want   string
	}{
		{"empty", "", "", 0, ""},
		{"whereOnly", "WHERE x = $1", "", 0, " WHERE x = $1"},
		{"orderOnly", "", "ORDER BY x", 0, " ORDER BY x"},
		{"limitOnly", "", "", 5, " LIMIT 5"},
		{"all", "WHERE x = $1", "ORDER BY x DESC", 10, " WHERE x = $1 ORDER BY x DESC LIMIT 10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tailClause(c.clause, c.order, c.limit); got != c.want {
				t.Errorf("tailClause = %q, want %q", got, c.want)
			}
		})
	}
}

func TestQuoteIdentifiers(t *testing.T) {
	got := quoteIdentifiers([]string{"id", "name", "deleted_at"})
	want := []string{"id", "name", "deleted_at"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestQuoteIdentifiers_PanicsOnBadIdentifier(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on invalid identifier")
		}
	}()
	quoteIdentifiers([]string{"bad; DROP TABLE"})
}

// findRoots compiles the criteria before touching the pool. An unknown field in
// the WHERE predicate makes compileWhere fail, so FindOne/FindAll surface the
// error without dialing — covering the loader's compile-error branch.
func TestFindOne_CriteriaCompileError(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(criteria.Eq("NoSuchField", "x"))
	if _, err := l.FindOne(context.Background(), q); err == nil {
		t.Fatal("expected criteria compile error from FindOne")
	}
}

func TestFindAll_CriteriaCompileError(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(criteria.Eq("NoSuchField", "x"))
	if _, err := l.FindAll(context.Background(), q); err == nil {
		t.Fatal("expected criteria compile error from FindAll")
	}
}

func TestFindAll_OrderCompileError(t *testing.T) {
	l := NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(nil).OrderBy("NoSuchField")
	if _, err := l.FindAll(context.Background(), q); err == nil {
		t.Fatal("expected order compile error from FindAll")
	}
}
