package query

import (
	"context"
	"log/slog"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// viewEmbedSignal is the fan-out a WRITE to a view document triggers: "this
// view is materialized inside other views (query.JoinView) — refresh them".
// It is the local-source twin of the UpstreamSubscriber's mirror ripple, and
// the reason a view may embed another view at all: a materialized segment stays
// fresh because every writer of the source signals here.
//
// One instance is shared process-wide (built at boot from the registered view
// set) and consulted by EVERY writer of a view document:
//
//   - the SyncEngine, after each projection write / delete (the day-to-day path);
//   - an embedRippler, after each dependent write (the chained hop — a refreshed
//     view is itself a source for whoever embeds IT).
//
// A view nobody embeds resolves to nil on the lookup and the whole path costs
// one map read — the fast exit for the overwhelmingly common case.
type viewEmbedSignal struct {
	mongo    ReadModelStore
	resolver *ViewResolver
	// targets maps a SOURCE view name to its ripple target. Built once at boot
	// and never mutated, so concurrent reads from every worker are safe.
	targets map[string]*viewRippleTarget
}

// viewRippleTarget is one source view's fan-out: the rippler that refreshes its
// dependents, plus whether any of them embeds it 1:N (which is the only case
// needing the pre-write document — the old parent of a moved child).
type viewRippleTarget struct {
	rippler *embedRippler
	hasMany bool
}

// newViewEmbedSignal builds the fan-out for every view that is materialized
// into another. Returns nil when no view embeds a view — the framework's
// default shape, where every hook below is a nil-receiver no-op.
func newViewEmbedSignal(
	eng core.RelationalEngine,
	mongo ReadModelStore,
	composer *Composer,
	resolver *ViewResolver,
	views []*ViewDefinition,
	group string,
	logger *slog.Logger,
	metrics *upstreamMetrics,
) *viewEmbedSignal {
	if logger == nil {
		logger = slog.Default()
	}
	targets := map[string]*viewRippleTarget{}
	for _, src := range views {
		dependents := DependentViewViews(views, src.Name())
		if len(dependents) == 0 {
			continue
		}
		hasMany := false
		for _, d := range dependents {
			for _, e := range d.Embeds() {
				if e.Many() && e.leg != nil && e.leg.view != nil && e.leg.view.Name() == src.Name() {
					hasMany = true
					break
				}
			}
		}
		targets[src.Name()] = &viewRippleTarget{
			rippler: &embedRippler{
				eng:      eng,
				mongo:    mongo,
				composer: composer,
				resolver: resolver,
				// The failure-ledger coordinate of a view source. The ledger
				// column is the subscription topic; a view has none, so it is
				// labelled by its own identity — queryable exactly like a topic.
				topic:          viewFailureLabel(src.Name()),
				group:          group,
				collection:     src.Name(),
				sourceIsView:   true,
				dependentViews: dependents,
				logger:         logger,
				metrics:        metrics,
			},
			hasMany: hasMany,
		}
	}
	if len(targets) == 0 {
		return nil
	}
	sig := &viewEmbedSignal{mongo: mongo, resolver: resolver, targets: targets}
	// Chaining: a dependent this rippler refreshes may itself be a source, so
	// every rippler reports its writes back into the same fan-out. The embed
	// graph is acyclic (appendEmbedCycles), so the recursion terminates.
	for _, t := range targets {
		t.rippler.onViewWritten = sig.Written
	}
	return sig
}

// viewFailureLabel is the unified-ledger source coordinate of a view
// source — namespaced so it can never collide with a subscription topic.
func viewFailureLabel(viewName string) string { return "view:" + viewName }

// Before captures the pre-write state of a source document, needed ONLY when
// some dependent embeds this view 1:N (the old parent of a moved child is
// unreachable once the ParentID changed). Nil — and no Mongo read — otherwise, so a
// 1:1-only or unembedded view pays nothing. Call BEFORE the write. A read
// error degrades to nil (the post-write signal is the recorded one).
func (g *viewEmbedSignal) Before(ctx context.Context, viewName, id string) Document {
	if g == nil {
		return nil
	}
	t := g.targets[viewName]
	if t == nil || !t.hasMany {
		return nil
	}
	doc, _ := g.read(ctx, viewName, id)
	return doc
}

// Written fans out after a successful write to a source view's document. It
// re-reads the document (the write may have been a partial projection pipeline,
// so the stored document — not the stages — is what the embedders must carry)
// and ripples it into every view materializing this one.
//
// A document that reads back as absent produces NO signal: either it never
// existed (a guarded no-op write) or it vanished concurrently, and in both cases
// its own DELETED event owns the removal. Signalling a nil here would clear
// live segments on the strength of a lost race.
func (g *viewEmbedSignal) Written(ctx context.Context, viewName, id string) {
	g.WrittenWithBefore(ctx, viewName, id, nil)
}

// WrittenWithBefore is Written carrying the pre-write document captured by
// Before (the 1:N old-parent case).
func (g *viewEmbedSignal) WrittenWithBefore(ctx context.Context, viewName, id string, before Document) {
	if g == nil {
		return
	}
	t := g.targets[viewName]
	if t == nil {
		return
	}
	after, err := g.read(ctx, viewName, id)
	if err != nil {
		// The ripple never even started — without a ledger row this loss was
		// invisible: no error reaches the event (its own write succeeded), no
		// pair row exists (discovery needs the document), and the parity sweep
		// does not see segments. One kind=ripple/stage=signal row per dependent
		// view makes the retry driver re-run this exact fan-out later.
		for _, d := range t.rippler.dependentViews {
			t.rippler.recordFailure(ctx, d.Name(), id, "", ProjectionFailureStageSignal, err)
		}
		return
	}
	if after == nil {
		return
	}
	t.rippler.ripple(ctx, id, before, after, watermarkOf(after[docRevisionField]))
}

// replay re-runs the fan-out for one source document from CURRENT state — the
// parked-retry loop's entry point for view-sourced ripple rows. An absent
// document replays as a removal (clear 1:1 segments, drop 1:N elements): the
// source's own delete owns the disappearance, and the pending row exists
// precisely because some segment never heard about it.
func (g *viewEmbedSignal) replay(ctx context.Context, viewName, id string) {
	if g == nil {
		return
	}
	t := g.targets[viewName]
	if t == nil {
		return
	}
	after, err := g.read(ctx, viewName, id)
	if err != nil {
		for _, d := range t.rippler.dependentViews {
			t.rippler.recordFailure(ctx, d.Name(), id, "", ProjectionFailureStageSignal, err)
		}
		return
	}
	var rev int64
	if after != nil {
		rev = watermarkOf(after[docRevisionField])
	}
	t.rippler.ripple(ctx, id, nil, after, rev)
}

// Deleted fans out a source document's removal: the embedders clear their 1:1
// segments to the explicit null and drop the element from their 1:N arrays.
// rev is the deleted row's last revision — the watermark that keeps a late
// older write from resurrecting the segment (0 when unknown, which disables the
// guard exactly as an unwatermarked source does).
func (g *viewEmbedSignal) Deleted(ctx context.Context, viewName, id string, before Document, rev int64) {
	if g == nil {
		return
	}
	t := g.targets[viewName]
	if t == nil {
		return
	}
	t.rippler.ripple(ctx, id, before, nil, rev)
}

// Tracks reports whether any view materializes viewName — the cheap gate a
// caller uses before paying for a pre-write read.
func (g *viewEmbedSignal) Tracks(viewName string) bool {
	if g == nil {
		return false
	}
	return g.targets[viewName] != nil
}

// read returns the source view's current document by id. Absence (nil, nil) is
// a legitimate state — the source's own delete owns it; an ERROR is not: the
// callers record it as a stage=signal ledger row, because a silently skipped
// signal is a stale segment with no trail.
func (g *viewEmbedSignal) read(ctx context.Context, viewName, id string) (Document, error) {
	docs, err := g.mongo.FindManyByField(ctx, g.resolver.Active(viewName), "_id", id)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return docs[0], nil
}
