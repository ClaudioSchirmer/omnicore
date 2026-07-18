package query

import (
	"context"
	"fmt"
)

// verifySampleSize bounds the value-sample pass: this many source ids are
// recomposed and structurally compared to their shadow document before a flip.
const verifySampleSize = 20

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
// for an orphan and deleted.
func (s *SyncEngine) verifyShadow(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection) error {
	shadowIDs, err := s.mongo.SnapshotDocumentIDs(ctx, shadow)
	if err != nil {
		return fmt.Errorf("verify %q: snapshot shadow ids: %w", view.name, err)
	}
	sourceIDs, err := s.sourceIDSet(ctx, view)
	if err != nil {
		return err
	}

	// Pass 1 — reverse completeness (tail reconcile).
	var orphans []string
	for id := range shadowIDs {
		if _, live := sourceIDs[id]; !live {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		if _, err := s.mongo.DeleteByIDs(ctx, shadow, orphans); err != nil {
			return fmt.Errorf("verify %q: delete shadow orphans: %w", view.name, err)
		}
	}

	// Pass 2 — forward completeness (auto-correct).
	var missing []string
	for id := range sourceIDs {
		if _, present := shadowIDs[id]; !present {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		if err := s.recomposeInto(ctx, view, shadow, missing); err != nil {
			return fmt.Errorf("verify %q: auto-correct missing ids: %w", view.name, err)
		}
	}

	// Pass 3 — value sample.
	return s.verifyValueSample(ctx, view, shadow, sourceIDs)
}

// sourceIDSet returns the set of live root PKs of the view from the relational
// source — the ground truth the shadow is verified against.
func (s *SyncEngine) sourceIDSet(ctx context.Context, view *ViewDefinition) (map[string]struct{}, error) {
	idDialect := s.eng.Dialect()
	schema := view.SchemaDef()
	if schema == nil {
		return nil, fmt.Errorf("verify %q: view declares no root .Schema(...)", view.name)
	}
	q := "SELECT " + idDialect.QuoteIdent(schema.PKColumn()) + " FROM " + idDialect.QuoteIdent(view.rootTable)
	rows, err := s.eng.Querier().Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("verify %q: scan source ids: %w", view.name, err)
	}
	defer rows.Close()
	set := make(map[string]struct{})
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		id, err := idDialect.DecodeID(raw)
		if err != nil {
			return nil, err
		}
		set[id] = struct{}{}
	}
	return set, rows.Err()
}

// recomposeInto composes the given ids from the source and bulk-upserts the
// non-nil results into target. Used by verify's forward pass to fill a gap.
func (s *SyncEngine) recomposeInto(ctx context.Context, view *ViewDefinition, target PhysicalCollection, ids []string) error {
	composed, err := s.composer.ComposeBatch(ctx, view, ids)
	if err != nil {
		return err
	}
	if len(composed) == 0 {
		return nil
	}
	pkCol := view.schema.PKColumn()
	docs := make([]IdentifiedDocument, 0, len(composed))
	for _, doc := range composed {
		docs = append(docs, IdentifiedDocument{ID: fmt.Sprintf("%v", doc[pkCol]), Doc: doc})
	}
	return s.mongo.BulkUpsert(ctx, target, docs)
}

// verifyValueSample recomposes a bounded sample of source ids and compares each
// to its stored shadow document by field shape. A mismatch is re-checked once
// (a concurrent in-flight write can transiently diverge) before it aborts.
func (s *SyncEngine) verifyValueSample(ctx context.Context, view *ViewDefinition, shadow PhysicalCollection, sourceIDs map[string]struct{}) error {
	sample := make([]string, 0, verifySampleSize)
	for id := range sourceIDs {
		sample = append(sample, id)
		if len(sample) >= verifySampleSize {
			break
		}
	}
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
			return fmt.Errorf("verify %q: shadow document %q diverges in shape from a fresh compose", view.name, id)
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

// sameFieldShape compares two documents by their top-level field names, ignoring
// Mongo's _id (present on the stored doc, absent on a fresh compose).
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
		if k != "_id" {
			set[k] = struct{}{}
		}
	}
	return set
}
