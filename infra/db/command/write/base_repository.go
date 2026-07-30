package write

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ConstraintBinding maps the name of a unique constraint (typically a unique
// index) to the typed notification that must be emitted when that constraint
// is violated. The engine's Dialect classifies the backend-specific error
// uniformly (PG SQLSTATE 23505 / MySQL errno 1062) — see IsUniqueViolation.
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
// (Insert/Update/Archive/Unarchive/Delete) as one-liners that delegate to the
// RelationalEngine + provides New() T via the injected NewEntity factory. The
// aggregate-aware dispatch happens transparently because the engine methods
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
// Constraints is optional. Without it (nil/empty), any unique-constraint
// violation (classified by the engine's Dialect) returns the raw error and the
// caller decides what to do.
type BaseRepository[T any] struct {
	Engine      RelationalEngine
	ContextName string
	Constraints map[string]ConstraintBinding
	NewEntity   func() T

	// schema is the mandatory explicit Go↔column map for this repository's
	// entity (root + aggregate children). It is UNEXPORTED and bound only
	// through WithSchema — there is no way to set it that bypasses the
	// construction-time boot checks (ID, Revision, aggregate depth, old-clone
	// safety, Modes() ⟺ DeletedAt). There is no convention fallback — a write
	// with a nil schema panics.
	schema *TableSchema
}

// Scope binds the request-scoped concerns — the ctx (cancellation via
// context.Context, actor → audit) and the optional in-TX lifecycle hooks carried by the
// WriteOption[T] variadic — and returns a pure domain.Writer ready to
// persist a ValidEntity. The returned boundWriter closes over the ctx and
// the resolved hook; its Insert/Update/Archive/Unarchive/Delete take only
// a ValidEntity, so the domain port never pronounces the ctx.
//
// AdaptWriteOptions folds the typed afterBegin / beforeCommit closures the
// Cmd (Auto path) or the caller (manual path) supplied into the type-erased
// WriteHook the persister fires at positions A and D of the TX.
func (r *BaseRepository[T]) Scope(ctx *configuration.AppContext, opts ...persistence.WriteOption[T]) domain.Writer {
	return boundWriter[T]{repo: r, ctx: ctx, hook: AdaptWriteOptions(opts)}
}

// boundWriter is the request-scoped domain.Writer Scope returns. It holds
// the BaseRepository (for the RelationalEngine, the TableSchema, and the
// constraint-violation mapErr), the bound ctx, and the resolved hook. Each
// write delegates to the RelationalEngine with the captured ctx — the pure
// domain signature (ValidEntity in, outcome out) is preserved.
type boundWriter[T any] struct {
	repo *BaseRepository[T]
	ctx  *configuration.AppContext
	hook WriteHook
}

func (w boundWriter[T]) Insert(i domain.Insertable) (domain.ID, error) {
	res, err := w.repo.Engine.Insert(w.ctx, i, w.repo.schema, w.hook)
	if err != nil {
		return domain.ID{}, w.repo.mapErr(err)
	}
	return res.ID, nil
}

func (w boundWriter[T]) Update(u domain.Updatable) error {
	_, err := w.repo.Engine.Update(w.ctx, u, w.repo.schema, w.hook)
	return w.repo.mapErr(err)
}

func (w boundWriter[T]) Delete(d domain.Deletable) error {
	return w.repo.mapErr(w.repo.Engine.Delete(w.ctx, d, w.repo.schema, w.hook))
}

func (w boundWriter[T]) Archive(a domain.Archivable) error {
	return w.repo.mapErr(w.repo.Engine.Archive(w.ctx, a, w.repo.schema, w.hook))
}

func (w boundWriter[T]) Unarchive(u domain.Unarchivable) error {
	return w.repo.mapErr(w.repo.Engine.Unarchive(w.ctx, u, w.repo.schema, w.hook))
}

// WithSchema declares the mandatory TableSchema and runs the construction-time
// boot checks before binding it: type-anchored (not NewExternalSchema),
// ID-declared, aggregate-depth (no grandchildren), old-clone safety (no
// `json:"-"` on a persisted field, no custom json.(Un)Marshaler on the entity —
// either would corrupt the domain.Old() snapshot), and — when T exposes Modes()
// — the Modes() ⟺ DeletedAt invariant. The field-existence + bijection checks
// already ran while the TableSchema was built. A violation panics at
// construction, not on the first request, so a flat (non-aggregate) repository
// gets the same fail-fast the aggregate path has via
// BaseAggregateRepository.WithSchema.
//
// WithSchema is the ONLY way to bind the schema — the field is unexported, so
// there is no direct-set path that bypasses these checks. Calling it also
// surfaces a nil NewEntity factory at construction (via r.New()) instead of on
// the first write.
func (r *BaseRepository[T]) WithSchema(schema *TableSchema) *BaseRepository[T] {
	schema.ValidateAnchored()
	if schema.IsSecondary() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): a sibling (NewSiblingSchema) cannot be a repository root — "+
				"it is a shared-ID secondary table; attach it to a root via root.Sibling(...)",
			schema.Table(),
		))
	}
	if !schema.HasPKDeclared() {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): no primary key declared — declare .ID(column); "+
				"there is no default, the developer must declare it",
			schema.Table(),
		))
	}
	if schema.RevisionColumn() == "" {
		panic(fmt.Sprintf(
			"infra.TableSchema(%s): no Revision declared — declare .Revision(column) "+
				"(revision BIGINT NOT NULL DEFAULT 0 in the migration): it is the commit-order token "+
				"that lets the read side refuse stale document writes (zombie consumers, redelivery)",
			schema.Table(),
		))
	}
	schema.ValidateChildDepth()
	schema.ValidateSiblings()
	schema.ValidateSharedBaseChildren()
	schema.ValidateOldCloneSafety()
	if m, ok := any(r.New()).(interface{ Modes() []domain.EntityMode }); ok {
		schema.ValidateModes(m.Modes())
	}
	// SharedBase role: register on the engine-scoped registry, so the
	// refcount/lifecycle probes see every role of the base's TABLE regardless of
	// how many NewSharedBaseSchema instances declared it — the consumer never needs a
	// singleton. A divergent re-declaration of the same base panics here (boot).
	if reg, ok := any(r.Engine).(interface{ RegisterSharedBaseRole(*TableSchema) }); ok {
		reg.RegisterSharedBaseRole(schema)
	}
	r.schema = schema
	return r
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

// mapErr translates a unique-constraint violation into a typed
// *InfrastructureError, using the Constraints map. The classification is
// dialect-aware (the engine's Dialect reads PG 23505 / MySQL 1062 and the
// violated constraint name); a non-violation, an unmapped constraint, or a nil
// engine returns the error raw.
func (r *BaseRepository[T]) mapErr(err error) error {
	if err == nil {
		return nil
	}
	if r.Engine == nil {
		return err
	}
	constraint, ok := r.Engine.Dialect().IsUniqueViolation(err)
	if !ok {
		return err
	}
	binding, ok := r.Constraints[constraint]
	if !ok {
		return err
	}
	return FieldErrorWithCause(r.effectiveContextName(), binding.Field, err, binding.Notification)
}
