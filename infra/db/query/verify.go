package query

import (
	"context"
	"fmt"
	"log/slog"
)

// verifyReconcileChunk bounds how many ids a single forward-recompose, reverse-delete
// or parity batch carries, so the reconcile buffers stay bounded even when a
// shadow is far off (normally all of them are near-empty).
const verifyReconcileChunk = 1000

// verifyShadow validates a freshly-built shadow slot before a flip. Three passes:
//
//  1. Forward PARITY: every live source root must have a shadow document whose
//     revision watermark is at least the source's. This subsumes both the old
//     forward-completeness check (an absent document is the extreme case of
//     being behind) and the old value SAMPLE.
//  2. Repair: everything the parity pass flagged is recomposed and written.
//  3. Reverse completeness: shadow documents whose root no longer exists in the
//     source are deleted, so the flip never freezes an orphan.
//
// The value sample this replaced compared FIELD SHAPE on twenty documents — and
// not a random twenty, but the first twenty the source cursor yielded, biased to
// the oldest rows, i.e. precisely the ones least likely to exercise a recent
// shape change. A systematic corruption of 1%% of documents survived that check
// about 82%% of the time. Revision parity covers 100%% of documents instead, at
// set-comparison cost, because it compares a watermark both sides already carry
// rather than recomposing anything.
//
// Snapshot order matters: the shadow ids are read BEFORE the source ids, so a row
// inserted mid-verify (already dual-applied into the shadow) is never mistaken
// for an orphan and deleted. The source rows are STREAMED against the frozen
// shadow set rather than materialized into a second full set — only the shadow
// snapshot plus small bounded buffers live at once, while the completeness
// guarantee stays exact and the snapshot ordering is preserved.
func (s *SyncEngine) verifyShadow(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection) error {
	shadowIDs, err := s.mongo.SnapshotDocumentIDs(ctx, shadow)
	if err != nil {
		return fmt.Errorf("verify %q: snapshot shadow ids: %w", view.name, err)
	}

	// Stream the source rows against the frozen shadow set, comparing revisions in
	// bounded batches. A matched id is present (removed from the set); an id whose
	// shadow document is absent OR behind the source revision is a divergence.
	var diverged []string
	batchIDs := make([]string, 0, verifyReconcileChunk)
	batchRevs := make(map[string]int64, verifyReconcileChunk)

	flush := func() error {
		if len(batchIDs) == 0 {
			return nil
		}
		stored, err := s.mongo.RevisionsByIDs(ctx, shadow, batchIDs)
		if err != nil {
			return fmt.Errorf("verify %q: read shadow revisions: %w", view.name, err)
		}
		for _, id := range batchIDs {
			got, present := stored[id]
			if !present || got < batchRevs[id] {
				diverged = append(diverged, id)
			}
		}
		batchIDs = batchIDs[:0]
		batchRevs = make(map[string]int64, verifyReconcileChunk)
		return nil
	}

	if err := s.streamSourceRevisions(ctx, view, func(id string, rev int64) error {
		delete(shadowIDs, id)
		batchIDs = append(batchIDs, id)
		batchRevs[id] = rev
		if len(batchIDs) >= verifyReconcileChunk {
			return flush()
		}
		return nil
	}); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}

	// Repair. The source cursor is closed now, so recompose may borrow pool
	// connections freely. Chunked so a far-off shadow does not build one
	// oversized batch.
	for start := 0; start < len(diverged); start += verifyReconcileChunk {
		end := start + verifyReconcileChunk
		if end > len(diverged) {
			end = len(diverged)
		}
		if err := s.recomposeInto(ctx, view, shadow, diverged[start:end]); err != nil {
			return fmt.Errorf("verify %q: repair diverged ids: %w", view.name, err)
		}
	}

	// Reverse completeness (tail reconcile): whatever remains in the shadow set is
	// in the shadow but not in the source, i.e. an orphan. Chunked delete for the
	// same reason. This direction is exact HERE — and only here — because the
	// shadow snapshot was taken before the source scan; it is deliberately never
	// run against a live slot, where "absent from source" is a normal transient.
	if len(shadowIDs) > 0 {
		orphans := make([]string, 0, len(shadowIDs))
		for id := range shadowIDs {
			orphans = append(orphans, id)
		}
		for start := 0; start < len(orphans); start += verifyReconcileChunk {
			end := start + verifyReconcileChunk
			if end > len(orphans) {
				end = len(orphans)
			}
			if _, err := s.mongo.DeleteByIDs(ctx, shadow, orphans[start:end]); err != nil {
				return fmt.Errorf("verify %q: delete shadow orphans: %w", view.name, err)
			}
		}
	}

	// The dual-apply leak rate IS the health metric of the blue-green mechanism:
	// a rebuild that had to repair thousands of documents did not succeed quietly,
	// it revealed a broken dual-apply. It used to be computed and discarded.
	slog.InfoContext(ctx, "view.rebuild.verify",
		slog.String("view", view.name),
		slog.Int("diverged", len(diverged)),
		slog.Int("orphans", len(shadowIDs)))
	return nil
}

// streamSourceRevisions scans the view's live root (PK, revision) pairs from the
// relational source and invokes fn once per row — the streaming companion of a
// full-set scan, so verify never holds a second complete set in memory.
func (s *SyncEngine) streamSourceRevisions(ctx context.Context, view *ViewDefinition, fn func(id string, rev int64) error) error {
	idDialect := s.eng.Dialect()
	schema := view.SchemaDef()
	if schema == nil {
		return fmt.Errorf("verify %q: view declares no root .Schema(...)", view.name)
	}
	revCol := schema.RevisionColumn()
	if revCol == "" {
		return fmt.Errorf("verify %q: root schema declares no Revision column — parity is not defined", view.name)
	}
	q := "SELECT " + idDialect.QuoteIdent(schema.PKColumn()) + ", " + idDialect.QuoteIdent(revCol) +
		" FROM " + idDialect.QuoteIdent(view.RootTable())
	rows, err := s.eng.Querier().Query(ctx, q)
	if err != nil {
		return fmt.Errorf("verify %q: scan source revisions: %w", view.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var rev int64
		if err := rows.Scan(&raw, &rev); err != nil {
			return err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			return err
		}
		if err := fn(id, rev); err != nil {
			return err
		}
	}
	return rows.Err()
}

// recomposeInto composes the given ids from the source and applies the non-nil
// results into target as GUARDED pipelines — the same discipline as the
// backfill batches (a verify repair racing a fresher dual-applied write must
// not regress it). Used by verify's forward pass to fill a gap.
func (s *SyncEngine) recomposeInto(ctx context.Context, view *ViewDefinition, target PhysicalCollection, ids []string) error {
	composed, err := s.composer.ComposeBatch(ctx, view, ids)
	if err != nil {
		return err
	}
	if len(composed) == 0 {
		return nil
	}
	pkCol := view.schema.PKColumn()
	items := make([]IdentifiedStages, 0, len(composed))
	for _, doc := range composed {
		items = append(items, IdentifiedStages{ID: fmt.Sprintf("%v", doc[pkCol]), Stages: consultGuardedStages(view, doc)})
	}
	return s.mongo.BulkApplyProjection(ctx, target, items)
}
