package write

import (
	"context"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The outbox payload — the EVENT-CARRIED-STATE contract of the framework.
// One shape for every verb with a body, produced by buildWritePayload:
//
//   - every scalar the aggregate owns lands COLUMN-KEYED at the TOP level:
//     the root/role fields ∪ the sibling fields ∪ the shared-base business
//     fields ∪ the verb's managed timestamps (the operation's writeNow() stamp
//     — the exact values bound into the DML);
//   - "_ids" carries the structural identity: the aggregate PK, and — for a
//     SharedBase role — the deterministic base id plus the base's REVISION
//     (the row-lock-serialized last-writer-wins token) and the purge flag;
//   - "_children" / "_base_children" carry the aggregate's child collections,
//     grouped by Go type name, each item column-keyed with an "_op" verb
//     (insert / update / archive / delete / noop) resolved from the SAME
//     OperationOf categorization the persister executed — so a consumer can
//     apply children surgically, and a loaded-but-untouched child (noop)
//     never needs data it does not carry.
//
// Keys starting with "_" are the framework's reserved namespace (a physical
// column may never claim them — enforced at TableSchema declaration).
//
// Timestamps by verb: INSERTED carries created_at + updated_at; UPDATED only
// updated_at (an absent key is untouched by the consumer's $set, so the
// document keeps its original created_at); ARCHIVED carries the soft-delete
// stamp; UNARCHIVED an explicit null. DELETED does not come through here —
// buildDeletePayload keeps the historical structural keys and only ADDS.
const (
	payloadKeyIDs          = "_ids"
	payloadKeyChildren     = "_children"
	payloadKeyBaseChildren = "_base_children"
	payloadKeyOp           = "_op"

	payloadIDsID           = "id"
	payloadIDsRevision     = "revision"
	payloadIDsBaseID       = "base_id"
	payloadIDsBaseRevision = "base_revision"
	payloadIDsBasePurged   = "base_purged"
)

// outboxMeta is the structural identity block of one outbox payload.
type outboxMeta struct {
	ID           string
	Revision     int64  // the aggregate row's own commit-order token (0 = row gone)
	BaseID       string // "" when the schema has no shared base
	BaseRevision int64  // valid when BaseID != "" and the base row still exists
	BasePurged   bool   // DELETED only: the hard-delete purged the identity
}

// idsBlock renders the "_ids" map of a payload.
func (m outboxMeta) idsBlock() map[string]any {
	ids := map[string]any{payloadIDsID: m.ID}
	if m.Revision > 0 {
		ids[payloadIDsRevision] = m.Revision
	}
	if m.BaseID != "" {
		ids[payloadIDsBaseID] = m.BaseID
		ids[payloadIDsBaseRevision] = m.BaseRevision
	}
	if m.BasePurged {
		ids[payloadIDsBasePurged] = true
	}
	return ids
}

// buildWritePayload assembles the payload for the body verbs
// (INSERTED / UPDATED / ARCHIVED / UNARCHIVED). rootFields is the verb's bound
// column→value map for the root/role table; src is the entity value the
// sibling and shared-base fields read from; root is the aggregate (nil for a
// flat entity); now is the operation stamp the DML bound.
func buildWritePayload(
	schema *TableSchema,
	src domain.Entity,
	root *domain.AggregateRoot,
	eventType string,
	now time.Time,
	rootFields domain.Fields,
	meta outboxMeta,
) map[string]any {
	out := make(map[string]any, len(rootFields)+8)
	for k, v := range rootFields {
		out[k] = v
	}
	// Siblings — flat at the top, ALWAYS present. An all-nil facet still emits
	// its columns as explicit nulls: under event-carried state the consumer
	// cannot distinguish "sibling cleared by this write" from "sibling
	// untouched" if the keys are simply absent — a PUT that removed the 1:1
	// sibling row would leave the projected document carrying the stale values
	// forever. The projector recognizes the all-null group as the removed row
	// and DROPS the keys (shape parity with the composer, which omits a
	// missing sibling row).
	for _, sib := range schema.Siblings() {
		for k, v := range sib.WriteFields(src) {
			out[k] = v
		}
	}
	// Shared-base business fields — flat at the top (managed base columns do
	// not travel; the consumer's precedence rules own them), plus the role's
	// separate FK column so the payload is self-sufficient on every verb.
	if base, fkCol, ok := schema.SharedBaseRef(); ok && meta.BaseID != "" {
		bf, _ := sharedBaseValues(base, src)
		for k, v := range bf {
			out[k] = v
		}
		if fkCol != schema.PKColumn() {
			out[fkCol] = domain.NewID(meta.BaseID)
		}
	}
	// Managed timestamps — the exact values the DML bound this operation.
	switch eventType {
	case "INSERTED":
		if c := schema.CreatedAtColumn(); c != "" {
			out[c] = now
		}
		if u := schema.UpdatedAtColumn(); u != "" {
			out[u] = now
		}
		// The composed document ALWAYS carries the soft-delete key (SELECT *
		// includes the NULL column); the projected document must match shape,
		// so a fresh row travels with an explicit null.
		if sd, ok := schema.SoftDeleteColumn(); ok {
			out[sd] = nil
		}
	case "UPDATED":
		if u := schema.UpdatedAtColumn(); u != "" {
			out[u] = now
		}
	case "ARCHIVED":
		if sd, ok := schema.SoftDeleteColumn(); ok {
			out[sd] = now
		}
	case "UNARCHIVED":
		if sd, ok := schema.SoftDeleteColumn(); ok {
			out[sd] = nil
		}
	}
	out[payloadKeyIDs] = meta.idsBlock()
	appendChildrenBlocks(out, schema, root, eventType, now)
	return out
}

// appendChildrenBlocks fills "_children" / "_base_children" from the aggregate
// map: every non-absent item, column-keyed, with its "_op" verb. On the soft
// verbs every item is "noop" — the cascade is implied by the ROOT verb, not by
// per-child operations.
func appendChildrenBlocks(out map[string]any, schema *TableSchema, root *domain.AggregateRoot, eventType string, now time.Time) {
	if root == nil {
		return
	}
	own := map[string]any{}
	fromBaseCh := map[string]any{}
	soft := eventType == "ARCHIVED" || eventType == "UNARCHIVED"
	for typeName, items := range root.AllAggregateItems() {
		child, fromBase, ok := schema.ResolveAggregateChild(typeName)
		if !ok {
			continue // an undeclared child already failed the write itself
		}
		list := make([]map[string]any, 0, len(items))
		for _, it := range items {
			op := childOpName(domain.OperationOf(it.OriginalStatus, it.CurrentStatus), soft, fromBase, child)
			item := map[string]any{payloadKeyOp: op}
			if op != "archive" && op != "delete" {
				for k, v := range child.WriteFields(it.Item) {
					item[k] = v
				}
				// The child's SIBLING fields merge FLAT into the item (shape #4),
				// exactly like the composed document renders them — an all-nil
				// sibling slice stays absent, mirroring the write itself.
				for _, sib := range child.Siblings() {
					sf := sib.WriteFields(it.Item)
					if allNilFields(sf) {
						continue
					}
					for k, v := range sf {
						item[k] = v
					}
				}
			}
			// An archive op carries the exact stamp the child UPDATE bound, so
			// the surgical read-side edit lands the same value.
			if op == "archive" {
				if sd, ok := child.SoftDeleteColumn(); ok {
					item[sd] = now
				}
			}
			if id := it.Item.GetID().Value(); id != "" {
				item[child.PKColumn()] = domain.NewID(id)
			}
			list = append(list, item)
		}
		if len(list) == 0 {
			continue
		}
		if fromBase {
			fromBaseCh[typeName] = list
		} else {
			own[typeName] = list
		}
	}
	if len(own) > 0 {
		out[payloadKeyChildren] = own
	}
	if len(fromBaseCh) > 0 {
		out[payloadKeyBaseChildren] = fromBaseCh
	}
}

// childOpName maps the persister's OperationOf categorization to the payload's
// "_op" verb. A Removed child mirrors removeChild's actual effect: hard-delete
// for a base-child without soft-delete, archive otherwise.
func childOpName(op domain.AggregateItemOp, softVerb, fromBase bool, child *TableSchema) string {
	if softVerb {
		return "noop"
	}
	switch op {
	case domain.OpInsert:
		return "insert"
	case domain.OpUpdate:
		return "update"
	case domain.OpDelete:
		if fromBase {
			if _, ok := child.SoftDeleteColumn(); !ok {
				return "delete"
			}
		}
		return "archive"
	default:
		return "noop"
	}
}

// buildDeletePayload assembles the DELETED payload: the historical
// structural keys (the PK under its physical column name + the shared-base FK
// — consumers depend on them, they only GROW) plus the "_ids" block with the
// purge flag.
func buildDeletePayload(schema *TableSchema, src domain.Entity, id string, meta outboxMeta) domain.Fields {
	keys := deleteKeysPayload(schema, src, id)
	keys[payloadKeyIDs] = meta.idsBlock()
	return keys
}

// readRevision reads a row's commit-order token inside the write TX — after
// the row's own statements ran, so the payload stamps the value THIS
// operation's lock scope produced. A vanished row answers 0.
func readRevision(ctx context.Context, tx WriteTx, d Dialect, table, revCol, pkCol, id string) (int64, error) {
	q := d.ApplyLimit("SELECT "+d.QuoteIdent(revCol)+" FROM "+d.QuoteIdent(table)+
		" WHERE "+d.QuoteIdent(pkCol)+" = "+d.Placeholder(1), 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(id)))
	if err != nil || rows == nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var rev int64
	if err := rows.Scan(&rev); err != nil {
		return 0, err
	}
	return rev, rows.Err()
}

