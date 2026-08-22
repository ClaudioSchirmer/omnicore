package query

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// Continuous reconciliation of derived state, by REVISION PARITY.
//
// Projections are derived state and delivery guarantees fail eventually
// regardless of how carefully they are built, so a mature CQRS system runs a
// backstop that compares the projection to its source and does not depend on the
// mechanism it is checking (I5). This is that backstop.
//
// # Why revision parity rather than a content hash
//
// The instinct is to hash each composed document and compare hashes. That is the
// wrong primitive here, because a cheaper and exact one already exists.
//
// `revision` is a BOOT-MANDATORY column on every entity schema, it advances on
// every projection-relevant write — including archive AND unarchive, and
// including a child-only change (the root UPDATE is unconditional) — and for a
// shared identity every role verb advances the base revision. The projection
// already stamps it on the document as DocRevisionField. So the source's
// (pk, revision) and the document's (_id, _revision) are directly comparable,
// per root, with NO new column, NO write-path cost, and NO composition.
//
// That comparison detects exactly the invariant violations in the fault model:
// a MISSING document (I1), a STALE one (I1), and one silently rolled back to an
// older revision by a failover (I2). What it does not detect is a document at the
// correct revision whose content is wrong — a composer bug, which the fault model
// explicitly excludes, or rolling-deploy field blindness, which the
// equal-revision add-only fill already closes.
//
// # Why there is no time window
//
// A watermark based on `updated_at` would need a lag constant, a bounded
// transaction duration and a bounded clock skew, because a row stamped at T can
// become visible at T+Δ and a cutoff that advanced past T would never see it
// again. Parity needs none of that: it is a comparison between two sets keyed by
// primary key, not a window over a clock. Commit skew and clock skew are
// properties of a global temporal cutoff, and there is no global temporal cutoff
// here.
//
// # Direction
//
// This sweep is FORWARD only: for every live source root, is there a document at
// least as fresh? The reverse direction — a document whose source row is gone —
// is deliberately not run against a live slot, because "absent from source" is a
// normal transient there (a row deleted a millisecond ago whose DELETED event is
// still in flight) and acting on it is destructive. Reverse completeness runs
// where it is safe and already implemented: against a SHADOW slot before a flip,
// where the snapshot ordering makes it exact.

// reconcileDefaults are the framework defaults when the caller passes zeroes.
// The rate is deliberately conservative: the sweep is a backstop, not a
// throughput contest, and its whole cost profile must be something an operator
// can reason about from one number.
const (
	reconcileDefaultBatchSize     = 1000
	reconcileDefaultRowsPerSecond = 5000
	// reconcileGrace is how long a divergence candidate is given to resolve
	// itself before it is treated as real. It sheds the in-flight transient: an
	// event for that aggregate may be between the source read and the projection
	// write at the exact moment the sweep looked.
	reconcileGrace = 2 * time.Second
)

// ReconcileConfig governs one reconciliation sweep.
type ReconcileConfig struct {
	// BatchSize is how many source rows are compared per round trip. 0 → the
	// framework default.
	BatchSize int
	// RowsPerSecond throttles the sweep. It is the ONLY cost knob, and the full
	// pass duration derives from it: table size / rate. That duration is the
	// convergence bound this backstop provides, so it belongs in the deployment's
	// stated SLO rather than being an emergent property. 0 → the framework
	// default; negative → unthrottled.
	RowsPerSecond int
}

// ReconcileReport is one sweep's outcome — the evidence that I1 holds, or the
// measurement of by how much it does not.
type ReconcileReport struct {
	View       string
	Scanned    int
	Missing    int
	Stale      int
	Repaired   int
	Unrepaired int
	Duration   time.Duration
}

// Diverged is the total confirmed divergence found by the sweep.
func (r ReconcileReport) Diverged() int { return r.Missing + r.Stale }

