package hydrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// batch.go holds the SET-BASED companions of the per-row merge chain in merge.go.
// The per-row helpers fetch each related source with one query per aggregate —
// fine for one document, an N+1 storm across millions. The batched helpers below
// fetch every related row for a WHOLE batch of roots in one IN (...) query per
// related table (chunked at MaxInClauseSize) and group them in memory, so a
// caller pays one round trip per table per batch instead of one per aggregate.
// This is round-trip-bound work, so the win is largest on the engines with the
// heaviest per-query cost.
//
// Correctness rests on one property: the group key is fmt("%v", row[col]) taken on
// BOTH sides — the parent's ID/ParentID value AND the child's ParentID value. Both
// are the same id-typed column read through the same core.Querier.QueryMaps
// surface, so equal ids stringify equally regardless of the backend's physical id
// encoding; and each key is re-encoded through the exact EncodeKey the single-row
// path uses, so the IN predicate matches the stored column on every dialect.

// FetchInGrouped fetches every row whose keyCol is in keys (deduped + chunked at
// MaxInClauseSize, same DeletedAt gate as FetchWhere) and groups them by the
// stringified keyCol value — the set-based companion of FetchWhere/FetchRow.
func (h *Hydrator) FetchInGrouped(ctx context.Context, schema *core.TableSchema, table, keyCol string, keys []string, sdCol string, includeArchived bool) (map[string][]Document, error) {
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
	d := h.eng.Dialect()
	cond := ""
	if !includeArchived && sdCol != "" {
		cond = " AND " + d.QuoteIdent(sdCol) + " IS NULL"
	}
	for start := 0; start < len(uniq); start += MaxInClauseSize {
		end := start + MaxInClauseSize
		if end > len(uniq) {
			end = len(uniq)
		}
		chunk := uniq[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, k := range chunk {
			ph[i] = d.Placeholder(i + 1)
			args[i] = h.EncodeKey(k)
		}
		sql := fmt.Sprintf("SELECT %s FROM %s WHERE %s IN (%s)%s",
			selectList(d, readColsWithKey(schema, keyCol)), d.QuoteIdent(table), d.QuoteIdent(keyCol), strings.Join(ph, ", "), cond)
		results, err := h.eng.Querier().QueryMaps(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		for _, m := range results {
			row := Document(m)
			CoerceTypes(row, schema)
			k := fmt.Sprintf("%v", row[keyCol])
			out[k] = append(out[k], row)
		}
	}
	return out, nil
}

// MergeOwnerSiblingsBatch is the set-based MergeOwnerSiblings: each declared
// sibling is fetched for the whole batch by the shared ID, grouped, then merged
// FLAT into each owner doc (columns minus the shared ID). A sibling is 1:1 by ID,
// so at most one row per key — take the first.
func (h *Hydrator) MergeOwnerSiblingsBatch(ctx context.Context, docs []Document, ownerSchema *core.TableSchema, includeArchived bool) error {
	sibs := ownerSchema.Siblings()
	if len(sibs) == 0 || len(docs) == 0 {
		return nil
	}
	pkCol := SchemaPK(ownerSchema)
	keys := CollectKeys(docs, pkCol)
	for _, sib := range sibs {
		grouped, err := h.FetchInGrouped(ctx, sib, sib.Table(), pkCol, keys, "", includeArchived)
		if err != nil {
			return err
		}
		for _, doc := range docs {
			rows := grouped[KeyOf(doc, pkCol)]
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

// MergeSharedBaseBatch is the set-based MergeSharedBase: the shared base is
// fetched for the whole batch by its ID (the roles' ParentID values), then merged
// FLAT into each role doc — with the SAME managed-column skip as the per-row path
// (sharedBaseSkipSet) so a base's NULL DeletedAt / its timestamps never overwrite
// the role's authoritative ones.
func (h *Hydrator) MergeSharedBaseBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	keys := CollectKeys(docs, fkCol)
	grouped, err := h.FetchInGrouped(ctx, base, base.Table(), base.IDColumn(), keys, "", includeArchived)
	if err != nil {
		return err
	}
	skip := sharedBaseSkipSet(base, schema)
	for _, doc := range docs {
		rows := grouped[KeyOf(doc, fkCol)]
		if len(rows) == 0 {
			continue
		}
		// Same watermark discipline as the per-row MergeSharedBase: the base's
		// physical revision column becomes the base-revision watermark, never a
		// document field (idempotent — the grouped row is shared across docs).
		RemapRevision(rows[0], base, BaseRevisionField)
		for col, val := range rows[0] {
			if skip[col] {
				continue
			}
			doc[col] = val
		}
	}
	return nil
}

// MergeSharedBaseChildrenBatch is the set-based MergeSharedBaseChildren: each
// base child collection is fetched for the whole batch by its ParentID to the base
// id (the role's ParentID already on the doc), grouped, then nested under its
// collection segment. A doc without the base ParentID gets no base-child segments
// (as in the per-row path).
func (h *Hydrator) MergeSharedBaseChildrenBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	keys := CollectKeys(docs, fkCol)
	for _, bc := range baseChildren {
		sd, _ := SchemaDeletedAt(bc)
		grouped, err := h.FetchInGrouped(ctx, bc, bc.Table(), bc.ParentIDColumn(), keys, sd, includeArchived)
		if err != nil {
			return err
		}
		seg := bc.CollectionSegment()
		for _, doc := range docs {
			if KeyOf(doc, fkCol) == "" {
				continue
			}
			doc[seg] = EmptyIfNil(grouped[KeyOf(doc, fkCol)])
		}
	}
	return nil
}

// MergeOwnChildrenBatch is the set-based MergeOwnChildren: each own child
// collection is fetched for the whole batch by root.ID → child.ParentID, grouped,
// then nested under its collection segment. Every fetched child row also gets its
// OWN siblings merged flat — batched across ALL child rows of the batch (one query
// per child-sibling table), not per child row.
func (h *Hydrator) MergeOwnChildrenBatch(ctx context.Context, docs []Document, schema *core.TableSchema, includeArchived bool) error {
	children := schema.ChildSchemas()
	if len(children) == 0 || len(docs) == 0 {
		return nil
	}
	pkCol := SchemaPK(schema)
	keys := CollectKeys(docs, pkCol)
	for _, child := range children {
		sd, _ := SchemaDeletedAt(child)
		grouped, err := h.FetchInGrouped(ctx, child, child.Table(), child.ParentIDColumn(), keys, sd, includeArchived)
		if err != nil {
			return err
		}
		// The child rows carry their own siblings. They are the same Document
		// objects held in `grouped`, so merging over the flattened set mutates the
		// nested arrays in place.
		var allChild []Document
		for _, rows := range grouped {
			allChild = append(allChild, rows...)
		}
		if err := h.MergeOwnerSiblingsBatch(ctx, allChild, child, includeArchived); err != nil {
			return err
		}
		seg := child.CollectionSegment()
		for _, doc := range docs {
			if KeyOf(doc, pkCol) == "" {
				continue
			}
			doc[seg] = EmptyIfNil(grouped[KeyOf(doc, pkCol)])
		}
	}
	return nil
}
