package write

import (
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The v2 outbox payload — the EVENT-CARRIED-STATE contract of the framework.
// One shape for every verb with a body, produced by buildWritePayloadV2:
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
// buildDeletePayloadV2 keeps the historical structural keys and only ADDS.
const (
	payloadKeyIDs          = "_ids"
	payloadKeyChildren     = "_children"
	payloadKeyBaseChildren = "_base_children"
	payloadKeyOp           = "_op"

	payloadIDsID           = "id"
	payloadIDsBaseID       = "base_id"
	payloadIDsBaseRevision = "base_revision"
	payloadIDsBasePurged   = "base_purged"
)

// outboxMeta is the structural identity block of one outbox payload.
type outboxMeta struct {
	ID           string
	BaseID       string // "" when the schema has no shared base
	BaseRevision int64  // valid when BaseID != "" and the base row still exists
	BasePurged   bool   // DELETED only: the hard-delete purged the identity
}

// idsBlock renders the "_ids" map of a payload.
func (m outboxMeta) idsBlock() map[string]any {
	ids := map[string]any{payloadIDsID: m.ID}
	if m.BaseID != "" {
		ids[payloadIDsBaseID] = m.BaseID
		ids[payloadIDsBaseRevision] = m.BaseRevision
	}
	if m.BasePurged {
		ids[payloadIDsBasePurged] = true
	}
	return ids
}

// buildWritePayloadV2 assembles the v2 payload for the body verbs
// (INSERTED / UPDATED / ARCHIVED / UNARCHIVED). rootFields is the verb's bound
// column→value map for the root/role table; src is the entity value the
// sibling and shared-base fields read from; root is the aggregate (nil for a
// flat entity); now is the operation stamp the DML bound.
func buildWritePayloadV2(
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
	// Siblings — flat at the top, mirroring the document (absent slice omitted,
	// exactly like the write skips an all-nil sibling row).
	for _, sib := range schema.Siblings() {
		f := sib.WriteFields(src)
		if allNilFields(f) {
			continue
		}
		for k, v := range f {
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

// buildDeletePayloadV2 assembles the DELETED payload: the historical
// structural keys (the PK under its physical column name + the shared-base FK
// — consumers depend on them, they only GROW) plus the "_ids" block with the
// purge flag.
func buildDeletePayloadV2(schema *TableSchema, src domain.Entity, id string, meta outboxMeta) domain.Fields {
	keys := deleteKeysPayload(schema, src, id)
	keys[payloadKeyIDs] = meta.idsBlock()
	return keys
}