// outboxMetaFor resolves the outbox structural identity (_ids) for any verb
// that did not compute it inline: the row's OWN revision (read in-TX, after
// this operation's statements) and — only when the schema declares a shared
// base — the deterministic base id + the base's revision. For a plain flat or
// aggregate entity it is just the own-revision read. A verb that already
// KNOWS the own token (an INSERT: a row born in this TX is 1 by definition)
// skips this and calls fillBaseMeta directly — no wasted read.
func outboxMetaFor(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, id string) (outboxMeta, error) {
	meta := outboxMeta{ID: id}
	if rc := schema.RevisionColumn(); rc != "" {
		rev, err := readRevision(ctx, tx, d, schema.Table(), rc, schema.PKColumn(), id)
		if err != nil {
			return meta, err
		}
		meta.Revision = rev
	}
	if err := fillBaseMeta(ctx, tx, d, schema, src, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// fillBaseMeta resolves the shared-base half of the structural identity —
// the deterministic base id + the base's revision. Before reading, it ADVANCES
// the base revision (bumpBaseRevision): every caller of this helper is a role
// verb that did NOT otherwise write the base row (role archive/unarchive
// without a base transition, batch role ops, a batch role insert), and the
// base revision must move on EVERY identity-touching write so the read side
// can order compositions of the identity's closure. The verbs that upsert the
// base themselves (insertWithBase/updateWithBase) bump inside that statement
// and never come through here. A no-op when the schema declares no shared base
// or the natural key is unreadable (payload assembly never vetoes a write the
// verb allows).
func fillBaseMeta(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, meta *outboxMeta) error {
	base, _, ok := schema.SharedBaseRef()
	if !ok {
		return nil
	}
	_, nk := sharedBaseValues(base, src)
	if nk == "" {
		return nil
	}
	meta.BaseID = deterministicBaseID(nk)
	if err := bumpBaseRevision(ctx, tx, d, base, meta.BaseID); err != nil {
		return err
	}
	rev, err := readBaseRevision(ctx, tx, d, base, meta.BaseID)
	if err != nil {
		return err
	}
	meta.BaseRevision = rev
	return nil
}
