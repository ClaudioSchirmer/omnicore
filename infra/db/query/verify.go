package query

import (
	"context"
	"fmt"
	"sort"
)

// verifySampleSize bounds the value-sample pass: this many source ids are
// recomposed and structurally compared to their shadow document before a flip.
const verifySampleSize = 20

// verifyReconcileChunk bounds how many ids a single forward-recompose or
// reverse-delete batch carries, so the reconcile buffers stay bounded even when
// a shadow is far off (normally both are near-empty).
const verifyReconcileChunk = 1000

// verifyShadow validates a freshly-built shadow slot before a flip. Three passes:
//
//  1. Reverse completeness (the G2b tail reconcile): shadow documents whose root
//     no longer exists in the source are deleted — a doc resurrected by the
//     backfill/delete race is removed so the flip never freezes an orphan.
//  2. Forward completeness (the G1 net): every live source root that lacks a
//     shadow document is recomposed and written, auto-correcting a dropped
//     dual-apply write; a compose that yields nothing is legitimately absent.
//  3. Value sample: a bounded sample of source ids is recomposed and its field
//     shape compared to the stored shadow document; a persistent structural
//     mismatch (re-checked once to shed a transient in-flight write) aborts.
//
// Snapshot order matters: the shadow ids are read BEFORE the source ids, so a row
// inserted mid-verify (already dual-applied into the shadow) is never mistaken
// for an orphan and deleted. The source ids are STREAMED against the frozen
// shadow set rather than materialized into a second full set — only the shadow
// snapshot plus small reconcile buffers live at once (half the footprint of the
// old two-full-set diff), while the completeness guarantee stays exact and the
// snapshot ordering is preserved (the shadow set is the frozen reference).
func (s *SyncEngine) verifyShadow(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection) error {
	shadowIDs, err := s.mongo.SnapshotDocumentIDs(ctx, shadow)
	if err != nil {
		return fmt.Errorf("verify %q: snapshot shadow ids: %w", view.name, err)
	}

	// Stream the source ids against the frozen shadow set: a matched id is present
	// (removed from the set), an unmatched id is a forward gap (recompose). No
	// recompose runs DURING the stream, so the source cursor never contends with a
	// composer connection. A bounded sample of source ids is captured in passing
	// for pass 3.
	var missing []string
	sample := make([]string, 0, verifySampleSize)
	if err := s.streamSourceIDs(ctx, view, func(id string) error {
		if _, ok := shadowIDs[id]; ok {
			delete(shadowIDs, id)
		} else {
			missing = append(missing, id)
		}
		if len(sample) < verifySampleSize {
			sample = append(sample, id)
		}
		return nil
	}); err != nil {
		return err
	}

	// Pass 2 — forward completeness (auto-correct the gaps). The source cursor is
	// closed now, so recompose may borrow pool connections freely. Chunked so a
	// far-off shadow does not build one oversized batch.
	for start := 0; start < len(missing); start += verifyReconcileChunk {
		end := start + verifyReconcileChunk
		if end > len(missing) {
			end = len(missing)
		}
		if err := s.recomposeInto(ctx, view, shadow, missing[start:end]); err != nil {
			return fmt.Errorf("verify %q: auto-correct missing ids: %w", view.name, err)
		}
	}

	// Pass 1 — reverse completeness (tail reconcile): whatever remains in the
	// shadow set is in the shadow but not in the source, i.e. an orphan. Chunked
	// delete for the same reason.
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

	// Pass 3 — value sample.
	return s.verifyValueSample(ctx, view, shadow, sample)
}