// ReconcileView runs one full forward-parity pass over a view and repairs what
// it finds. It is safe to run concurrently with live projection: repairs go
// through the same revision-guarded pipeline every other writer uses, so a
// repair carrying older data than a concurrent event is suppressed by the guard
// rather than regressing the document.
func (s *SyncEngine) ReconcileView(ctx context.Context, view *ViewDefinition, cfg ReconcileConfig) (ReconcileReport, error) {
	started := time.Now()
	report := ReconcileReport{View: view.Name()}


	schema := view.SchemaDef()
	if schema == nil {
		return report, fmt.Errorf("reconcile %q: view declares no root .Schema(...)", view.name)
	}
	revCol := schema.RevisionColumn()
	if revCol == "" {
		// Unreachable on the canonical path (Revision is boot-mandatory), but a
		// view whose root somehow carries none cannot be parity-checked, and
		// silently degrading to a presence-only check would misreport coverage.
		return report, fmt.Errorf("reconcile %q: root schema declares no Revision column — parity is not defined", view.name)
	}
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = reconcileDefaultBatchSize
	}

	d := s.eng.Dialect()
	pkCol := schema.IDColumn()
	table := d.QuoteIdent(view.RootTable())
	qPK := d.QuoteIdent(pkCol)
	qRev := d.QuoteIdent(revCol)

	throttle := newRateLimiter(cfg.RowsPerSecond)
	var cursor string
	for {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		page, err := s.reconcilePage(ctx, table, qPK, qRev, cursor, batch)
		if err != nil {
			return report, fmt.Errorf("reconcile %q: scan page: %w", view.name, err)
		}
		if len(page.ids) == 0 {
			break
		}
		report.Scanned += len(page.ids)
		cursor = page.ids[len(page.ids)-1]

		missing, stale, err := s.parityCandidates(ctx, view, page)
		if err != nil {
			return report, fmt.Errorf("reconcile %q: %w", view.name, err)
		}
		if n := len(missing) + len(stale); n > 0 {
			report.Missing += len(missing)
			report.Stale += len(stale)
			repaired := append(append([]string{}, missing...), stale...)
			if err := s.recomposeInto(ctx, view, s.resolver.Active(view.name), repaired); err != nil {
				report.Unrepaired += n
				slog.ErrorContext(ctx, "projection.reconcile.repair_failed",
					slog.String("view", view.name), slog.Int("documents", n),
					slog.String("err", err.Error()))
			} else {
				report.Repaired += n
			}
		}
		throttle.wait(ctx, len(page.ids))
	}

	report.Duration = time.Since(started)
	return report, nil
}

// sourcePage is one page of (pk, revision) read from the relational source.
type sourcePage struct {
	ids  []string
	revs map[string]int64
}

// reconcilePage reads the next page of source roots in primary-key order.
// Keyset paging (pk > cursor) rather than OFFSET, so page N costs the same as
// page 1 no matter how deep the sweep has walked.
func (s *SyncEngine) reconcilePage(ctx context.Context, table, qPK, qRev, cursor string, limit int) (sourcePage, error) {
	d := s.eng.Dialect()
	out := sourcePage{revs: map[string]int64{}}

	sql := "SELECT " + qPK + ", " + qRev + " FROM " + table
	var args []any
	if cursor != "" {
		sql += " WHERE " + qPK + " > " + d.Placeholder(1)
		args = append(args, d.EncodeArg(domain.NewID(cursor)))
	}
	sql = d.ApplyLimit(sql+" ORDER BY "+qPK, limit)

	rows, err := s.eng.Querier().Query(ctx, sql, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var rev int64
		if err := rows.Scan(&raw, &rev); err != nil {
			return out, err
		}
		id, err := d.DecodeID(raw)
		if err != nil {
			return out, err
		}
		out.ids = append(out.ids, id)
		out.revs[id] = rev
	}
	return out, rows.Err()
}

