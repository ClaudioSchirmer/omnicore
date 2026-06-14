package infra

import (
	"context"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// BaseAggregateRepository[T] composes the canonical building blocks of an
// aggregate-aware Repository: embeds BaseRepository[T] (5 writes + New()) and
// holds an internal (public) *AggregateLoader[T] already initialized. Promotes
// FindByID and FindArchivedByID — satisfies domain.Repository[T] and
// domain.ArchivedFinder[T] without the service writing manual wrappers.
//
// When to use (common case):
//
//   - Aggregate-aware Repository with symmetric universal cascade.
//   - FindByID via Load auto-scan or manual scanner registered in the Loader.
//   - Children registered via WithChild[V](r.Loader) or WithChildScanner.
//
// When NOT to use (keep the old pattern of direct BaseRepository[T] embed
// + a separate *AggregateLoader[T]):
//
//   - Custom magic pattern: two distinct read paths on the same aggregate
//     (e.g. read replica + main, or materialized view).
//   - Custom composition where the Loader is not the canonical source of
//     FindByID.
//
// Manual and composed coexist — choose per Repository. ConstraintBinding and
// Config still live on the embedded BaseRepository; setting them after New is
// equivalent to the direct struct-literal of BaseRepository[T].
type BaseAggregateRepository[T domain.Entity] struct {
	BaseRepository[T]
	Loader *AggregateLoader[T]
}

// NewBaseAggregateRepository creates the composition. The newEntity factory is
// shared as a single source between BaseRepository.NewEntity (for Repo.New())
// and AggregateLoader (for entity scan/init) — aligned with the Phase 18
// finding.
//
// ContextName stays empty in both by default — derived from type T via
// TypeName[T]() on first use (Phase: lazy ContextName). Explicit override
// remains available: r.ContextName = "..." + r.Loader.WithContextName("...").
func NewBaseAggregateRepository[T domain.Entity](pg *Postgres, newEntity func() T) BaseAggregateRepository[T] {
	return BaseAggregateRepository[T]{
		BaseRepository: BaseRepository[T]{
			Postgres:  pg,
			NewEntity: newEntity,
		},
		Loader: NewAggregateLoader[T](pg, newEntity),
	}
}

// FindByID delegates to Loader.Load with context.Background() — same semantics
// as the manual wrappers this struct replaces. A caller that needs a custom
// context uses the Loader directly.
func (r *BaseAggregateRepository[T]) FindByID(id domain.ID) (T, error) {
	return r.Loader.Load(context.Background(), id)
}

// FindArchivedByID delegates to Loader.LoadIncludingArchived. Satisfies
// domain.ArchivedFinder[T], which UnarchiveCommandHandler consumes to hydrate
// the archived aggregate before cascading unarchive on the children.
func (r *BaseAggregateRepository[T]) FindArchivedByID(id domain.ID) (T, error) {
	return r.Loader.LoadIncludingArchived(context.Background(), id)
}
