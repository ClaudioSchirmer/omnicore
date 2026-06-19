package infra

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ConstraintBinding maps the name of a Postgres constraint (typically a
// unique index) to the typed notification that must be emitted when that
// constraint is violated (PG SQLSTATE 23505).
//
// Field is the domain field name (e.g. "email") — passed to
// NotificationMessage.FieldName.
type ConstraintBinding struct {
	Notification domain.Notification
	Field        string
}

// TableSchema — the per-Repository Go-field↔column map — is declared in
// table_schema.go and threaded via BaseAggregateRepository.WithSchema.

// BaseRepository[T] implements the 5 write methods of domain.Repository[T]
// (Insert/Update/Archive/Unarchive/Delete) as one-liners that delegate to
// *Postgres + provides New() T via the injected NewEntity factory. The
// aggregate-aware dispatch happens transparently because the Postgres methods
// check AggregateInfo() internally.
//
// FindByID is NOT implemented here — it depends on scanning entity T. The
// service embeds BaseRepository and implements FindByID separately (or uses
// AggregateLoader).
//
// NewEntity is the factory that returns an empty instance of T. Required —
// New() panics if nil (config-time bug: the service forgot to inject). The
// same factory is typically shared with AggregateLoader to avoid duplication
// in the service.
//
// ContextName is the context name for notifications emitted by the internal
// mapErr (constraint violation). **Optional** — when empty, derived from the
// Go type T via TypeName[T]() (e.g. T=*User → "User"). Set explicitly only
// when convention does not fit (custom magic pattern: e.g. "AdminUser" for
// two Repositories over the same entity).
//
// Constraints is optional. Without it (nil/empty), any pgErr 23505 returns
// the raw error and the caller decides what to do.
type BaseRepository[T any] struct {
	Postgres    *Postgres
	ContextName string
	Constraints map[string]ConstraintBinding
	NewEntity   func() T

	// Schema is the mandatory explicit Go↔column map for this repository's
	// entity (root + aggregate children). Set it directly or via
	// BaseAggregateRepository.WithSchema. There is no convention fallback — a
	// write with a nil Schema panics.
	Schema *TableSchema
}

// Scope binds the request-scoped concerns — the ctx (cancellation → pgx,
// actor → audit) and the optional in-TX lifecycle hooks carried by the
// WriteOption[T] variadic — and returns a pure domain.Writer ready to
// persist a ValidEntity. The returned boundWriter closes over the ctx and
// the resolved hook; its Insert/Update/Archive/Unarchive/Delete take only
// a ValidEntity, so the domain port never pronounces the ctx.
//
// AdaptWriteOptions folds the typed afterBegin / beforeCommit closures the
// Cmd (Auto path) or the caller (manual path) supplied into the type-erased
// writeHook the persister fires at positions A and D of the TX.
func (r *BaseRepository[T]) Scope(ctx *configuration.AppContext, opts ...persistence.WriteOption[T]) domain.Writer {
	return boundWriter[T]{repo: r, ctx: ctx, hook: AdaptWriteOptions(opts)}
}

// boundWriter is the request-scoped domain.Writer Scope returns. It holds
// the BaseRepository (for the Postgres adapter, the TableSchema, and the
// constraint-violation mapErr), the bound ctx, and the resolved hook. Each
// write delegates to *Postgres with the captured ctx — the pure domain
// signature (ValidEntity in, outcome out) is preserved.
type boundWriter[T any] struct {
	repo *BaseRepository[T]
	ctx  *configuration.AppContext
	hook writeHook
}

func (w boundWriter[T]) Insert(i domain.Insertable) (domain.ID, error) {
	res, err := w.repo.Postgres.Insert(w.ctx, i, w.repo.Schema, w.hook)
	if err != nil {
		return domain.ID{}, w.repo.mapErr(err)
	}
	return domain.NewID(res.ID), nil
}

func (w boundWriter[T]) Update(u domain.Updatable) error {
	_, err := w.repo.Postgres.Update(w.ctx, u, w.repo.Schema, w.hook)
	return w.repo.mapErr(err)
}

func (w boundWriter[T]) Delete(d domain.Deletable) error {
	return w.repo.mapErr(w.repo.Postgres.Delete(w.ctx, d, w.repo.Schema, w.hook))
}

func (w boundWriter[T]) Archive(a domain.Archivable) error {
	return w.repo.mapErr(w.repo.Postgres.Archive(w.ctx, a, w.repo.Schema, w.hook))
}

func (w boundWriter[T]) Unarchive(u domain.Unarchivable) error {
	return w.repo.mapErr(w.repo.Postgres.Unarchive(w.ctx, u, w.repo.Schema, w.hook))
}

// New returns an empty instance of T via the injected factory. Panics if
// NewEntity is nil — a configuration bug that deserves to be caught at
// service boot (first call to New), not on the first unarchive request.
func (r *BaseRepository[T]) New() T {
	if r.NewEntity == nil {
		panic("BaseRepository: NewEntity factory not configured — set NewEntity: func() T { return &MyEntity{} }")
	}
	return r.NewEntity()
}

// effectiveContextName returns ContextName if set explicitly; otherwise
// derives it from type T via TypeName[T](). Override always wins — convention
// is the default, custom is the caller's decision.
func (r *BaseRepository[T]) effectiveContextName() string {
	if r.ContextName != "" {
		return r.ContextName
	}
	return TypeName[T]()
}

// mapErr translates pgErr 23505 (unique violation) into a typed
// *InfrastructureError, using the Constraints map. Non-pgErr errors, unmapped
// violations and codes other than 23505 are returned raw.
func (r *BaseRepository[T]) mapErr(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return err
	}
	binding, ok := r.Constraints[pgErr.ConstraintName]
	if !ok {
		return err
	}
	return FieldErrorWithCause(r.effectiveContextName(), binding.Field, pgErr, binding.Notification)
}
