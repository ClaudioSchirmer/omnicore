package domain

// Repository is the per-entity read port at the domain layer. The
// write surface lives at the application layer as
// persistence.Writer[T] — the write methods carry a typed variadic of
// hook options (afterBegin / beforeCommit lifecycle), and those option
// types reference *configuration.AppContext from the application
// layer; pulling that import into the domain would break the
// dependency rule (domain → stdlib + google/uuid only).
//
// Keeping the read methods (FindByID, New) here preserves the DDD
// repository surface every consumer constructs in the domain layer
// (e.g., `type UserRepository struct { ... }` returning *User on
// FindByID). The Auto Command Handlers consume persistence.Writer[T]
// instead of domain.Repository[T] for the write-side variadic; a
// concrete BaseRepository[T] in infra/ implements both surfaces, so
// the consumer service constructs one struct and threads it wherever
// the matching port is expected.
//
// T is the "usable" type of the entity (typically a pointer like
// *User) — FindByID returns T directly because T already carries the
// indirection when applicable. This fits the domain.Entity constraint
// of the Auto Command Handlers, which requires a type with
// pointer-receiver methods (e.g. *User).
type Repository[TEntity any] interface {
	FindByID(id ID) (TEntity, error)
	New() TEntity
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
// returns RepositoryFunctionNotImplemented for every method. Useful as
// a placeholder during scaffolding or as a base type the consumer
// embeds and selectively overrides.
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
