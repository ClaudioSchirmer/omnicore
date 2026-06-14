package infra

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

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

// RepoConfig declares convention overrides for a specific Repository.
// Common case: zero value — all names come from inference (InferTableName,
// InferColumns with snake_case of the field, InferForeignKey).
//
// Overrides cover cases where convention does not fit — legacy schema,
// non-Go naming, edge cases:
//
//	Config: fwinfra.RepoConfig{
//	    Table: "tb_users",                              // override root table
//	    FieldOverrides: map[string]string{
//	        "Email": "mail",                             // GoField → SQL column
//	    },
//	    ChildTableOverrides: map[string]string{
//	        "Address": "tb_addresses",                  // ChildTypeName → SQL table
//	    },
//	    ChildFKOverrides: map[string]string{
//	        "Address": "owner_id",                      // ChildTypeName → FK column
//	    },
//	}
//
// Phase 19: DDD-pure domain does not pronounce table/column/FK names; override
// is a per-service/per-schema decision and lives here in the infra layer.
type RepoConfig struct {
	Table               string
	FieldOverrides      map[string]string
	ChildTableOverrides map[string]string
	ChildFKOverrides    map[string]string
}

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

	// Config is optional. Zero value = pure inference by convention (common
	// case). Populate overrides for legacy schema, non-Go naming, etc.
	Config RepoConfig
}

// Insert routes through *Postgres and threads the variadic WriteOption[T]
// to the persister via AdaptWriteOptions — the typed afterBegin /
// beforeCommit closures the Cmd (Auto path) or the caller (manual path)
// supplied land at positions A and D of the TX.
func (r *BaseRepository[T]) Insert(ctx domain.Context, i domain.Insertable, opts ...persistence.WriteOption[T]) (domain.ID, error) {
	res, err := r.Postgres.Insert(ctx, i, &r.Config, AdaptWriteOptions(opts))
	if err != nil {
		return domain.ID{}, r.mapErr(err)
	}
	return domain.NewID(res.ID), nil
}

func (r *BaseRepository[T]) Update(ctx domain.Context, u domain.Updatable, opts ...persistence.WriteOption[T]) error {
	_, err := r.Postgres.Update(ctx, u, &r.Config, AdaptWriteOptions(opts))
	return r.mapErr(err)
}

func (r *BaseRepository[T]) Delete(ctx domain.Context, d domain.Deletable, opts ...persistence.WriteOption[T]) error {
	return r.mapErr(r.Postgres.Delete(ctx, d, &r.Config, AdaptWriteOptions(opts)))
}

func (r *BaseRepository[T]) Archive(ctx domain.Context, a domain.Archivable, opts ...persistence.WriteOption[T]) error {
	return r.mapErr(r.Postgres.Archive(ctx, a, &r.Config, AdaptWriteOptions(opts)))
}

func (r *BaseRepository[T]) Unarchive(ctx domain.Context, u domain.Unarchivable, opts ...persistence.WriteOption[T]) error {
	return r.mapErr(r.Postgres.Unarchive(ctx, u, &r.Config, AdaptWriteOptions(opts)))
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
