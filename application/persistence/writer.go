package persistence

import (
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Writer is the application-layer persistence port consumed by the Auto
// Command Handlers and the canonical manual path. It carries the same
// read-side and write-side surface as domain.Repository[T] PLUS the
// variadic WriteOption[T] on every write verb.
//
// Why a separate port instead of widening domain.Repository[T]: the
// WriteOption / TxHandle / hook types live in application/persistence/
// (they reference *AppContext, which itself lives under
// application/configuration). Pulling that import into the domain layer
// would violate the dependency rule (domain → stdlib + google/uuid
// only). Keeping the variadic out of domain.Repository[T] preserves the
// rule; Writer[T] is the typed write port at the application layer
// where the variadic is natural.
//
// infra.BaseRepository[T] implements BOTH domain.Repository[T] AND this
// Writer[T] interface — the underlying Postgres adapter already carries
// the variadic surface, so the BaseRepository methods come for free.
// Consumer code that wants the canonical handler signature accepts
// persistence.Writer[T]; consumer code that needs only the
// domain-layer read+write contract continues to depend on
// domain.Repository[T] without change.
type Writer[T any] interface {
	Insert(ctx domain.Context, insertable domain.Insertable, opts ...WriteOption[T]) (domain.ID, error)
	Update(ctx domain.Context, updatable domain.Updatable, opts ...WriteOption[T]) error
	Delete(ctx domain.Context, deletable domain.Deletable, opts ...WriteOption[T]) error
	Archive(ctx domain.Context, archivable domain.Archivable, opts ...WriteOption[T]) error
	Unarchive(ctx domain.Context, unarchivable domain.Unarchivable, opts ...WriteOption[T]) error
	FindByID(id domain.ID) (T, error)
	New() T
}
