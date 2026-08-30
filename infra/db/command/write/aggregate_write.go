package write

import (
	"context"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// The aggregate-aware write path, written once on BaseEngine. Guarantees
// (identical on every backend):
//   - one framework-owned TX for root + all children
//   - exactly one outbox row per call (granularity B) + one in-TX audit row
//   - the Go-generated id is the ParentID injected (dialect-encoded) before each child INSERT
//   - status iteration: Added→INSERT, Changed→UPDATE, Removed→Archive,
//     Constructor→no-op (update) / INSERT (insert)
//   - root archive cascades archive of active children; unarchive restores
//     archived children; hard delete cascades an explicit DELETE per declared
//     child table (by ParentID) before the root DELETE, in the same TX — the framework
//     owns the cascade in Go rather than depending on a database ON DELETE CASCADE
//   - A/D lifecycle hooks fire once per call.

func (b *BaseEngine) insertAggregate(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootFields := schema.WriteFields(src)
	id, err := newWriteID()
	if err != nil {
		return domain.WriteResult{}, err
	}
	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()
	// The operation's one instant, read through the transaction it stamps (see
	// BaseEngine.now / relational.clock).
	now, err := b.now(ctx, tx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	hctx := HookContext{Verb: "Insert", EntityType: entity.EntityName()}

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	nowCols, stamped, err := stampedCols(schema, src, schema.InsertNowColumns(), now)
	if err != nil {
		return domain.WriteResult{}, err
	}
	sql, args := buildInsert(d, schema.Table(), schema.IDColumn(), id, rootFields, nowCols, now, schema.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeChildren(ctx, tx, d, root, schema, id, "", now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id, now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id,
		buildWritePayload(schema, src, root, "INSERTED", now, CascadeStamps{}, withStamps(rootFields, stamped, now), outboxMeta{ID: id, Revision: 1, CreatedAt: insertCreatedAt(schema, now)})); err != nil {
		return domain.WriteResult{}, err
	}
	ab := b.BuildAudit(func() audit.AuditEvent {
		return BuildInsertEvent(ctx, entity, domain.NewID(id), schema, b.auditClaims)
	}, entity.Events())
	if err := b.WriteAuditRow(ctx, tx, ab.Ev); err != nil {
		return domain.WriteResult{}, err
	}
	if err := b.FireBeforeCommit(ctx, tx, src, domain.NewID(id), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	b.AfterCommit(ctx, ab)
	return domain.WriteResult{ID: domain.NewID(id), Fields: rootFields}, nil
}

func (b *BaseEngine) updateAggregate(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootFields := schema.WriteFields(src)
	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()
	// The operation's one instant, read through the transaction it stamps (see
	// BaseEngine.now / relational.clock).
	now, err := b.now(ctx, tx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	hctx := HookContext{Verb: "Update", EntityType: entity.EntityName()}

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	rev := loadedRevision(src)
	nowCols, stamped, err := stampedCols(schema, src, schema.UpdateNowColumns(), now)
	if err != nil {
		return domain.WriteResult{}, err
	}
	sql, args, err := buildUpdate(d, schemaTarget(schema), criteria.Eq(idGoField, entity.ID()), rootFields, nowCols, now, schema.RevisionColumn(), rev)
	if err != nil {
		return domain.WriteResult{}, err
	}
	if err := execExpectingRow(ctx, tx, d, sql, args, schema.Table(), entity.EntityName(), schema.IDColumn(), entity.ID().Value(), rev); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeChildren(ctx, tx, d, root, schema, entity.ID().Value(), "", now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID().Value(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	meta, err := outboxMetaFor(ctx, tx, d, schema, src, entity.ID().Value())
	if err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID().Value(),
		buildWritePayload(schema, src, root, "UPDATED", now, CascadeStamps{}, withStamps(rootFields, stamped, now), meta)); err != nil {
		return domain.WriteResult{}, err
	}
	ab := b.BuildAudit(func() audit.AuditEvent {
		return BuildUpdateEvent(ctx, entity, schema, b.auditClaims)
	}, entity.Events())
	if err := b.WriteAuditRow(ctx, tx, ab.Ev); err != nil {
		return domain.WriteResult{}, err
	}
	if err := b.FireBeforeCommit(ctx, tx, src, domain.NewID(entity.ID().Value()), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	b.AfterCommit(ctx, ab)
	return domain.WriteResult{ID: entity.ID(), Fields: rootFields}, nil
}

// deleteAggregate hard-deletes an aggregate root via the shared hardDelete path
// (children + siblings cleared explicitly in Go before the root, one TX).
func (b *BaseEngine) deleteAggregate(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error {
	return b.hardDelete(ctx, entity.Source(), entity.ID().Value(), schema, hook,
		HookContext{Verb: "Delete", EntityType: entity.EntityName()},
		func() audit.AuditEvent { return BuildDeleteEvent(ctx, entity, schema, b.auditClaims) },
		func(baseID string) audit.AuditEvent {
			return BuildSharedBasePurgeEvent(ctx, entity, schema, baseID, b.auditClaims)
		},
		entity.Events())
}

// hardDelete is the shared hard-delete orchestration for both the flat and the
// aggregate paths: in one framework-owned TX it deletes every declared child
// table (by ParentID) and every owner sibling table (by the shared ID) EXPLICITLY in
// Go, then the owner row, then outbox + in-TX audit + the A/D hooks. The
// framework owns the cascade rather than depending on a database ON DELETE
// CASCADE it cannot emit or validate. Children come from the schema's declared
// ChildSchemas() (not the loaded aggregate), so every child row is removed
// regardless of what the aggregate carried. A flat schema contributes neither
// children nor (typically) siblings, so this collapses to the single root
// DELETE — behavior-identical to the previous flat delete. A missing root is a
// silent no-op (delete is not row-count-checked), unchanged from before.
func (b *BaseEngine) hardDelete(
	ctx persistence.RequestContext,
	src domain.Entity,
	id string,
	schema *TableSchema,
	hook WriteHook,
	hctx HookContext,
	buildEvent func() audit.AuditEvent,
	buildPurgeEvent func(baseID string) audit.AuditEvent,
	evs []domain.DomainEvent,
) error {
	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()
	// The operation's one instant, read through the transaction it stamps.
	now, err := b.now(ctx, tx)
	if err != nil {
		return err
	}

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}
	for _, child := range schema.ChildSchemas() {
		// A child's own siblings (shared child ID) go first, via subquery over the
		// child rows about to be deleted.
		for _, sib := range child.Siblings() {
			if err := tx.Exec(ctx, childSiblingDeleteSQL(d, sib.Table(), child.IDColumn(), child.Table(), child.ParentIDColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
				return err
			}
		}
		if err := tx.Exec(ctx, childDeleteSQL(d, child.Table(), child.ParentIDColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
			return err
		}
	}
	if err := deleteSiblings(ctx, tx, d, schema, id); err != nil {
		return err
	}
	// The row's LAST revision AND its created_at instant are read before the DELETE
	// removes them: the DELETED payload stamps them as _ids.revision +
	// _ids.created_at, and the read side writes both into the document
	// tombstone — the guard that stops a zombie consumer's older upsert from
	// resurrecting the document after this delete projects. Any event this
	// aggregate ever produced carries a revision <= this value; the birth
	// instant scopes the tombstone to THIS incarnation of the id, so a
	// deterministic id reborn under the same natural key is never mistaken for
	// a zombie of the dead one.
	var ownRev int64
	var ownCreatedAt time.Time
	if rc := schema.RevisionColumn(); rc != "" {
		if ownRev, ownCreatedAt, err = readRevisionCreatedAt(ctx, tx, d, schema.Table(), rc, schema.CreatedAtColumn(), schema.IDColumn(), id); err != nil {
			return err
		}
	}
	rootDelete, rootArgs, err := deleteSQL(d, schemaTarget(schema), criteria.Eq(idGoField, domain.NewID(id)))
	if err != nil {
		return err
	}
	if err := tx.Exec(ctx, rootDelete, rootArgs...); err != nil {
		return err
	}
	// SharedBase (M2): if this is a role whose shared identity is now orphaned,
	// converge the base — the database-vetoable purge (DeleteWhenUnreferenced) or
	// the orphan archive (archivable base). An actual purge carries its own
	// outbox row + audit event; the bundle echoes post-commit below.
	purge, basePurged, err := b.convergeBaseAfterHardDelete(ctx, tx, d, schema, src, now, buildPurgeEvent)
	if err != nil {
		return err
	}
	meta := outboxMeta{ID: id, Revision: ownRev, CreatedAt: ownCreatedAt, BasePurged: basePurged}
	if base, _, ok := schema.SharedBaseRef(); ok {
		if _, nk := sharedBaseValues(base, src); nk != "" {
			meta.BaseID = deterministicBaseID(nk)
			if !basePurged {
				// A role hard-delete is an identity-touching write (the remnant
				// pick / segment of every SharedBaseView changes), so the
				// base revision advances even when the convergence itself was a
				// no-op on the base row (KeepOrphan, still-referenced, no
				// DeletedAt). The purge branch is exempt — the base row is gone.
				if err = bumpBaseRevision(ctx, tx, d, base, meta.BaseID); err != nil {
					return err
				}
				if meta.BaseRevision, err = readBaseRevision(ctx, tx, d, base, meta.BaseID); err != nil {
					return err
				}
			}
		}
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "DELETED", id, buildDeletePayload(schema, src, id, meta)); err != nil {
		return err
	}
	ab := b.BuildAudit(buildEvent, evs)
	if err := b.WriteAuditRow(ctx, tx, ab.Ev); err != nil {
		return err
	}
	if err := b.FireBeforeCommit(ctx, tx, src, domain.NewID(id), hook, hctx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	b.AfterCommit(ctx, ab)
	b.AfterCommit(ctx, purge) // zero bundle (no purge) is inert
	return nil
}

// writeChildren persists every aggregate child by the operation its
// (originalStatus, currentStatus) pair resolves to via domain.OperationOf — the
// SINGLE categorization shared with the domain query helpers and the auditor
// (mirrors the reference ddd-kernel). One pass covers both a fresh insert (all
// children resolve to Insert) and an update/upsert (a loaded item is Constructor →
// Noop, re-added/changed → Update, removed → Delete) — there is no insert-vs-diff
// split, and a loaded base-child (Constructor) is never re-inserted, inherently.
// Each child is ParentID-routed by ResolveAggregateChild: a role's own child takes the
// role id (rootID); a shared-base native child takes the base id (baseID, "" when
// the schema has no shared base).
func writeChildren(ctx context.Context, tx WriteTx, d Dialect, root *domain.AggregateRoot, schema *TableSchema, rootID, baseID string, now time.Time) error {
	if root == nil {
		return nil
	}
	for typeName, items := range root.AllAggregateItems() {
		child, fromBase, ok := schema.ResolveAggregateChild(typeName)
		if !ok {
			return undeclaredChildErr(schema, typeName)
		}
		fkID := rootID
		if fromBase {
			fkID = baseID
		}
		for _, it := range items {
			switch domain.OperationOf(it.OriginalStatus, it.CurrentStatus) {
			case domain.OpInsert:
				childID, err := insertChild(ctx, tx, d, child, it.Item, fkID, now)
				if err != nil {
					return err
				}
				// Write the minted ID back into the aggregate map so post-write
				// readers (FromEntity projections, the outbox/audit snapshots
				// built after this loop) see the child as persisted, id included.
				root.AssignAggregateItemID(it.Item, childID)
			case domain.OpUpdate:
				if err := updateChild(ctx, tx, d, child, it.Item, now); err != nil {
					return err
				}
			case domain.OpDelete:
				if err := removeChild(ctx, tx, d, child, it.Item, now); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// removeChild applies a Removed child: the CHILD'S SCHEMA decides, exactly like the
// root's does. A child that declares DeletedAt is ARCHIVED (the row lingers, hidden,
// and the owner's unarchive brings it back); a child that declares none has no state
// to stamp, so the honest write is the DELETE — the same answer the root gives an
// entity without DeletedAt. Position in the tree is irrelevant: a role's own child and
// a shared base's native child follow the identical rule.
//
// A hard-removed child takes its sibling rows with it, mirroring what hardDelete does
// for the whole aggregate (a sibling is a 1:1 slice of the child's row and cannot
// outlive it). Base-children carry no siblings (rejected at boot), so that loop is a
// no-op there — it exists for the role children this path now covers.
func removeChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject, now time.Time) error {
	if sdCol, ok := child.DeletedAtColumn(); ok {
		return archiveChild(ctx, tx, d, child, sdCol, item, now)
	}
	id := item.GetID().Value()
	if id == "" {
		return fmt.Errorf("db: cannot delete child %q without id", child.Table())
	}
	for _, sib := range child.Siblings() {
		sibDelete, sibArgs, err := deleteSQL(d, idOnlyTarget(sib.Table(), child.IDColumn()), criteria.Eq(idGoField, domain.NewID(id)))
		if err != nil {
			return err
		}
		if err := tx.Exec(ctx, sibDelete, sibArgs...); err != nil {
			return err
		}
	}
	childDelete, childArgs, err := deleteSQL(d, schemaTarget(child), criteria.Eq(idGoField, domain.NewID(id)))
	if err != nil {
		return err
	}
	return tx.Exec(ctx, childDelete, childArgs...)
}

// insertChild persists one Added child and returns the ID it minted — the
// caller writes that id back into the aggregate map (AssignAggregateItemID).
func insertChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject, rootID string, now time.Time) (string, error) {
	fields := child.WriteFields(item)
	fields[child.ParentIDColumn()] = domain.NewID(rootID) // ParentID to the root, dialect-encoded by buildInsert
	childID, err := newWriteID()
	if err != nil {
		return "", err
	}
	sql, args := buildInsert(d, child.Table(), child.IDColumn(), childID, fields, child.InsertNowColumns(), now, "")
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return "", err
	}
	// A child may carry its own siblings (the one allowed recursive width); they
	// share the child's ID, read from the same AVO.
	if err := insertSiblings(ctx, tx, d, child, item, childID, now); err != nil {
		return "", err
	}
	return childID, nil
}

func updateChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject, now time.Time) error {
	id := item.GetID().Value()
	if id == "" {
		return fmt.Errorf("db: cannot update child %q without id", child.Table())
	}
	fields := child.WriteFields(item)
	// Unguarded on purpose: a child declares no revision — the OWNER's guarded
	// UPDATE already proved nobody moved the aggregate under this write.
	sql, args, err := buildUpdate(d, schemaTarget(child), criteria.Eq(idGoField, domain.NewID(id)), fields, child.UpdateNowColumns(), now, "", 0)
	if err != nil {
		return err
	}
	if err := execExpectingRow(ctx, tx, d, sql, args, child.Table(), child.Table(), child.IDColumn(), id, 0); err != nil {
		return err
	}
	// A Changed child carries its full new state → treat its siblings as a full
	// replace (partial=false): a slice cleared to all-nil is removed.
	return applySiblingUpdates(ctx, tx, d, child, item, id, false)
}

// archiveChild: Removed → Archive, for a child whose schema declares DeletedAt.
// removeChild resolves the column and only routes here when it exists, so the
// column arrives resolved rather than being re-derived (and re-guarded) here.
func archiveChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, sdCol string, item domain.AggregateValueObject, now time.Time) error {
	id := item.GetID().Value()
	if id == "" {
		return fmt.Errorf("db: cannot archive child %q without id", child.Table())
	}
	sql, args, err := archiveSQL(d, schemaTarget(child), sdCol, criteria.Eq(idGoField, domain.NewID(id)), now, "")
	if err != nil {
		return err
	}
	return tx.Exec(ctx, sql, args...)
}

// undeclaredChildErr is the loud error when an aggregate child type has no
// TableSchema registered on the role OR its shared base (every persisted child
// must be declared via root.Child(...) or base.Child(...)).
func undeclaredChildErr(schema *TableSchema, typeName string) error {
	return fmt.Errorf("db: aggregate child %q has no TableSchema declared on %q (role or shared base)", typeName, schema.Table())
}