// parityCandidates compares one page against the live slot and returns the
// CONFIRMED divergences, split by kind.
//
// Confirmation is a second look after a grace period, and it deliberately
// compares against the revision the SOURCE had on the first read rather than
// re-reading the source: the question is "did the projection catch up to what
// the source held when we looked?", which is precisely what distinguishes a
// transient in-flight write from real divergence. Re-reading the source would
// instead chase a moving target and could never converge on a busy aggregate.
func (s *SyncEngine) parityCandidates(ctx context.Context, view *ViewDefinition, page sourcePage) (missing, stale []string, err error) {
	active := s.resolver.Active(view.name)
	stored, err := s.mongo.RevisionsByIDs(ctx, active, page.ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read stored revisions: %w", err)
	}
	var suspects []string
	for _, id := range page.ids {
		got, present := stored[id]
		if !present || got < page.revs[id] {
			suspects = append(suspects, id)
		}
	}
	if len(suspects) == 0 {
		return nil, nil, nil
	}

	select {
	case <-time.After(reconcileGrace):
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	recheck, err := s.mongo.RevisionsByIDs(ctx, active, suspects)
	if err != nil {
		return nil, nil, fmt.Errorf("re-read stored revisions: %w", err)
	}
	for _, id := range suspects {
		got, present := recheck[id]
		if !present {
			missing = append(missing, id)
			continue
		}
		if got < page.revs[id] {
			stale = append(stale, id)
		}
	}
	return missing, stale, nil
}

// rateLimiter paces the sweep to a rows-per-second budget by sleeping between
// batches. Crude on purpose: the sweep's cost must be predictable from one
// number an operator sets, not from a control loop nobody can reason about.
type rateLimiter struct{ perSecond int }

func newRateLimiter(perSecond int) rateLimiter {
	if perSecond == 0 {
		perSecond = reconcileDefaultRowsPerSecond
	}
	return rateLimiter{perSecond: perSecond}
}

func (r rateLimiter) wait(ctx context.Context, rows int) {
	if r.perSecond < 0 || rows <= 0 {
		return
	}
	d := time.Duration(float64(rows) / float64(r.perSecond) * float64(time.Second))
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// ReconcileAllViews sweeps every view this engine projects, serialized across
// pods per view by an advisory lock, and returns one report per view swept.
//
// The lock name is SEPARATE from the rebuild lock on purpose. Sharing it would
// let a slow sweep block a boot rebuild, which is the more urgent operation; a
// distinct name keeps sweeps one-at-a-time per view across the deployment
// without ever standing in a rebuild's way.
//
// A view with a rebuild in flight is SKIPPED rather than swept. Its documents
// are legitimately mid-migration — the backfill is still filling the shadow and
// dual-apply is fanning live writes into it — so parity against the active slot
// would report divergence that is not divergence, and repairing it would fight
// the backfill for the same documents.
//
// This is deliberately NOT started by the library on its own: turning a
// continuous background scan on is the operator's call, made through the
// `mongo.reconcile` yaml block (off by default) — bootstrap then drives
// RunReconcileLoop. Off in dev is a feature, not a gap: a projection bug
// surfaces as a stale document instead of being quietly repaired.
func (s *SyncEngine) ReconcileAllViews(ctx context.Context, cfg ReconcileConfig) ([]ReconcileReport, error) {
	seen := map[string]bool{}
	var views []*ViewDefinition
	for _, vs := range s.index.byPGTable {
		for _, v := range vs {
			if !seen[v.name] {
				seen[v.name] = true
				views = append(views, v)
			}
		}
	}

	var reports []ReconcileReport
	for _, view := range views {
		if _, rebuilding := s.resolver.ShadowActive(view.name); rebuilding {
			slog.InfoContext(ctx, "projection.reconcile.skipped",
				slog.String("view", view.name), slog.String("reason", "rebuild in flight"))
			continue
		}
		rep, err := s.reconcileLocked(ctx, view, cfg)
		if err != nil {
			return reports, err
		}
		if rep != nil {
			reports = append(reports, *rep)
		}
	}
	s.metrics.reconciled(time.Now())
	return reports, nil
}

// RunReconcileLoop drives ReconcileAllViews on a cadence until the context
// ends: one full pass, then `every` of pause, then the next — measured from the
// END of a pass to the START of the next, so a slow pass never overlaps its
// successor. This is the scheduled form of the sweep (the `mongo.reconcile`
// yaml block); the pass itself is unchanged — advisory-lock guarded per view,
// rebuilds skipped, repairs through the normal guards.
//
// A pass that errors is logged and the loop continues: the sweep is a backstop,
// and a backstop that dies on its first bad pass protects nothing. Staleness of
// ProjectionHealth().LastReconcile is the honest signal that passes stopped
// completing.
func (s *SyncEngine) RunReconcileLoop(ctx context.Context, every time.Duration, cfg ReconcileConfig) {
	for ctx.Err() == nil {
		if _, err := s.ReconcileAllViews(ctx, cfg); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "projection.reconcile.pass_failed",
				slog.String("err", err.Error()))
		}
		select {
		case <-time.After(every):
		case <-ctx.Done():
			return
		}
	}
}

// reconcileLocked runs one view's sweep under its advisory lock. A lock held by
// another pod means that pod is already sweeping this view: nil, no error.
func (s *SyncEngine) reconcileLocked(ctx context.Context, view *ViewDefinition, cfg ReconcileConfig) (*ReconcileReport, error) {
	lock, err := s.eng.AcquireRebuildLock(ctx, "reconcile:"+view.name)
	if err != nil {
		return nil, fmt.Errorf("reconcile %q: acquire lock: %w", view.name, err)
	}
	defer func() {
		if rerr := lock.Release(ctx); rerr != nil {
			slog.WarnContext(ctx, "projection.reconcile.lock_release_failed",
				slog.String("view", view.name), slog.String("err", rerr.Error()))
		}
	}()
	if !lock.Acquired() {
		return nil, nil // another pod owns this view's sweep
	}

	rep, err := s.ReconcileView(ctx, view, cfg)
	if err != nil {
		return nil, err
	}
	// Always report, never only on divergence. A sweep that finds nothing is the
	// evidence that I1 holds; without the clean passes in the record, the noisy
	// ones have no denominator.
	slog.InfoContext(ctx, "projection.reconcile.pass",
		slog.String("view", rep.View),
		slog.Int("scanned", rep.Scanned),
		slog.Int("missing", rep.Missing),
		slog.Int("stale", rep.Stale),
		slog.Int("repaired", rep.Repaired),
		slog.Int("unrepaired", rep.Unrepaired),
		// Milliseconds, not slog.Duration — see view.rebuild.end.
		slog.Float64("durationMs", float64(rep.Duration.Nanoseconds())/1e6))
	return &rep, nil
}
