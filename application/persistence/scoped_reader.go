package persistence

import (
	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ScopedReaderProvider is the read-side mirror of ScopedRepository.Scope. A
// repository that can bind the request ctx to its loads implements it, so the
// write-command handlers (Update/Archive/Delete) load the aggregate under the
// SAME deadline/cancellation as the write that follows — closing the gap where
// a ctx-less domain.Reader[T].FindByID runs the load on context.Background()
// and ignores http.requestTimeoutSeconds.
//
// The returned domain.Reader[T] keeps the pure ctx-less signature
// (FindByID(id) / New()); the concrete the provider returns closes over the
// ctx, exactly as Scope's boundWriter does for writes. The domain port never
// pronounces the ctx — the binding lives entirely in application + infra.
//
// Optional capability, probed (never embedded) like domain.ArchivedFinder.
// infra.BaseAggregateRepository[T] implements it; a hand-rolled repository
// (bare BaseRepository + manual loader) that does not falls back to the
// ctx-less domain.Reader[T].FindByID via LoadForWrite — it owns its ctx, in
// line with the escape-hatch posture.
type ScopedReaderProvider[T any] interface {
	ScopedReader(ctx *configuration.AppContext) domain.Reader[T]
}

// ScopedArchivedReaderProvider is the archived-scope twin of
// ScopedReaderProvider, mirroring domain.ArchivedFinder[T] under the request
// ctx. UnarchiveCommandHandler probes it to hydrate the archived aggregate
// (deleted_at IS NOT NULL) under the request deadline before falling back to
// the ctx-less domain.ArchivedFinder[T].
type ScopedArchivedReaderProvider[T any] interface {
	ScopedArchivedReader(ctx *configuration.AppContext) domain.ArchivedFinder[T]
}

// LoadForWrite loads the aggregate a write command mutates. When the repo
// provides ScopedReader, the load runs under the request ctx (cancellation +
// http.requestTimeoutSeconds reach the SELECT); otherwise it degrades to the
// ctx-less domain.Reader[T].FindByID. Used by the Update/Archive/Delete Auto
// handlers.
//
// The load stamps the old-state snapshot (domain.CaptureOld) so domain.Old[T]
// answers the PERSISTED state no matter what mutates the entity afterwards.
// The framework's relational loader already snapshots at hydration; this is the
// net for a hand-rolled repository that bypasses it, and it is a no-op when the
// snapshot is already there.
func LoadForWrite[T any](repo ScopedRepository[T], ctx *configuration.AppContext, id domain.ID) (T, error) {
	if sr, ok := any(repo).(ScopedReaderProvider[T]); ok {
		return captureLoaded(sr.ScopedReader(ctx).FindByID(id))
	}
	return captureLoaded(repo.FindByID(id))
}

// captureLoaded stamps the birth-time snapshot on a freshly loaded entity,
// passing the load's own (value, error) pair straight through. A failed load
// and a T that is not a domain.Entity are both left untouched.
func captureLoaded[T any](loaded T, err error) (T, error) {
	if err != nil {
		return loaded, err
	}
	if ent, ok := any(loaded).(domain.Entity); ok {
		domain.CaptureOld(ent)
	}
	return loaded, nil
}

// LoadArchivedForWrite hydrates an archived aggregate for the unarchive path,
// preferring the ctx-bound ScopedArchivedReader, then the ctx-less
// domain.ArchivedFinder[T]. The bool reports whether an archived-finder path
// was taken; when false the caller falls back to Repo.New() + SetID (flat
// aggregate without children), preserving the existing UnarchiveCommandHandler
// behavior.
func LoadArchivedForWrite[T any](repo ScopedRepository[T], ctx *configuration.AppContext, id domain.ID) (T, bool, error) {
	if sr, ok := any(repo).(ScopedArchivedReaderProvider[T]); ok {
		loaded, err := captureLoaded(sr.ScopedArchivedReader(ctx).FindArchivedByID(id))
		return loaded, true, err
	}
	if finder, ok := any(repo).(domain.ArchivedFinder[T]); ok {
		loaded, err := captureLoaded(finder.FindArchivedByID(id))
		return loaded, true, err
	}
	var zero T
	return zero, false, nil
}