// streamSourceIDs scans the view's live root PKs from the relational source and
// invokes fn once per id — the streaming companion of the old full-set scan, so
// verify never holds a second complete id set in memory.
func (s *SyncEngine) streamSourceIDs(ctx context.Context, view *ViewDefinition, fn func(id string) error) error {
	idDialect := s.eng.Dialect()
	schema := view.SchemaDef()
	if schema == nil {
		return fmt.Errorf("verify %q: view declares no root .Schema(...)", view.name)
	}
	q := "SELECT " + idDialect.QuoteIdent(schema.PKColumn()) + " FROM " + idDialect.QuoteIdent(view.rootTable)
	rows, err := s.eng.Querier().Query(ctx, q)
	if err != nil {
		return fmt.Errorf("verify %q: scan source ids: %w", view.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			return err
		}
		if err := fn(id); err != nil {
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

// verifyValueSample recomposes a bounded sample of source ids and compares each
// to its stored shadow document by field shape. A mismatch is re-checked once
// (a concurrent in-flight write can transiently diverge) before it aborts. The
// sample is gathered by verifyShadow while it streams the source ids.
func (s *SyncEngine) verifyValueSample(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection, sample []string) error {
	for _, id := range sample {
		ok, err := s.sampleMatches(ctx, view, shadow, id)
		if err != nil {
			return err
		}
		if ok {
			continue
		}
		// Re-check once to shed a transient in-flight write.
		ok, err = s.sampleMatches(ctx, view, shadow, id)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("verify %q: shadow document %q diverges in shape from a fresh compose%s",
				view.name, id, s.shapeDiffDetail(ctx, view, shadow, id))
		}
	}
	return nil
}

// sampleMatches recomposes one id and compares its top-level field shape to the
// stored shadow document. Structural (field-name set) rather than deep-value to
// survive the BSON round-trip; deep value comparison is an integration concern.
func (s *SyncEngine) sampleMatches(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection, id string) (bool, error) {
	fresh, err := s.composer.Compose(ctx, view, id)
	if err != nil {
		return false, err
	}
	stored, err := s.mongo.FindManyByField(ctx, shadow, "_id", id)
	if err != nil {
		return false, err
	}
	if fresh == nil {
		// The source row vanished since the scan — absence is fine, presence isn't.
		return len(stored) == 0, nil
	}
	if len(stored) == 0 {
		return false, nil // the forward pass should have filled it
	}
	return sameFieldShape(fresh, stored[0]), nil
}

// shapeDiffDetail names the diverging top-level keys for the verify error —
// " (fresh-only: [...]; stored-only: [...])" — so an equivalence failure is
// diagnosable from the log alone. Best-effort: any re-read error yields "".
func (s *SyncEngine) shapeDiffDetail(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection, id string) string {
	fresh, err := s.composer.Compose(ctx, view, id)
	if err != nil || fresh == nil {
		return ""
	}
	stored, err := s.mongo.FindManyByField(ctx, shadow, "_id", id)
	if err != nil || len(stored) == 0 {
		return ""
	}
	fk := fieldNamesExceptID(fresh)
	sk := fieldNamesExceptID(stored[0])
	var freshOnly, storedOnly []string
	for k := range fk {
		if _, ok := sk[k]; !ok {
			freshOnly = append(freshOnly, k)
		}
	}
	for k := range sk {
		if _, ok := fk[k]; !ok {
			storedOnly = append(storedOnly, k)
		}
	}
	sort.Strings(freshOnly)
	sort.Strings(storedOnly)
	return fmt.Sprintf(" (fresh-only: %v; stored-only: %v)", freshOnly, storedOnly)
}

// sameFieldShape compares two documents by their top-level field names,
// ignoring Mongo's _id AND every framework-internal "_"-prefixed field (the
// projection watermarks _revision/_base_revision): a document written before
// the watermarks existed must not read as drift against a fresh compose.
func sameFieldShape(fresh, stored Document) bool {
	fk := fieldNamesExceptID(fresh)
	sk := fieldNamesExceptID(stored)
	if len(fk) != len(sk) {
		return false
	}
	for k := range fk {
		if _, ok := sk[k]; !ok {
			return false
		}
	}
	return true
}

func fieldNamesExceptID(d Document) map[string]struct{} {
	set := make(map[string]struct{}, len(d))
	for k := range d {
		if len(k) > 0 && k[0] == '_' {
			continue // _id + framework watermarks (_revision/_base_revision)
		}
		set[k] = struct{}{}
	}
	return set
}
