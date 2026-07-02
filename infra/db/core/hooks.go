package core

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// WriteHook is the type-erased shape each engine's flat + aggregate paths fire at
// positions A and D. The typed persistence.AfterBeginHook[T] / BeforeCommitHook[T]
// declared by consumers reach this surface through BaseRepository[T]'s adapter,
// which casts the source entity to T and the request context to *AppContext
// exactly once per call.
//
// A nil WriteHook function means "no hook configured" — the persister skips the
// firing branch. The id parameter on the beforeCommit slot is the post-write id
// the persister already holds at position D, spared to the consumer.
type WriteHook struct {
	AfterBegin   func(ctx persistence.RequestContext, source domain.Entity, tx persistence.TxHandle) error
	BeforeCommit func(ctx persistence.RequestContext, source domain.Entity, id domain.ID, tx persistence.TxHandle) error
}

// AdaptWriteOptions translates the typed persistence.WriteOption[T] variadic into
// the type-erased WriteHook the engine methods consume, so BaseRepository[T] (and
// any custom Repository) builds the dispatch struct in one call.
//
// The closure layer pays a single per-call type assertion against T and against
// *configuration.AppContext — both deterministic at runtime (T is the
// Repository's entity type; the request ctx IS the AppContext the middleware
// populated). A mismatch is a wiring bug → typed panic → pipeline.Run recover →
// Result.Exception (500), with the persister's defer rollback intact.
//
// Returns the zero WriteHook (both fields nil) when opts is empty — the
// zero-cost fast path for an Auto handler whose Cmd implements neither provider.
func AdaptWriteOptions[T any](opts []persistence.WriteOption[T]) WriteHook {
	afterBegin, beforeCommit := persistence.ResolveWriteOptions(opts)
	hook := WriteHook{}
	if afterBegin != nil {
		hook.AfterBegin = func(ctx persistence.RequestContext, source domain.Entity, tx persistence.TxHandle) error {
			return afterBegin(assertAppContext(ctx), assertEntity[T](source), tx)
		}
	}
	if beforeCommit != nil {
		hook.BeforeCommit = func(ctx persistence.RequestContext, source domain.Entity, id domain.ID, tx persistence.TxHandle) error {
			return beforeCommit(assertAppContext(ctx), assertEntity[T](source), id, tx)
		}
	}
	return hook
}

// assertAppContext casts persistence.RequestContext to *configuration.AppContext.
// The framework's middleware always installs one; a mismatch is a wiring bug
// (test fixture / custom dispatch skipped the middleware) → panic → rollback +
// Result.Exception via pipeline.Run's recover.
func assertAppContext(ctx persistence.RequestContext) *configuration.AppContext {
	appCtx, ok := ctx.(*configuration.AppContext)
	if !ok {
		panic("persistence: hook fired without *configuration.AppContext on the request — wiring bug, AppContextMiddleware skipped?")
	}
	return appCtx
}

// assertEntity casts a domain.Entity (carried by every ValidEntity's Source()) to
// the typed T the Repository is parameterized on. The cast succeeds at runtime
// because the Repository's NewEntity factory + the handler's ToEntity / FindByID
// paths all produce T; a mismatch is a Repository<T> wired with the wrong T.
func assertEntity[T any](source domain.Entity) T {
	typed, ok := source.(T)
	if !ok {
		panic("persistence: hook fired on entity that does not match Repository's T — wiring bug, wrong type parameter?")
	}
	return typed
}
