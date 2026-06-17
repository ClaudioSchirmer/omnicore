package domain

// Reader is the read port at the domain layer. It speaks pure business
// vocabulary — an ID in, the live entity out — and carries no
// request-scoped concern (no context, no cancellation, no actor). The
// concrete implementation (infra) uses whatever ctx it needs internally;
// the domain contract does not pronounce it.
type Reader[TEntity any] interface {
	FindByID(id ID) (TEntity, error)
	New() TEntity
}

// Writer is the write port at the domain layer. Each method takes a
// ValidEntity (the validation attestation produced only by the domain
// Get* functions) and returns the persistence outcome. It is NOT generic
// on the entity type because the ValidEntity flavors already are the
// contract — Insertable/Updatable/Archivable/Unarchivable/Deletable carry
// the source entity internally.
//
// Writer carries NO request-scoped concern (no context, no actor, no
// lifecycle hooks). Those are infrastructure details bound BELOW the port:
// the application obtains a request-scoped Writer via
// persistence.ScopedRepository[T].Scope(ctx, opts...), and the infra
// adapter the Scope returns closes over the ctx + hooks. The domain port
// stays pure.
type Writer interface {
	Insert(i Insertable) (ID, error)
	Update(u Updatable) error
	Archive(a Archivable) error
	Unarchive(u Unarchivable) error
	Delete(d Deletable) error
}

// Repository is the full per-entity port — read + write — every consumer
// declares in the domain layer (e.g., `type UserRepository interface {
// domain.Repository[*User]; FindByEmail(...) }`). Pure: stdlib +
// google/uuid only, no application import. A request-scoped instance whose
// write methods are bound to a ctx is produced by
// persistence.ScopedRepository[T].Scope(ctx, opts...).
type Repository[TEntity any] interface {
	Reader[TEntity]
	Writer
}

// ArchivedFinder is an optional capability: Repositories that can load
// an aggregate INCLUDING archived rows (deleted_at IS NOT NULL) implement it.
//
// Used by UnarchiveCommandHandler to hydrate the archived aggregate before
// dispatch — this ensures the cascade SQL in aggregate_persister sees
// the typeNames of the children via root.AllAggregateItems() and restores all
// child rows. Without this interface, the handler falls back to Repo.New() (empty sample)
// and cascade only works on flat aggregates (without children).
type ArchivedFinder[TEntity any] interface {
	FindArchivedByID(id ID) (TEntity, error)
}

// DefaultRepository[T] is a zero-state read-port implementation that
// returns RepositoryFunctionNotImplemented for FindByID and a zero value
// for New. Useful as a placeholder during scaffolding or as a base type
// the consumer embeds and selectively overrides. It satisfies Reader[T]
// (not the write side — a placeholder never persists).
type DefaultRepository[TEntity any] struct {
	Name string
}

func (d DefaultRepository[TEntity]) FindByID(ID) (TEntity, error) {
	var zero TEntity
	return zero, repoNotImpl(d.Name, "FindByID")
}

func (d DefaultRepository[TEntity]) New() TEntity {
	var zero TEntity
	return zero
}

func repoNotImpl(name, fn string) error {
	ctx := NewNotificationContext(name)
	ctx.AddNotificationMessage(NotificationMessage{
		FuncName:     name + "." + fn,
		Notification: RepositoryFunctionNotImplementedNotification{},
	})
	return NewDomainError([]*NotificationContext{ctx})
}
