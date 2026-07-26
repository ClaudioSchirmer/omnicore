package query

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
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
// BOTH sides — the parent's PK/FK value AND the child's FK value. Both are the same
// id-typed column read through the same core.Querier.QueryMaps surface, so equal
// ids stringify equally regardless of the backend's physical id encoding; and each
// key is re-encoded through the exact encodeKey the single-row path uses, so the IN
// predicate matches the stored column on every dialect.

// keyOf returns the stringified join-key value of col on doc, or "" when the
// column is absent or NULL (the per-row path skips such a doc; so does the batch).
func keyOf(doc Document, col string) string {
	v, ok := doc[col]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// collectKeys gathers the non-empty join keys of col across docs (duplicates kept
// — fetchInGrouped dedupes before binding the IN predicate).
func collectKeys(docs []Document, col string) []string {
	keys := make([]string, 0, len(docs))
	for _, d := range docs {
		if k := keyOf(d, col); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// emptyIfNil normalizes an absent group to a non-nil empty slice, so a childless
// root composes an empty array (matching fetchWhere's []Document{}), never a
// missing/null field the reader would mishandle.
func emptyIfNil(rows []Document) []Document {
	if rows == nil {
		return []Document{}
	}
	return rows
}

// fetchInGrouped fetches every row whose keyCol is in keys (deduped + chunked at
// maxInClauseSize, same soft-delete gate as fetchWhere) and groups them by the
// stringified keyCol value — the set-based companion of fetchWhere/fetchRow.
func (c *Composer) fetchInGrouped(ctx context.Context, schema *core.TableSchema, table, keyCol string, keys []string, sdCol string, includeArchived bool) (map[string][]Document, error) {
	out := make(map[string][]Document)
	seen := make(map[string]struct{}, len(keys))
	uniq := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		uniq = append(uniq, k)
	}
	if len(uniq) == 0 {
		return out, nil
	}
	d := c.eng.Dialect()
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	for start := 0; start < len(uniq); start += maxInClauseSize {
		end := start + maxInClauseSize
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, k := range chunk {
			ph[i] = d.Placeholder(i + 1)
			args[i] = c.encodeKey(k)
		}
		sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)%s",
			selectList(d, readColsWithKey(schema, keyCol)), d.QuoteIdent(table), d.QuoteIdent(keyCol), strings.Join(ph, ", "), cond)
		results, err := c.eng.Querier().QueryMaps(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		for _, m := range results {
			row := Document(m)
			coerceTypes(row, schema)
			k := fmt.Sprintf("%v", row[keyCol])
			out[k] = append(out[k], row)
		}
	}
	return out, nil
}

// mergeOwnerSiblingsBatch is the set-based mergeOwnerSiblings: each declared
// sibling is fetched for the whole batch by the shared PK, grouped, then merged
// FLAT into each owner doc (columns minus the shared PK). A sibling is 1:1 by PK,
// so at most one row per key — take the first.
func (c *Composer) mergeOwnerSiblingsBatch(ctx context.Context, docs []Document, ownerSchema *core.TableSchema, includeArchived bool) error {
	sibs := ownerSchema.Siblings()
	if len(sibs) == 0 || len(docs) == 0 {
		return nil
	}
	pkCol := schemaPK(ownerSchema)
	keys := collectKeys(docs, pkCol)
	for _, sib := range sibs {
		grouped, err := c.fetchInGrouped(ctx, sib, sib.Table(), pkCol, keys, "", includeArchived)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			rows := grouped[keyOf(doc, pkCol)]
			if len(rows) == 0 {
				continue
			}
			for col, val := range rows[0] {
				if col == pkCol {
					continue
				}
				doc[col] = val
			}
		}
	}
	return nil
}

// mergeSharedBaseBatch is the set-based mergeSharedBase: the shared base is fetched
// for the whole batch by its PK (the roles' FK values), then merged FLAT into each
// role doc — with the SAME managed-column skip as the per-row path so a base's
// NULL soft-delete / its timestamps never overwrite the role's authoritative ones.
func (c *Composer) mergeSharedBaseBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	keys := collectKeys(docs, fkCol)
	grouped, err := c.fetchInGrouped(ctx, base, base.Table(), base.PKColumn(), keys, "", includeArchived)
	if err != nil {
		return err
	}
	skip := map[string]bool{base.PKColumn(): true}
	if col, ok := base.SoftDeleteColumn(); ok {
		if _, roleHas := schema.SoftDeleteColumn(); roleHas {
			skip[col] = true
		}
	}
	if col := base.CreatedAtColumn(); col != "" && schema.CreatedAtColumn() != "" {
		skip[col] = true
	}
	if col := base.UpdatedAtColumn(); col != "" && schema.UpdatedAtColumn() != "" {
		skip[col] = true
	}
	for _, doc := range docs {
		rows := grouped[keyOf(doc, fkCol)]
		if len(rows) == 0 {
			continue
		}
		// Same watermark discipline as the per-row mergeSharedBase: the base's
		// physical revision column becomes the _base_revision watermark, never a
		// document field (idempotent — the grouped row is shared across docs).
		remapRevision(rows[0], base, docBaseRevisionField)
		for col, val := range rows[0] {
			if skip[col] {
				continue
			}
			doc[col] = val
		}
	}
	return nil
}

