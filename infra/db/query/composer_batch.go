package query

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/hydrate"
)

// composer_batch.go holds the SET-BASED companions of the per-row merge chain in
// composer.go. The per-row helpers (used by Compose on a single root) fetch each
// related source with one query per aggregate — fine for one document, an N+1
// storm on a rebuild of millions. The batched helpers below fetch every related
// row for a WHOLE batch of roots in one IN (...) query per related table (chunked
// at maxInClauseSize) and group them in memory by the join key, so composeRows —
// the shared body of ComposeAll and ComposeBatch — pays one round trip per table
// per batch instead of one per aggregate. This is round-trip-bound work, so the
// win is largest on the engines with the heaviest per-query cost.
//
// Correctness rests on one property: the group key is fmt("%v", row[col]) taken on
// BOTH sides — the parent's ID/ParentID value AND the child's ParentID value. Both are the same
// id-typed column read through the same core.Querier.QueryMaps surface, so equal
// ids stringify equally regardless of the backend's physical id encoding; and each
// key is re-encoded through the exact encodeKey the single-row path uses, so the IN
// predicate matches the stored column on every dialect.

// composeBaseRootedRowsBatched is the set-based composeBaseRootedRow: for a batch
// of SharedBaseView base rows it nests the base's own children, then fills ONE
// sub-document per role. The role row per base is chosen active-first exactly as
// fetchRoleRow does — the batch fetches every active role row in one IN lookup and
// falls back to the per-base latest-archived remnant ONLY for bases with no active
// row (the rare archived-only identity), so the common case is fully set-based.
func (c *Composer) composeBaseRootedRowsBatched(ctx context.Context, view *ViewDefinition, rows []Document, includeArchived bool) error {
	base := view.schema
	basePK := hydrate.SchemaPK(base)
	if err := c.h.MergeOwnChildrenBatch(ctx, rows, base, includeArchived); err != nil {
		return err
	}
	for _, r := range view.roles {
		_, fkCol, _ := r.schema.SharedBaseRef()
		sd, hasSD := hydrate.SchemaDeletedAt(r.schema)
		baseIDs := hydrate.CollectKeys(rows, basePK)

		// Active role rows (or, without DeletedAt, simply the row) for the whole
		// batch in one lookup, grouped by the base ParentID.
		var grouped map[string][]Document
		var err error
		if !hasSD {
			grouped, err = c.h.FetchInGrouped(ctx, r.schema, r.schema.Table(), fkCol, baseIDs, "", true)
		} else {
			grouped, err = c.h.FetchInGrouped(ctx, r.schema, r.schema.Table(), fkCol, baseIDs, sd, false)
		}
		if err != nil {
			return err
		}

		roleByBase := make(map[string]Document, len(rows))
		chosen := make([]Document, 0, len(rows))
		for _, row := range rows {
			bid := hydrate.KeyOf(row, basePK)
			if bid == "" {
				continue
			}
			if rr := grouped[bid]; len(rr) > 0 {
				roleByBase[bid] = rr[0]
				chosen = append(chosen, rr[0])
			}
		}
		// Fallback for bases with no active row: the most recently archived remnant
		// (per-base, matching fetchRoleRow's step 2). Rare — only archived-only roles.
		if hasSD && includeArchived {
			for _, row := range rows {
				bid := hydrate.KeyOf(row, basePK)
				if bid == "" || roleByBase[bid] != nil {
					continue
				}
				arch, err := c.h.FetchLatestArchived(ctx, r.schema, fkCol, bid, sd)
				if err != nil {
					return err
				}
				if arch != nil {
					roleByBase[bid] = arch
					chosen = append(chosen, arch)
				}
			}
		}

		// The chosen role rows carry their own siblings + children — batched.
		if err := c.h.MergeOwnerSiblingsBatch(ctx, chosen, r.schema, includeArchived); err != nil {
			return err
		}
		if err := c.h.MergeOwnChildrenBatch(ctx, chosen, r.schema, includeArchived); err != nil {
			return err
		}

		// Attach the sub-document (an absent role writes an explicit nil segment so
		// the $set upsert overwrites a vanished role rather than leaving it stale).
		// The role row's physical revision column becomes the segment's _revision
		// watermark — same discipline as the per-row composeBaseRootedRow.
		for _, row := range rows {
			if rr := roleByBase[hydrate.KeyOf(row, basePK)]; rr != nil {
				hydrate.RemapRevision(rr, r.schema, docRevisionField)
				row[r.segment] = rr
			} else {
				row[r.segment] = nil
			}
		}
	}
	// Embeds are external (Mongo). The batched path resolves them SET-BASED across
	// the whole batch (one $in per embed source), same as the own relational legs.
	return c.applyEmbedsBatch(ctx, rows, basePK, view.embeds, includeArchived)
}

