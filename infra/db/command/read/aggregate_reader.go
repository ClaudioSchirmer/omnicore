package read

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The type-erased face of AggregateLoader[T]: FindAllEntities and CountEntities
// return the loaded aggregate as domain.Entity (root plus own children, with the
// domain.Managed carrier populated) so a non-generic caller — the relational
// ViewReader — can load by criteria without naming T. They are thin adapters
// over the generic FindAll / Aggregate(Count), so the
// relational read path reuses the SAME loader (its func() T constructor, its
// SharedBase closure) the write side already built. The query package's
// query.RelationalReader interface is satisfied structurally by these methods;
// this file deliberately does not import query, keeping the load layer below the
// view layer.

// FindAllEntities loads every aggregate matching q, honoring its order/window/
// scope, and returns them type-erased. Empty slice (not NotFound) on no match.
func (l *AggregateLoader[T]) FindAllEntities(ctx context.Context, q *criteria.Query) ([]domain.Entity, error) {
	ts, err := l.FindAll(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Entity, len(ts))
	for i := range ts {
		out[i] = ts[i]
	}
	return out, nil
}

// CountEntities returns how many roots match q — COUNT(*) under the same filter
// and scope, nothing materialized — via the read-side aggregate DSL.
func (l *AggregateLoader[T]) CountEntities(ctx context.Context, q *criteria.Query) (int64, error) {
	total := Count()
	if err := l.Aggregate(ctx, q, total); err != nil {
		return 0, err
	}
	return total.Value, nil
}

// BoundTable is the root table the loader reads — its WithSchema table (empty
// when no schema is bound, or on a nil loader). The RelationalSource boot guard
// checks it equals the view's own schema table, so a view can never be wired to
// the wrong entity's loader.
func (l *AggregateLoader[T]) BoundTable() string {
	if l == nil || l.schema == nil {
		return ""
	}
	return l.schema.Table()
}
