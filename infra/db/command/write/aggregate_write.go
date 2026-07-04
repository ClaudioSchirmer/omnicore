package write

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The aggregate-aware write path, written once on BaseEngine. Guarantees
// (identical on every backend):
//   - one framework-owned TX for root + all children
//   - exactly one outbox row per call (granularity B) + one in-TX audit row
//   - the Go-generated id is the FK injected (dialect-encoded) before each child INSERT
//   - status iteration: Added→INSERT, Changed→UPDATE, Removed→Archive,
//     Constructor→no-op (update) / INSERT (insert)
//   - root archive cascades archive of active children; unarchive restores
//     archived children; hard delete cascades an explicit DELETE per declared
//     child table (by FK) before the root DELETE, in the same TX — the framework
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
	hctx := HookContext{Verb: "Insert", EntityType: entity.EntityName()}

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	sql, args := buildInsert(d, schema.Table(), schema.PKColumn(), id, rootFields, schema.InsertNowColumns())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeChildren(ctx, tx, d, root, schema, id, ""); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id, BuildAggregatePayload(rootFields, root, schema)); err != nil {
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
	return domain.WriteResult{ID: id, Fields: rootFields}, nil
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
	hctx := HookContext{Verb: "Update", EntityType: entity.EntityName()}

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	sql, args := buildUpdate(d, schema.Table(), schema.PKColumn(), entity.ID(), rootFields, schema.UpdateNowColumns())
	if err := execExpectingRow(ctx, tx, sql, args, entity.EntityName(), schema.PKColumn(), entity.ID()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeChildren(ctx, tx, d, root, schema, entity.ID(), ""); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID(), BuildAggregatePayload(rootFields, root, schema)); err != nil {
		return domain.WriteResult{}, err
	}
	ab := b.BuildAudit(func() audit.AuditEvent {
		return BuildUpdateEvent(ctx, entity, schema, b.auditClaims)
	}, entity.Events())
	if err := b.WriteAuditRow(ctx, tx, ab.Ev); err != nil {
		return domain.WriteResult{}, err
	}
	if err := b.FireBeforeCommit(ctx, tx, src, domain.NewID(entity.ID()), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	b.AfterCommit(ctx, ab)
	return domain.WriteResult{ID: entity.ID(), Fields: rootFields}, nil
}

func (b *BaseEngine) archiveAggregate(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook WriteHook) error {
	root, _ := entity.AggregateInfo()
	return b.softWriteAggregate(ctx, root, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Archive", EntityType: entity.EntityName()}, "ARCHIVED",
		archiveSQL, "NOW()", " IS NULL",
		func() audit.AuditEvent { return BuildArchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

func (b *BaseEngine) unarchiveAggregate(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook WriteHook) error {
	root, _ := entity.AggregateInfo()
	return b.softWriteAggregate(ctx, root, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Unarchive", EntityType: entity.EntityName()}, "UNARCHIVED",
		unarchiveSQL, "NULL", " IS NOT NULL",
		func() audit.AuditEvent { return BuildUnarchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

// deleteAggregate hard-deletes an aggregate root via the shared hardDelete path
// (children + siblings cleared explicitly in Go before the root, one TX).
func (b *BaseEngine) deleteAggregate(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error {
	return b.hardDelete(ctx, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Delete", EntityType: entity.EntityName()},
		func() audit.AuditEvent { return BuildDeleteEvent(ctx, entity, schema, b.auditClaims) },
		func(baseID string) audit.AuditEvent {
			return BuildSharedBasePurgeEvent(ctx, entity, schema, baseID, b.auditClaims)
		},
		entity.Events())
}

// hardDelete is the shared hard-delete orchestration for both the flat and the
// aggregate paths: in one framework-owned TX it deletes every declared child
// table (by FK) and every owner sibling table (by the shared PK) EXPLICITLY in
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

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}
	for _, child := range schema.ChildSchemas() {
		// A child's own siblings (shared child PK) go first, via subquery over the
		// child rows about to be deleted.
		for _, sib := range child.Siblings() {
			if err := tx.Exec(ctx, childSiblingDeleteSQL(d, sib.Table(), child.PKColumn(), child.Table(), child.FKColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
				return err
			}
		}
		if err := tx.Exec(ctx, childDeleteSQL(d, child.Table(), child.FKColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
			return err
		}
	}
	if err := deleteSiblings(ctx, tx, d, schema, id); err != nil {
		return err
	}
	if err := tx.Exec(ctx, deleteSQL(d, schema.Table(), schema.PKColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
		return err
	}
	// SharedBase (M2): if this is a role whose shared identity is now orphaned,
	// converge the base — the database-vetoable purge (DeleteWhenUnreferenced) or
	// the orphan archive (soft-deletable base). An actual purge carries its own
	// outbox row + audit event; the bundle echoes post-commit below.
	purge, err := b.convergeBaseAfterHardDelete(ctx, tx, d, schema, src, buildPurgeEvent)
	if err != nil {
		return err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "DELETED", id, deleteKeysPayload(schema, src, id)); err != nil {
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

// softWriteAggregate is the shared root-soft-write + child-cascade path for
// archive/unarchive: soft-write the root, then cascade onto each declared child
// (setExpr = NOW()/NULL, gate = " IS NULL"/" IS NOT NULL"), then outbox (the
// aggregate snapshot with the root's soft-delete column reflecting the verb)
// + audit + hooks + post-commit.
func (b *BaseEngine) softWriteAggregate(
	ctx persistence.RequestContext,
	root *domain.AggregateRoot,
	src domain.Entity,
	id string,
	schema *TableSchema,
	hook WriteHook,
	hctx HookContext,
	eventType string,
	rootStmt func(d Dialect, table, sdCol, pk string) string,
	childSet, childGate string,
	buildEvent func() audit.AuditEvent,
	evs []domain.DomainEvent,
) error {
	sdCol, err := requireSoftDelete(schema, hctx.EntityType)
	if err != nil {
		return err
	}

	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}
	if err := tx.Exec(ctx, rootStmt(d, schema.Table(), sdCol, schema.PKColumn()), d.EncodeArg(domain.NewID(id))); err != nil {
		return err
	}
	// Cascade onto each declared child with a soft-delete column.
	if root != nil {
		for typeName := range root.AllAggregateItems() {
			child := schema.ChildSchema(typeName)
			if child == nil {
				continue
			}
			childSd, ok := child.SoftDeleteColumn()
			if !ok {
				continue
			}
			cq := childCascadeSQL(d, child.Table(), childSd, child.FKColumn(), childSet, childGate)
			if err := tx.Exec(ctx, cq, d.EncodeArg(domain.NewID(id))); err != nil {
				return err
			}
		}
	}
	// SharedBase role (aggregate): drive the shared identity's lifecycle from this
	// verb — archive it once no role stays active, reactivate on unarchive. The
	// base's NATIVE children cascade with the base, not with this role. No-op when
	// the role declares no shared base.
	if err := b.convergeBaseAfterSoftWrite(ctx, tx, d, schema, src, eventType); err != nil {
		return err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), eventType, id,
		BuildAggregatePayload(softWritePayload(schema, src, sdCol, eventType), root, schema)); err != nil {
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
	return nil
}

// writeChildren persists every aggregate child by the operation its
// (originalStatus, currentStatus) pair resolves to via domain.OperationOf — the
// SINGLE categorization shared with the domain query helpers and the auditor
// (mirrors the reference ddd-kernel). One pass covers both a fresh insert (all
// children resolve to Insert) and an update/upsert (a loaded item is Constructor →
// Noop, re-added/changed → Update, removed → Delete) — there is no insert-vs-diff
// split, and a loaded base-child (Constructor) is never re-inserted, inherently.
// Each child is FK-routed by ResolveAggregateChild: a role's own child takes the
// role id (rootID); a shared-base native child takes the base id (baseID, "" when
// the schema has no shared base).
func writeChildren(ctx context.Context, tx WriteTx, d Dialect, root *domain.AggregateRoot, schema *TableSchema, rootID, baseID string) error {
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
				if err := insertChild(ctx, tx, d, child, it.Item, fkID); err != nil {
					return err
				}
			case domain.OpUpdate:
				if err := updateChild(ctx, tx, d, child, it.Item); err != nil {
					return err
				}
			case domain.OpDelete:
				if err := removeChild(ctx, tx, d, child, typeName, it.Item, fromBase); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// removeChild applies a Removed child: archive (soft-delete) when the child has a
// soft-delete column, else — only for a base-child, whose lifecycle follows the
// shared base — hard-delete the row. A role child without soft-delete still errors
// inside archiveChild (unchanged): a removable role child must be archivable.
func removeChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, typeName string, item domain.AggregateValueObject, fromBase bool) error {
	if fromBase {
		if _, ok := child.SoftDeleteColumn(); !ok {
			id := item.GetID()
			if id == "" {
				return fmt.Errorf("db: cannot delete base child %q without id", child.Table())
			}
			return tx.Exec(ctx, deleteSQL(d, child.Table(), child.PKColumn()), d.EncodeArg(domain.NewID(id)))
		}
	}
	return archiveChild(ctx, tx, d, child, typeName, item)
}

func insertChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject, rootID string) error {
	fields := child.WriteFields(item)
	fields[child.FKColumn()] = domain.NewID(rootID) // FK to the root, dialect-encoded by buildInsert
	childID, err := newWriteID()
	if err != nil {
		return err
	}
	sql, args := buildInsert(d, child.Table(), child.PKColumn(), childID, fields, child.InsertNowColumns())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	// A child may carry its own siblings (the one allowed recursive width); they
	// share the child's PK, read from the same AVO.
	return insertSiblings(ctx, tx, d, child, item, childID)
}

func updateChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("db: cannot update child %q without id", child.Table())
	}
	fields := child.WriteFields(item)
	sql, args := buildUpdate(d, child.Table(), child.PKColumn(), id, fields, child.UpdateNowColumns())
	if err := execExpectingRow(ctx, tx, sql, args, child.Table(), child.PKColumn(), id); err != nil {
		return err
	}
	// A Changed child carries its full new state → treat its siblings as a full
	// replace (partial=false): a slice cleared to all-nil is removed.
	return applySiblingUpdates(ctx, tx, d, child, item, id, false)
}

// archiveChild: Removed → Archive. A child with soft-delete disabled cannot be
// archived — surfaced as an error.
func archiveChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, typeName string, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("db: cannot archive child %q without id", child.Table())
	}
	sdCol, err := requireSoftDelete(child, typeName)
	if err != nil {
		return err
	}
	return tx.Exec(ctx, archiveSQL(d, child.Table(), sdCol, child.PKColumn()), d.EncodeArg(domain.NewID(id)))
}

// undeclaredChildErr is the loud error when an aggregate child type has no
// TableSchema registered on the role OR its shared base (every persisted child
// must be declared via root.Child(...) or base.Child(...)).
func undeclaredChildErr(schema *TableSchema, typeName string) error {
	return fmt.Errorf("db: aggregate child %q has no TableSchema declared on %q (role or shared base)", typeName, schema.Table())
}