// findEmbedsGrouped fetches every embed doc whose keyCol is in the distinct keys
// (one FindManyByFieldIn per embed source — a single $in) and groups them by the
// stringified keyCol value. The set-based companion of the per-parent
// FindManyByField in fetchMongoEmbed.
func (c *Composer) findEmbedsGrouped(ctx context.Context, coll PhysicalCollection, keyCol string, keys []any) (map[string][]Document, error) {
	out := make(map[string][]Document)
	if len(keys) == 0 {
		return out, nil
	}
	seen := make(map[string]struct{}, len(keys))
	uniq := make([]any, 0, len(keys))
	for _, k := range keys {
		ks := fmt.Sprintf("%v", k)
		if _, ok := seen[ks]; ok {
			continue
		}
		seen[ks] = struct{}{}
		uniq = append(uniq, k)
	}
	docs, err := c.mongo.FindManyByFieldIn(ctx, coll, keyCol, uniq)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		v, ok := d[keyCol]
		if !ok || v == nil {
			continue
		}
		gk := fmt.Sprintf("%v", v)
		out[gk] = append(out[gk], d)
	}
	return out, nil
}

// applyEmbedsBatch is the set-based applyEmbeds: for a whole batch of parent docs
// it resolves each external embed in ONE $in per embed source instead of one
// FindManyByField per parent, groups the results by the join key, and lands the
// 1:1 sub-document / 1:N array on each parent with the SAME null semantics the
// per-row path (fetchMongoEmbed) produces:
//   - 1:N: a parent with a nil/absent join key gets no field; otherwise the field
//     is the (possibly nil) grouped slice — identical to FindManyByField's result.
//   - 1:1: a parent with a nil/absent ParentID, or no matching embed doc, leaves the
//     field ABSENT; otherwise it is the single matched doc.
//
// Embeds are single-level (a source carries no embeds of its own), so this
// resolves exactly the view's top-level embeds — one $in per embed source, no
// descent.
func (c *Composer) applyEmbedsBatch(ctx context.Context, docs []Document, parentPK string, embeds []embedDef, includeArchived bool) error {
	if len(embeds) == 0 || len(docs) == 0 {
		return nil
	}
	if c.mongo == nil {
		return fmt.Errorf("composer: view embed requires a MongoDB handle " +
			"(builder constructed without NewComposerWithMongo)")
	}
	for _, e := range embeds {
		coll := c.resolver.Active(e.leg.Collection())
		if e.many {
			// 1:N — embed.JoinColumn == parent.ID.
			keys := make([]any, 0, len(docs))
			for _, doc := range docs {
				if v, ok := doc[parentPK]; ok && v != nil {
					keys = append(keys, v)
				}
			}
			grouped, err := c.findEmbedsGrouped(ctx, coll, e.JoinColumn(), keys)
			if err != nil {
				return err
			}
			allow := embedTrimSet(e)
			for _, doc := range docs {
				v, ok := doc[parentPK]
				if !ok || v == nil {
					continue
				}
				doc[e.Field()] = trimDocsToFields(grouped[fmt.Sprintf("%v", v)], allow)
			}
		} else {
			// 1:1 — embed._id == parent[JoinColumn].
			keys := make([]any, 0, len(docs))
			for _, doc := range docs {
				if v, ok := doc[e.JoinColumn()]; ok && v != nil {
					keys = append(keys, v)
				}
			}
			grouped, err := c.findEmbedsGrouped(ctx, coll, "_id", keys)
			if err != nil {
				return err
			}
			allow := embedTrimSet(e)
			for _, doc := range docs {
				// Unresolved 1:1 → explicit null, same reason as the per-row
				// path: $set-merged writes would keep a stale sub-document if
				// the key were omitted.
				v, ok := doc[e.JoinColumn()]
				if !ok || v == nil {
					doc[e.Field()] = nil
					continue
				}
				rows := grouped[fmt.Sprintf("%v", v)]
				if len(rows) == 0 {
					doc[e.Field()] = nil
					continue
				}
				doc[e.Field()] = trimToFields(rows[0], allow)
			}
		}
	}
	return nil
}
