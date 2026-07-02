package pipeline

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// InsertCommand is the contract consumed by handlers.InsertCommandHandler[T, Cmd, TResult].
// The Cmd hydrates a new entity via ToEntity and projects the post-insert
// entity into the response shape via FromEntity. T is the "usable" type of the
// entity (typically a pointer like *User, to satisfy domain.Entity); TResult
// is the application-layer projection the wire layer will render.
//
// Both ToEntity and FromEntity receive the request *AppContext — symmetric
// boundary at input and output. The Command is the only layer below the web
// boundary allowed to consume ctx, and the symmetry lets a service translate
// identity-derived claims on the way in (owner_user_id, tenant_id) AND on
// the way out (decorate the result with claim-derived fields, e.g. show
// only fields the principal is allowed to see). Domain entity sees only
// business-named fields.
type InsertCommand[T domain.Entity, TResult any] interface {
	Command
	ToEntity(ctx *configuration.AppContext) (T, error)
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// SharedBaseInsertCommand is the contract consumed by
// handlers.SharedBaseInsertCommandHandler[T, Cmd, TResult] — the POST of an
// entity backed by a SharedBase (Party-Role), which is an UPSERT: the framework
// loads the existing shared identity first, the command applies the request on
// top (like an update), and persists. So — unlike a plain Insert — the command
// declares ApplyTo (mutate the loaded-or-fresh entity), NOT ToEntity. The dev
// owns dedup of the shared identity's native children inside ApplyTo (the
// existing ones arrive as Constructor items); the infra only loads and persists.
// The framework calls ApplyTo on a throwaway entity to read the natural key, so
// ApplyTo MUST be a pure mapper (no side effects). Same ctx input/output symmetry
// as the other auto commands.
type SharedBaseInsertCommand[T domain.Entity, TResult any] interface {
	Command
	ApplyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// UpdateCommand is the contract consumed by handlers.UpdateCommandHandler[T, Cmd, TResult].
// PUT-shaped: full replace. ApplyTo overwrites ALL editable fields of the
// loaded entity. Cmd typically declares fields as direct types (not
// pointers) — the handler is coupled to the Fiber wrapper via the FullBody
// marker, which enforces presence of all fields in the body JSON before
// dispatch.
//
// ApplyTo and FromEntity both receive the request *AppContext — symmetric
// input/output boundary. ApplyTo translates ctx into business-named transient
// fields (e.g., u.SetRequestingOwnerID(ctx.Identity().Subject)) that
// BuildRules' IfUpdate validates against the persistent owner field;
// FromEntity translates the post-update entity back into the wire-shaped
// Result, with full access to the same ctx + the cmd itself (via receiver).
// Use cases for ctx + cmd in FromEntity: subresource endpoints (return only
// the child the cmd targeted), claim-filtered projections, identity-aware
// shaping. Domain sees only business fields, never ctx.
type UpdateCommand[T domain.Entity, TResult any] interface {
	CommandWithID
	ApplyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// PartialUpdateCommand is the contract consumed by
// handlers.PartialUpdateCommandHandler[T, Cmd, TResult]. PATCH-shaped: partial
// update. Cmd typically declares fields as pointers (tri-state nil/non-nil)
// and ApplyPartiallyTo applies only the non-nil fields onto the loaded entity.
//
// Same input/output ctx symmetry as UpdateCommand — ApplyPartiallyTo translates
// ctx into business-named transients; FromEntity projects the post-patch
// entity back into the Result with full ctx access.
type PartialUpdateCommand[T domain.Entity, TResult any] interface {
	CommandWithID
	ApplyPartiallyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// ArchiveCommand is the contract consumed by handlers.ArchiveCommandHandler[T, Cmd, TResult].
// Bodyless verb — no field mutation; Archive carries only the ID via the
// CommandWithID embed. ApplyTo is invoked AFTER FindByID loads the aggregate
// and BEFORE GetArchivable runs the (now-extended) BuildRules → state
// transition pipeline. The Command's job inside ApplyTo is to populate
// business-named transient fields the domain's IfUpdate may consult for
// authorization or other identity-derived checks. The entity itself is the
// loaded aggregate, ready for the cascade.
//
// FromEntity is symmetric — runs after the state transition, projects the
// post-archive entity into TResult. Bodyless verbs typically declare
// TResult = results.None and have FromEntity return results.None{}; richer
// responses are equally fine.
//
// Why ApplyTo on a bodyless verb? Uniformity across all 6 Auto handlers, and
// a place for ctx → business translation symmetric with the other verbs. An
// ArchiveCommand that doesn't need ctx just ignores both parameters.
type ArchiveCommand[T domain.Entity, TResult any] interface {
	CommandWithID
	ApplyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// UnarchiveCommand is the symmetric inverse of ArchiveCommand — same shape,
// fires on PATCH /:id/unarchive. ApplyTo lands AFTER the Repository's
// ArchivedFinder hydrates the archived aggregate (so the cascade SQL sees the
// children typeNames) and BEFORE GetUnarchivable runs BuildRules + state
// transition. FromEntity projects the post-unarchive entity into TResult.
type UnarchiveCommand[T domain.Entity, TResult any] interface {
	CommandWithID
	ApplyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}

// DeleteCommand is consumed by handlers.DeleteCommandHandler[T, Cmd, TResult].
// Same shape as Archive/Unarchive. ApplyTo runs AFTER FindByID and BEFORE
// GetDeletable runs BuildRules in ModeDelete (where IfDelete-scoped rules
// fire). Use ApplyTo to populate identity-derived transients the domain
// IfDelete may need. FromEntity projects the entity-before-delete state into
// TResult — useful when the wire response wants to echo the deleted shape.
type DeleteCommand[T domain.Entity, TResult any] interface {
	CommandWithID
	ApplyTo(ctx *configuration.AppContext, entity T) error
	FromEntity(ctx *configuration.AppContext, entity T) (TResult, error)
}
