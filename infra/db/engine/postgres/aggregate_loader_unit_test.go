//go:build postgres

package postgres

import (
	"context"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// These tests cover the loader's criteria-compile error branches of findRoots,
// which return before any Query call (driven through the in-process fake
// Postgres). The pure tailClause/quoteIdentifiers helpers are db-internal and
// tested from the db package.

func loaderSchema() *core.TableSchema {
	return core.NewTableSchema[*aggLoaderTestEntity]("agg_loader").
		PK("id").
		SoftDelete("deleted_at")
}

// findRoots compiles the criteria before touching the pool. An unknown field in
// the WHERE predicate makes compileWhere fail, so FindOne/FindAll surface the
// error without dialing — covering the loader's compile-error branch.
func TestFindOne_CriteriaCompileError(t *testing.T) {
	l := read.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(criteria.Eq("NoSuchField", "x"))
	if _, err := l.FindOne(context.Background(), q); err == nil {
		t.Fatal("expected criteria compile error from FindOne")
	}
}

func TestFindAll_CriteriaCompileError(t *testing.T) {
	l := read.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(criteria.Eq("NoSuchField", "x"))
	if _, err := l.FindAll(context.Background(), q); err == nil {
		t.Fatal("expected criteria compile error from FindAll")
	}
}

func TestFindAll_OrderCompileError(t *testing.T) {
	l := read.NewAggregateLoader[*aggLoaderTestEntity](newFakePostgres(newFakePool()), newAggLoaderTestEntity).
		WithSchema(loaderSchema())
	q := criteria.Where(nil).OrderBy("NoSuchField")
	if _, err := l.FindAll(context.Background(), q); err == nil {
		t.Fatal("expected order compile error from FindAll")
	}
}
