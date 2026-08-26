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
//   - "_ids" carries the structural identity: the aggregate ID, and — for a
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
// document keeps its original created_at); ARCHIVED carries the DeletedAt
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
	payloadIDsCreatedAt    = "created_at"
)

// outboxMeta is the structural identity block of one outbox payload.
type outboxMeta struct {
	ID           string
	Revision     int64  // the aggregate row's own commit-order token (0 = row gone)
	BaseID       string // "" when the schema has no shared base
	BaseRevision int64  // valid when BaseID != "" and the base row still exists
	BasePurged   bool   // DELETED only: the hard-delete purged the identity
	// CreatedAt is the row's created_at — the incarnation discriminator of this
	// incarnation of the id. A DETERMINISTIC id (a shared-ID role, the base)
	// can be REBORN: delete the natural key, re-create it, and the same id
	// returns with its revision restarted at 1 — so the read side's document
	// tombstone (recorded at the delete's revision) would treat every write of
	// the new life as a zombie of the old one. The created_at tells the incarnations
	// apart: the tombstone kills only documents whose stored created_at equals
	// the DEAD row's created_at instant. Zero when the schema declares no
	// CreatedAt column (the tombstone then falls back to revision-only —
	// document that trade-off, never widen it silently).
	CreatedAt time.Time
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
	if !m.CreatedAt.IsZero() {
		ids[payloadIDsCreatedAt] = m.CreatedAt
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
	// Redaction of the InSync axis happens HERE, on the copy — never on
	// rootFields, which is the very map the INSERT/UPDATE binds (see
	// aggregate_write.go: one WriteFields call feeds both the DML and this
	// payload). Applying it upstream would write the mask to the column that
	// exists to hold the real value. Every segment below redacts through its OWN
	// schema, because a sibling, a child and a shared base each declare their own
	// fields.
	schema.RedactSyncColumns(out)
	// Siblings — flat at the top, ALWAYS present. An all-nil facet still emits
	// its columns as explicit nulls: under event-carried state the consumer
	// cannot distinguish "sibling cleared by this write" from "sibling
	// untouched" if the keys are simply absent — a PUT that removed the 1:1
	// sibling row would leave the projected document carrying the stale values
	// forever. The projector recognizes the all-null group as the removed row
	// and DROPS the keys (shape parity with the composer, which omits a
	// missing sibling row).
	for _, sib := range schema.Siblings() {
		sf := sib.WriteFields(src)
		sib.RedactSyncColumns(sf)
		for k, v := range sf {
			out[k] = v
		}
	}
	// Shared-base business fields — flat at the top (managed base columns do
	// not travel; the consumer's precedence rules own them), plus the role's
	// separate ParentID column so the payload is self-sufficient on every verb.
	if base, fkCol, ok := schema.SharedBaseRef(); ok && meta.BaseID != "" {
		bf, _ := sharedBaseValues(base, src)
		base.RedactSyncColumns(bf)
		for k, v := range bf {
			out[k] = v
		}
		if fkCol != schema.IDColumn() {
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
		// The composed document ALWAYS carries the DeletedAt key (SELECT *
		// includes the NULL column); the projected document must match shape,
		// so a fresh row travels with an explicit null.
		if sd, ok := schema.DeletedAtColumn(); ok {
			out[sd] = nil
		}
	case "UPDATED":
		if u := schema.UpdatedAtColumn(); u != "" {
			out[u] = now
		}
	case "ARCHIVED":
		if sd, ok := schema.DeletedAtColumn(); ok {
			out[sd] = now
		}
	case "UNARCHIVED":
		if sd, ok := schema.DeletedAtColumn(); ok {
			out[sd] = nil
		}
	}
	out[payloadKeyIDs] = meta.idsBlock()
	appendChildrenBlocks(out, schema, root, eventType, now)
	return out
}

// appendChildrenBlocks fills "_children" / "_base_children" from the aggregate
// map: every non-absent item, column-keyed, with its "_op" verb.
//
// On the soft verbs the item's op is the CASCADE the root statement performed —
// "archive" carrying the stamp the child UPDATE bound, "unarchive" carrying an
// explicit null. It used to be "noop", which the read side skips: the relational
// child rows flipped their DeletedAt and the projected array never heard about
// it, so a live document and one rebuilt from the source disagreed about which
// children were archived. A child table without a DeletedAt column takes no
// cascade and stays "noop".
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
		_, childHasDeletedAt := child.DeletedAtColumn()
		for _, it := range items {
			op := childOpName(domain.OperationOf(it.OriginalStatus, it.CurrentStatus), soft, eventType, fromBase, child, childHasDeletedAt)
			item := map[string]any{payloadKeyOp: op}
			if op != "archive" && op != "unarchive" && op != "delete" {
				cf := child.WriteFields(it.Item)
				child.RedactSyncColumns(cf)
				for k, v := range cf {
					item[k] = v
				}
				// The child's SIBLING fields merge FLAT into the item (shape #4),
				// exactly like the composed document renders them — an all-nil
				// sibling slice stays absent, mirroring the write itself.
				for _, sib := range child.Siblings() {
					sf := sib.WriteFields(it.Item)
					// The all-nil test decides PRESENCE, so it must read the real
					// values: redaction runs only after the row is known to exist
					// (and never turns a non-nil value into nil, so the two orders
					// agree — the order here is the one that stays correct if a
					// future redactor is added).
					if allNilFields(sf) {
						continue
					}
					sib.RedactSyncColumns(sf)
					for k, v := range sf {
						item[k] = v
					}
				}
			}
			// An archive op carries the exact stamp the child UPDATE bound, so
			// the surgical read-side edit lands the same value; unarchive carries
			// the explicit null the cascade wrote.
			if sd, ok := child.DeletedAtColumn(); ok {
				switch op {
				case "archive":
					item[sd] = now
				case "unarchive":
					item[sd] = nil
				}
			}
			if id := it.Item.GetID().Value(); id != "" {
				item[child.IDColumn()] = domain.NewID(id)
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
// for a base-child without DeletedAt, archive otherwise.
//
// On a soft verb the item's own status is irrelevant: the root's cascade hit
// EVERY child row under the ParentID with one statement, so every item reports
// that same transition — unless its table has no DeletedAt column, which the
// cascade skips.
func childOpName(op domain.AggregateItemOp, softVerb bool, eventType string, fromBase bool, child *TableSchema, childHasDeletedAt bool) string {
	if softVerb {
		if !childHasDeletedAt {
			return "noop"
		}
		if eventType == "ARCHIVED" {
			return "archive"
		}
		return "unarchive"
	}
	switch op {
	case domain.OpInsert:
		return "insert"
	case domain.OpUpdate:
		return "update"
	case domain.OpDelete:
		if fromBase {
			if _, ok := child.DeletedAtColumn(); !ok {
				return "delete"
			}
		}
		return "archive"
	default:
		return "noop"
	}
}

// buildDeletePayload assembles the DELETED payload: the historical
// structural keys (the ID under its physical column name + the shared-base ParentID
// — consumers depend on them, they only GROW) plus the "_ids" block with the
// purge flag.
func buildDeletePayload(schema *TableSchema, src domain.Entity, id string, meta outboxMeta) domain.Fields {
	keys := deleteKeysPayload(schema, src, id)
	keys[payloadKeyIDs] = meta.idsBlock()
	return keys
}

// insertCreatedAt answers the created_at instant an INSERT stamps on its payload: the
// operation's own writeNow() value IS the row's created_at, so no read-back is
// needed. Zero when the schema declares no CreatedAt column (no discriminator
// to carry).
func insertCreatedAt(schema *TableSchema, now time.Time) time.Time {
	if schema.CreatedAtColumn() == "" {
		return time.Time{}
	}
	return now
}

// readRevision reads a row's commit-order token inside the write TX — after
// the row's own statements ran, so the payload stamps the value THIS
// operation's lock scope produced. A vanished row answers 0.
func readRevision(ctx context.Context, tx WriteTx, d Dialect, table, revCol, pkCol, id string) (int64, error) {
	rev, _, err := readRevisionCreatedAt(ctx, tx, d, table, revCol, "", pkCol, id)
	return rev, err
}

// readRevisionCreatedAt reads the row's commit-order token AND — when createdCol is
// declared — its created_at created_at instant, in ONE statement. The created_at instant
// is the incarnation discriminator the document tombstone needs (see
// outboxMeta.CreatedAt). A vanished row answers (0, zero). The timestamp scan is
// dialect-tolerant: drivers answer time.Time, string or []byte depending on
// engine and connection options.
func readRevisionCreatedAt(ctx context.Context, tx WriteTx, d Dialect, table, revCol, createdCol, pkCol, id string) (int64, time.Time, error) {
	cols := d.QuoteIdent(revCol)
	if createdCol != "" {
		cols += ", " + d.QuoteIdent(createdCol)
	}
	q := d.ApplyLimit("SELECT "+cols+" FROM "+d.QuoteIdent(table)+
		" WHERE "+d.QuoteIdent(pkCol)+" = "+d.Placeholder(1), 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(id)))
	if err != nil || rows == nil {
		return 0, time.Time{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, time.Time{}, rows.Err()
	}
	var rev int64
	if createdCol == "" {
		if err := rows.Scan(&rev); err != nil {
			return 0, time.Time{}, err
		}
		return rev, time.Time{}, rows.Err()
	}
	var rawCreatedAt any
	if err := rows.Scan(&rev, &rawCreatedAt); err != nil {
		return 0, time.Time{}, err
	}
	return rev, normalizeCreatedAt(rawCreatedAt), rows.Err()
}

// normalizeCreatedAt coerces a scanned created_at into a UTC time.Time. The write
// path binds UTC values, so a naive string form (MySQL DATETIME without
// parseTime) parses as UTC. An unrecognized form degrades to zero — the
// tombstone then falls back to revision-only, never a wrong discriminator.
func normalizeCreatedAt(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t.UTC()
	case string:
		return parseCreatedAt(t)
	case []byte:
		return parseCreatedAt(string(t))
	default:
		return time.Time{}
	}
}

func parseCreatedAt(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
		// The created_at instant rides the SAME statement as the revision read —
		// zero extra round trips for the tombstone's incarnation discriminator.
		rev, createdAt, err := readRevisionCreatedAt(ctx, tx, d, schema.Table(), rc, schema.CreatedAtColumn(), schema.IDColumn(), id)
		if err != nil {
			return meta, err
		}
		meta.Revision = rev
		meta.CreatedAt = createdAt
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
