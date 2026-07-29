package query

import (
	"context"
	"fmt"
	"time"
)

// The projection-state registry — the durable RENDEZVOUS of the projection's
// distributed writers. It closes the two windows a per-document revision guard
// cannot see:
//
//   - a document that MATERIALIZES LATE: a shared-base fan-out discovers its
//     targets from a FindIDsByField snapshot, so a role document whose
//     INSERTED/UNARCHIVED projection had not landed yet is missed, and its own
//     payload carries base state that may already be stale. The registry keeps
//     the highest base revision ever pushed; the writer of a late-born
//     document re-checks it AFTER writing and repairs by consult when the
//     registry proves a newer fan-out already passed.
//   - a document that DIES: DELETED removes the document, and absence carries
//     no watermark — a zombie consumer's older upsert would resurrect it. The
//     DELETED path records a tombstone (the row's last revision) BEFORE the
//     guarded delete; every document-creating upsert re-checks the tombstone
//     AFTER writing and removes its own write when the tombstone is newer.
//
// Both handshakes are write-then-check against a single store: if writer A's
// push (fan-out / delete) missed writer B's document, B's write entered the
// store's oplog AFTER A's probe, which came after A's registry write — so B's
// post-write registry read NECESSARILY sees A's record. One side always fires;
// the closure is a dependency, never a timing bet.
//
// The registry lives OUTSIDE the view slots (like the omnicore_mongo_views
// control plane): it is not view data, is never rebuilt, and needs no
// dual-apply. Document shapes:
//
//	{_id: "base:<baseTable>:<baseID>", base_revision: <int64>}  — base revision
//	{_id: "doc:<view>:<docID>", rev: <int64>, at: <ISODate>}    — tombstone
//
// Tombstones carry the "at" stamp for the TTL sweep (EnsureProjectionState);
// base-revision documents never carry it and never expire. An base-revision
// document recreated by a zombie event after a base purge is inert garbage
// (no role document exists to pull from it) — bounded by the purge volume.

// ProjectionStateCollectionName is the physical name of the registry
// collection. Exported for the store adapters (EnsureProjectionState
// provisions its TTL index) and for ops tooling; everything else reaches it
// through the helpers below.
const ProjectionStateCollectionName = "omnicore_projection_state"

// TombstoneTTL is the retention window of a document tombstone — far beyond
// any realistic consumer lag/redelivery horizon, small enough that delete-heavy
// workloads do not accumulate forever. Exported for the store adapters
// (EnsureProjectionState provisions the TTL index with it).
const TombstoneTTL = 24 * time.Hour

// projectionStateCollection returns the registry's PhysicalCollection — the
// registry is slot-less, so the name maps 1:1.
func projectionStateCollection() PhysicalCollection {
	return PhysicalCollection{name: ProjectionStateCollectionName}
}

// baseRevisionID / tombstoneID render the registry _id forms.
func baseRevisionID(baseTable, baseID string) string {
	return "base:" + baseTable + ":" + baseID
}
func tombstoneID(view, id string) string {
	return "doc:" + view + ":" + id
}

// stampBaseRevision advances (never regresses) the registry's base-revision record
// for one shared identity. Runs BEFORE the fan-out's FindIDsByField probe —
// that order is the handshake's premise.
func (s *SyncEngine) stampBaseRevision(ctx context.Context, baseTable, baseID string, revision int64) error {
	if revision <= 0 {
		return nil
	}
	stages := []Document{guardedSetStage("base_revision", Document{}, revision)}
	return s.mongo.ApplyProjection(ctx, projectionStateCollection(), baseRevisionID(baseTable, baseID), stages, true)
}

// stampedBaseRevision reads the registry's base-revision record for one shared identity;
// 0 when the identity has no record yet.
func (s *SyncEngine) stampedBaseRevision(ctx context.Context, baseTable, baseID string) (int64, error) {
	docs, err := s.mongo.FindManyByField(ctx, projectionStateCollection(), "_id", baseRevisionID(baseTable, baseID))
	if err != nil || len(docs) == 0 {
		return 0, err
	}
	return watermarkOf(docs[0]["base_revision"]), nil
}

