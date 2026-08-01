package query

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// RelationalReader is the type-erased face of the aggregate loader that a
// RelationalSource() view hands the framework at declaration time. It loads a
// whole aggregate — root plus its own children, with the domain.Managed carrier
// populated — from the relational backend (SoR) through a neutral criteria.Query,
// WITHOUT the caller naming the entity's Go type.
//
// read.AggregateLoader[T] satisfies it structurally (via FindAllEntities /
// CountEntities / BoundTable), so a view carries its repository's
// already-built loader — repo.Loader, threaded down through the view constructor
// — and the relational ViewReader consults it by view name. That is what lets a
// relational read reuse the exact typed loader the write side already built (its
// func() T constructor, its SharedBase closure), with no reflection and no
// per-entity registry: the loader rides on the ViewDefinition the bootstrap
// already collects.
//
// It lives in this package (not read's) so ViewDefinition can hold it without
// the view layer importing the load layer; criteria is a low-level IR shared by
// both, so query -> criteria introduces no cycle.
type RelationalReader interface {
	// FindAllEntities loads every aggregate matching q — root plus own children,
	// managed columns populated — honoring q's order, window (limit/offset) and
	// scope (active / include-archived / only-archived). An empty slice, not an
	// error, when nothing matches; the by-id read runs it with a criteria.ByID
	// filter + Limit(1).
	FindAllEntities(ctx context.Context, q *criteria.Query) ([]domain.Entity, error)
	// CountEntities returns how many roots match q — COUNT(*) under the same
	// filter and scope, nothing materialized — backing the onlyTotal read.
	CountEntities(ctx context.Context, q *criteria.Query) (int64, error)
	// BoundTable is the root table the loader is bound to (its WithSchema table).
	// The boot guard checks it equals the RelationalSource view's own schema
	// table — a mismatch means the view was handed the wrong entity's loader, a
	// boot-fail programmer error.
	BoundTable() string
}
