package infra

import (
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// writeHook is the type-erased shape the Postgres flat + aggregate paths
// fire at positions A and D. The typed persistence.AfterBeginHook[T] /
// BeforeCommitHook[T] declared by consumers reach this surface through
// BaseRepository[T]'s adapter, which casts the source entity to T and
// the request context to *AppContext exactly once per call.
//
// A nil writeHook function means "no hook configured" — the persister
// skips the firing branch without paying for the per-attempt slog line
// reserved for hook errors.
//
// We keep both slot signatures intentionally similar so the firing
// helpers below can stay symmetric. The id parameter exists on the
// beforeCommit slot because the persister already has the post-write id
// when it reaches position D; surfacing it on the signature spares the
// consumer one t.GetID() call inside their hook.
type writeHook struct {
	AfterBegin   func(ctx domain.Context, source domain.Entity, tx persistence.TxHandle) error
	BeforeCommit func(ctx domain.Context, source domain.Entity, id domain.ID, tx persistence.TxHandle) error
}

// hookContext describes the slot the persister is about to fire. Used
// only for the observability slog.Warn emitted on hook error — Topic 7
// fields: verb, hookSlot, entityType, threadId, error.
type hookContext struct {
	verb       string
	entityType string
}

// fireAfterBegin runs the AfterBeginHook (when configured) inside the
// open TX, right after BEGIN, BEFORE any framework write. Returns the
// hook's error verbatim so the caller can ROLLBACK + propagate the
// NotificationCarrier identity end-to-end. The slog.Warn emission is
// best-effort and never blocks the rollback path.
func (p *Postgres) fireAfterBegin(
	ctx domain.Context,
	tx pgx.Tx,
	source domain.Entity,
	hook writeHook,
	hctx hookContext,
) error {
	if hook.AfterBegin == nil {
		return nil
	}
	if err := hook.AfterBegin(ctx, source, newPgxTxHandle(tx)); err != nil {
		p.logHookError(ctx, hctx, "afterBegin", err)
		return err
	}
	return nil
}

// fireBeforeCommit runs the BeforeCommitHook (when configured) inside
// the open TX, AFTER all framework writes (data + outbox + audit) and
// BEFORE COMMIT. Mirrors fireAfterBegin's error + observability shape.
func (p *Postgres) fireBeforeCommit(
	ctx domain.Context,
	tx pgx.Tx,
	source domain.Entity,
	id domain.ID,
	hook writeHook,
	hctx hookContext,
) error {
	if hook.BeforeCommit == nil {
		return nil
	}
	if err := hook.BeforeCommit(ctx, source, id, newPgxTxHandle(tx)); err != nil {
		p.logHookError(ctx, hctx, "beforeCommit", err)
		return err
	}
	return nil
}

// logHookError emits the Topic 7 observability line. Best-effort: a nil
// logger falls back to slog.Default so the line still lands somewhere
// even when the framework boot path skipped WithAudit's logger arg.
func (p *Postgres) logHookError(ctx domain.Context, hctx hookContext, slot string, err error) {
	logger := p.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("persistence.hook.error",
		"verb", hctx.verb,
		"hookSlot", slot,
		"entityType", hctx.entityType,
		"threadId", ctx.ID().String(),
		"error", err.Error(),
	)
}

// AdaptWriteOptions translates the typed persistence.WriteOption[T]
// variadic into the type-erased writeHook the Postgres methods consume.
// It is exposed on the infra package so BaseRepository[T] (and any
// custom Repository implementation that talks straight to Postgres) can
// build the dispatch struct in one call.
//
// The closure layer pays a single per-call type assertion against T and
// against *configuration.AppContext. Both assertions are deterministic
// at this layer: T is the entity type the Repository was constructed
// for; the request ctx that reaches the Repository at runtime IS the
// AppContext the bootstrap middleware populated. A mismatch on either
// is a configuration / wiring bug — the framework returns a typed error
// so the lifecycle propagates ROLLBACK cleanly instead of panicking.
//
// Returns the zero writeHook (both fields nil) when opts is empty,
// which the firing helpers treat as "no hook configured" — zero-cost
// fast path for the common Auto handler case where the Cmd implements
// neither provider.
func AdaptWriteOptions[T any](opts []persistence.WriteOption[T]) writeHook {
	afterBegin, beforeCommit := persistence.ResolveWriteOptions(opts)
	hook := writeHook{}
	if afterBegin != nil {
		hook.AfterBegin = func(ctx domain.Context, source domain.Entity, tx persistence.TxHandle) error {
			return afterBegin(assertAppContext(ctx), assertEntity[T](source), tx)
		}
	}
	if beforeCommit != nil {
		hook.BeforeCommit = func(ctx domain.Context, source domain.Entity, id domain.ID, tx persistence.TxHandle) error {
			return beforeCommit(assertAppContext(ctx), assertEntity[T](source), id, tx)
		}
	}
	return hook
}

// assertAppContext casts domain.Context to *configuration.AppContext.
// The framework's middleware always installs an AppContext on the
// request, so the cast succeeds in production code paths. A mismatch
// indicates a wiring bug (test fixture forgot to use AppContext, custom
// dispatch path skipped the middleware) — panicking is the framework
// convention for "the caller asked for behavior that the contract does
// not permit": defer tx.Rollback() in the persister still fires, the
// panic propagates to pipeline.Run's recover → Result.Exception (500).
func assertAppContext(ctx domain.Context) *configuration.AppContext {
	appCtx, ok := ctx.(*configuration.AppContext)
	if !ok {
		panic("persistence: hook fired without *configuration.AppContext on the request — wiring bug, AppContextMiddleware skipped?")
	}
	return appCtx
}

// assertEntity casts a domain.Entity (carried by every ValidEntity's
// Source()) to the typed T the Repository is parameterized on. Same
// guarantee as assertAppContext: the cast succeeds at runtime because
// the Repository's NewEntity factory + the handler's ToEntity /
// FindByID paths all produce T. A mismatch indicates a Repository<T>
// wired with the wrong T — caught at first request, panic propagated
// to Result.Exception.
func assertEntity[T any](source domain.Entity) T {
	typed, ok := source.(T)
	if !ok {
		panic("persistence: hook fired on entity that does not match Repository's T — wiring bug, wrong type parameter?")
	}
	return typed
}
