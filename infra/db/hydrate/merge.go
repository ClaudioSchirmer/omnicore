package hydrate

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// merge.go holds the per-row merge chain: one query per related source, for one
// root. Correct and simple, and the right shape when there is exactly one root to
// hydrate. batch.go carries the set-based companions for a whole batch of roots,
// producing the identical per-document result.

// MergeOwnerSiblings merges each declared sibling's columns FLAT into the owner
// doc, fetched by the shared primary key. The document stays a flat mirror of the
// entity (siblings land at the owner's level, not nested) — the read-side
// reflection of how the write side partitioned the row. An absent sibling row
// leaves its fields omitted (never forced empty). Siblings carry no DeletedAt
// (the owner's gate governs the row's visibility), so the sibling fetch passes an
// empty DeletedAt column — no per-sibling filter. The shared ID column is already
// on the owner doc, so it is not re-copied. CoerceTypes (inside FetchRow) restores
// bool fidelity on the sibling's own columns.
func (h *Hydrator) MergeOwnerSiblings(ctx context.Context, doc Document, ownerSchema *core.TableSchema, pkVal string, includeArchived bool) error {
	pkCol := SchemaPK(ownerSchema)
	for _, sib := range ownerSchema.Siblings() {
		row, err := h.FetchRow(ctx, sib, sib.Table(), pkCol, pkVal, "", includeArchived)
		if err != nil {
			return err
		}
		if row == nil {
			continue
		}
		for col, val := range row {
			if col == pkCol {
				continue
			}
			doc[col] = val
		}
	}
	return nil
}

// MergeSharedBaseChildren nests the shared base's NATIVE children (base-children)
// into the role document — the person-native collections (e.g. a person's
// addresses) shared across every role. They are fetched by the base-child's
// ParentID to the base id, which is the role's ParentID to the base already on the
// doc. Each collection lands under its declared collection segment. No-op without
// a shared base or base children.
func (h *Hydrator) MergeSharedBaseChildren(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	baseChildren := base.ChildSchemas()
	if len(baseChildren) == 0 {
		return nil
	}
	baseID, present := doc[fkCol]
	if !present || baseID == nil {
		return nil
	}
	idStr := fmt.Sprintf("%v", baseID)
	for _, bc := range baseChildren {
		sd, _ := SchemaDeletedAt(bc)
		rows, err := h.FetchWhere(ctx, bc, bc.Table(), bc.ParentIDColumn(), idStr, sd, includeArchived)
		if err != nil {
			return err
		}
		doc[bc.CollectionSegment()] = rows
	}
	return nil
}

// MergeOwnChildren nests the schema's OWN aggregate children (schema.Child(...))
// into the document — the read-side mirror of the write side's child hydration.
// Unlike base-children (keyed on the base's deterministic id via
// MergeSharedBaseChildren), an own child is joined on root.ID → child.ParentID:
// the child's ParentID column matched against the ID value already on the doc.
// Each collection lands under its declared collection segment. Every fetched
// child row also gets its siblings merged FLAT (the child-sibling merge). No-op
// when the schema declares no own children.
func (h *Hydrator) MergeOwnChildren(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	children := schema.ChildSchemas()
	if len(children) == 0 {
		return nil
	}
	pkVal, present := doc[SchemaPK(schema)]
	if !present || pkVal == nil {
		return nil
	}
	idStr := fmt.Sprintf("%v", pkVal)
	for _, child := range children {
		sd, _ := SchemaDeletedAt(child)
		rows, err := h.FetchWhere(ctx, child, child.Table(), child.ParentIDColumn(), idStr, sd, includeArchived)
		if err != nil {
			return err
		}
		// The child rows carry their own siblings. Merge them SET-BASED across all
		// rows of this child collection — one IN (...) query per child-sibling
		// table instead of one per child row (the batch helper produces the
		// identical flat merge the per-row path did).
		if err := h.MergeOwnerSiblingsBatch(ctx, rows, child, includeArchived); err != nil {
			return err
		}
		doc[child.CollectionSegment()] = rows
	}
	return nil
}

// MergeSharedBase merges a role's shared identity (SharedBase) FLAT into the role
// document, fetched by the role's ParentID to the base's deterministic id. Like a
// sibling, the base fields land at the role's level (the doc stays flat). The base
// ID column equals the ParentID value already on the doc, so it is not re-copied.
//
// The base's MANAGED columns (DeletedAt, created_at, updated_at) never overwrite
// the role's own: the document represents the ROLE, whose lifecycle and timestamps
// are authoritative (the base's are derived — it converges from its roles).
// Without this guard, a two-role identity with ONE archived role would compose the
// ACTIVE base's NULL deleted_at over the role's archived timestamp, hiding the
// archival from the reader's DeletedAt gate, and every role doc would carry the
// person's creation timestamps instead of its own. Each managed column of the base
// is skipped only when the role declares its own column of the same kind.
func (h *Hydrator) MergeSharedBase(ctx context.Context, doc Document, schema *core.TableSchema, includeArchived bool) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	fk, present := doc[fkCol]
	if !present || fk == nil {
		return nil
	}
	row, err := h.FetchRow(ctx, base, base.Table(), base.IDColumn(), fmt.Sprintf("%v", fk), "", includeArchived)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	RemapRevision(row, base, BaseRevisionField)
	skip := sharedBaseSkipSet(base, schema)
	for col, val := range row {
		if skip[col] {
			continue
		}
		doc[col] = val
	}
	return nil
}

// sharedBaseSkipSet names the base columns that must NOT land on the role doc:
// the shared id (already there) plus each managed column the ROLE declares for
// itself. Shared by the per-row and batched shared-base merges so the two cannot
// drift.
func sharedBaseSkipSet(base, role *core.TableSchema) map[string]bool {
	skip := map[string]bool{base.IDColumn(): true}
	if col, ok := base.DeletedAtColumn(); ok {
		if _, roleHas := role.DeletedAtColumn(); roleHas {
			skip[col] = true
		}
	}
	if col := base.CreatedAtColumn(); col != "" && role.CreatedAtColumn() != "" {
		skip[col] = true
	}
	if col := base.UpdatedAtColumn(); col != "" && role.UpdatedAtColumn() != "" {
		skip[col] = true
	}
	return skip
}
