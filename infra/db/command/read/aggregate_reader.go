package read

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The type-erased face of AggregateLoader[T]. FindAllEntities and CountEntities
// return the loaded aggregate as domain.Entity (root plus own children, with the
// domain.Managed carrier populated) so a NON-GENERIC caller can load by criteria
// without naming T; Schema exposes the declaration the loader is bound to. They
// are thin adapters over the generic FindAll / Aggregate(Count), so a caller that
// reads through this face reuses the SAME loader the write side already built —
// its func() T constructor, its SharedBase closure, and, crucially, its ONE
// implementation of the criteria→SQL translation: the sibling/shared-base LEFT
// JOINs, the id qualification under a join, the scope gate and the window. A
// second SQL builder over the same TableSchema would have to relearn every one of
// those, and would drift from this one the first time the schema grows a new kind
// of relation.
//
// The query package's AggregateReader interface is satisfied structurally by
// these methods; this file deliberately does not import query, keeping the load
// layer below the view layer.

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

// Schema is the declaration the loader is bound to (its WithSchema schema), nil
// when none is bound or on a nil loader. A read model declared over this loader
// takes its schema from HERE rather than being handed one separately: one source,
// so a view can never be wired to a schema the loader does not read.
func (l *AggregateLoader[T]) Schema() *TableSchema {
	if l == nil {
		return nil
	}
	return l.schema
}