// mergeSharedBaseChildrenBatch is the set-based mergeSharedBaseChildren: each base
// child collection is fetched for the whole batch by its FK to the base id (the
// role's FK already on the doc), grouped, then nested under its Go segment. A doc
// without the base FK gets no base-child segments (as in the per-row path).
func (c *Composer) mergeSharedBaseChildrenBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	keys := collectKeys(docs, fkCol)
	for _, bc := range baseChildren {
		sd, _ := schemaSoftDelete(bc)
		grouped, err := c.fetchInGrouped(ctx, bc, bc.Table(), bc.FKColumn(), keys, sd, includeArchived)
		if err != nil {
			return err
		}
		seg := childDocSegment(bc)
		for _, doc := range docs {
			if keyOf(doc, fkCol) == "" {
				continue
			}
			doc[seg] = emptyIfNil(grouped[keyOf(doc, fkCol)])
		}
	}
	return nil
}

// mergeOwnChildrenBatch is the set-based mergeOwnChildren: each own child
// collection is fetched for the whole batch by root.PK → child.FK, grouped, then
// nested under its Go segment. Every fetched child row also gets its OWN siblings
// merged flat — batched across ALL child rows of the batch (one query per
// child-sibling table), not per child row.
func (c *Composer) mergeOwnChildrenBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	children := schema.ChildSchemas()
	if len(children) == 0 || len(docs) == 0 {
		return nil
	}
	pkCol := schemaPK(schema)
	keys := collectKeys(docs, pkCol)
	for _, child := range children {
		sd, _ := schemaSoftDelete(child)
		grouped, err := c.fetchInGrouped(ctx, child, child.Table(), child.FKColumn(), keys, sd, includeArchived)
		if err != nil {
			return err
		}
		// The child rows carry their own siblings (shape #4). They are the same
		// Document objects held in `grouped`, so merging over the flattened set
		// mutates the nested arrays in place.
		var allChild []Document
		for _, rows := range grouped {
			allChild = append(allChild, rows...)
		}
		if err := c.mergeOwnerSiblingsBatch(ctx, allChild, child, includeArchived); err != nil {
			return err
		}
		seg := childDocSegment(child)
		for _, doc := range docs {
			if keyOf(doc, pkCol) == "" {
				continue
			}
			doc[seg] = emptyIfNil(grouped[keyOf(doc, pkCol)])
		}
	}
	return nil
}

// composeBaseRootedRowsBatched is the set-based composeBaseRootedRow: for a batch
// of SharedBaseView base rows it nests the base's own children, then fills ONE
// sub-document per role. The role row per base is chosen active-first exactly as
// fetchRoleRow does — the batch fetches every active role row in one IN lookup and
// falls back to the per-base latest-archived remnant ONLY for bases with no active
// row (the rare archived-only identity), so the common case is fully set-based.
func (c *Composer) composeBaseRootedRowsBatched(ctx context.Context, view *ViewDefinition, rows []Document, includeArchived bool) error {
	base := view.schema
	basePK := schemaPK(base)
	if err := c.mergeOwnChildrenBatch(ctx, rows, base, includeArchived); err != nil {
		return err
	}
	for _, r := range view.roles {
		_, fkCol, _ := r.schema.SharedBaseRef()
		sd, hasSD := schemaSoftDelete(r.schema)
		baseIDs := collectKeys(rows, basePK)

		// Active role rows (or, without soft-delete, simply the row) for the whole
		// batch in one lookup, grouped by the base FK.
		var grouped map[string][]Document
		var err error
		if !hasSD {
			grouped, err = c.fetchInGrouped(ctx, r.schema, r.schema.Table(), fkCol, baseIDs, "", true)
		} else {
			grouped, err = c.fetchInGrouped(ctx, r.schema, r.schema.Table(), fkCol, baseIDs, sd, false)
		}
		if err != nil {
			return err
		}

		roleByBase := make(map[string]Document, len(rows))
		chosen := make([]Document, 0, len(rows))
		for _, row := range rows {
			bid := keyOf(row, basePK)
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
				bid := keyOf(row, basePK)
				if bid == "" || roleByBase[bid] != nil {
					continue
				}
				arch, err := c.fetchLatestArchived(ctx, r.schema, fkCol, bid, sd)
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
		if err := c.mergeOwnerSiblingsBatch(ctx, chosen, r.schema, includeArchived); err != nil {
			return err
		}
		if err := c.mergeOwnChildrenBatch(ctx, chosen, r.schema, includeArchived); err != nil {
			return err
		}

		// Attach the sub-document (an absent role writes an explicit nil segment so
		// the $set upsert overwrites a vanished role rather than leaving it stale).
		// The role row's physical revision column becomes the segment's _revision
		// watermark — same discipline as the per-row composeBaseRootedRow.
		for _, row := range rows {
			if rr := roleByBase[keyOf(row, basePK)]; rr != nil {
				remapRevision(rr, r.schema, docRevisionField)
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
//   - 1:1: a parent with a nil/absent FK, or no matching embed doc, leaves the
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
		coll := c.resolver.Active(e.source.table)
		if e.many {
			// 1:N — embed.JoinColumn == parent.PK.
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
			for _, doc := range docs {
				v, ok := doc[parentPK]
				if !ok || v == nil {
					continue
				}
				doc[e.field] = grouped[fmt.Sprintf("%v", v)]
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
			for _, doc := range docs {
				// Unresolved 1:1 → explicit null, same reason as the per-row
				// path: $set-merged writes would keep a stale sub-document if
				// the key were omitted.
				v, ok := doc[e.JoinColumn()]
				if !ok || v == nil {
					doc[e.field] = nil
					continue
				}
				rows := grouped[fmt.Sprintf("%v", v)]
				if len(rows) == 0 {
					doc[e.field] = nil
					continue
				}
				doc[e.field] = rows[0]
			}
		}
	}
	return nil
}