// dropBaseRevision removes the identity's base-revision record — the base purge's
// registry cleanup.
func (s *SyncEngine) dropBaseRevision(ctx context.Context, baseTable, baseID string) error {
	return s.mongo.Delete(ctx, projectionStateCollection(), baseRevisionID(baseTable, baseID))
}

// stampTombstone records a document tombstone: the view document keyed by id
// was deleted at revision rev. Advance-only on rev (a redelivered DELETED
// keeps the original stamp — and its original TTL window). Runs BEFORE the
// guarded delete, mirroring stampBaseRevision's write-then-probe order.
// createdAtMs > 0 scopes the tombstone to ONE incarnation of the id (the dead
// row's created_at instant, millisecond grain): the guarded delete and the
// creator's post-write check only kill documents whose stored created_at
// matches it, so a deterministic id REBORN under the same natural key is never
// mistaken for a zombie of the dead life. 0 (no CreatedAt on the schema)
// falls back to the revision-only tombstone.
func (s *SyncEngine) stampTombstone(ctx context.Context, view, id string, rev, createdAtMs int64) error {
	if rev <= 0 {
		return nil
	}
	fields := Document{"at": lit(time.Now().UTC())}
	if createdAtMs > 0 {
		fields["created_at"] = lit(createdAtMs)
	}
	stages := []Document{guardedSetStage("revision", fields, rev)}
	return s.mongo.ApplyProjection(ctx, projectionStateCollection(), tombstoneID(view, id), stages, true)
}

// tombstoneRevision reads the tombstone (revision + created_at discriminator) for
// one view document; (0, 0) when no tombstone exists.
func (s *SyncEngine) tombstoneRevision(ctx context.Context, view, id string) (int64, int64, error) {
	docs, err := s.mongo.FindManyByField(ctx, projectionStateCollection(), "_id", tombstoneID(view, id))
	if err != nil || len(docs) == 0 {
		return 0, 0, err
	}
	return watermarkOf(docs[0]["revision"]), watermarkOf(docs[0]["created_at"]), nil
}

// checkTombstone is the creator-side half of the delete handshake: after any
// document-creating upsert (payload-direct own doc, consult recompose, the
// pull repair) it re-reads the tombstone and, when the document was deleted at
// a revision >= the write's, removes the just-written document with the SAME
// guarded delete the DELETED path uses — so a write racing the delete from
// either side converges on "gone". myRev == 0 (no revision known) checks
// against any tombstone. Best-effort: a registry read failure logs and leaves
// the at-least-once redelivery to reconverge.
func (s *SyncEngine) checkTombstone(ctx context.Context, viewName, id string, myRev int64) error {
	rev, born, err := s.tombstoneRevision(ctx, viewName, id)
	if err != nil {
		return fmt.Errorf("tombstone check %s/%s: %w", viewName, id, err)
	}
	if rev == 0 || rev < myRev {
		return nil
	}
	// The guarded delete carries the tombstone's created_at discriminator: a REBORN
	// document (a fresher created_at under the same deterministic id) never
	// matches and survives — only the dead incarnation's zombies die.
	// A zombie removal is a REMOVAL: the views materializing this one must lose
	// the segment too, or they would keep serving a document the projection just
	// disowned. Captured before the delete (the pre-delete document is the only
	// route to a 1:N parent id) and signalled after it.
	before := s.viewSignal.Before(ctx, viewName, id)
	if err := s.mongo.DeleteGuarded(ctx, s.resolver.Active(viewName), id, rev, born); err != nil {
		return err
	}
	if shadow, on := s.resolver.ShadowActive(viewName); on {
		if err := s.dualApply(ctx, viewName, func() error { return s.mongo.DeleteGuarded(ctx, shadow, id, rev, born) }); err != nil {
			return err
		}
	}
	s.viewSignal.Deleted(ctx, viewName, id, before, rev)
	return nil
}
