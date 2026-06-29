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
//     archived children; hard delete relies on FK ON DELETE CASCADE
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
	if err := insertChildren(ctx, tx, d, root, schema, id); err != nil {
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
	if err := applyChildChanges(ctx, tx, d, root, schema, entity.ID()); err != nil {
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

func (b *BaseEngine) deleteAggregate(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error {
	// Children removed via FK ON DELETE CASCADE in the schema — DELETE only the root.
	return b.flatSoftWrite(ctx, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Delete", EntityType: entity.EntityName()}, "DELETED",
		func(d Dialect) string { return deleteSQL(d, schema.Table(), schema.PKColumn()) },
		func() audit.AuditEvent { return BuildDeleteEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

// softWriteAggregate is the shared root-soft-write + child-cascade path for
// archive/unarchive: soft-write the root, then cascade onto each declared child
// (setExpr = NOW()/NULL, gate = " IS NULL"/" IS NOT NULL"), then outbox + audit
// + hooks + post-commit.
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
	if err := WriteOutbox(ctx, tx, schema.Table(), eventType, id, nil); err != nil {
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

// insertChildren INSERTs items with status Added or Constructor for each child
// type, FK injected from the root id (dialect-encoded by buildInsert).
func insertChildren(ctx context.Context, tx WriteTx, d Dialect, root *domain.AggregateRoot, schema *TableSchema, rootID string) error {
	if root == nil {
		return nil
	}
	for typeName, items := range root.AllAggregateItems() {
		child, err := childSchemaOrErr(schema, typeName)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.CurrentStatus != domain.StatusAdded && it.CurrentStatus != domain.StatusConstructor {
				continue
			}
			if err := insertChild(ctx, tx, d, child, it.Item, rootID); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyChildChanges processes Added/Changed/Removed during an aggregate update.
// Constructor items are no-op. Removed always Archives (symmetric universal
// cascade; no per-child hard delete).
func applyChildChanges(ctx context.Context, tx WriteTx, d Dialect, root *domain.AggregateRoot, schema *TableSchema, rootID string) error {
	if root == nil {
		return nil
	}
	for typeName, items := range root.AllAggregateItems() {
		child, err := childSchemaOrErr(schema, typeName)
		if err != nil {
			return err
		}
		for _, it := range items {
			switch it.CurrentStatus {
			case domain.StatusAdded:
				if err := insertChild(ctx, tx, d, child, it.Item, rootID); err != nil {
					return err
				}
			case domain.StatusChanged:
				if err := updateChild(ctx, tx, d, child, it.Item); err != nil {
					return err
				}
			case domain.StatusRemoved:
				if err := archiveChild(ctx, tx, d, child, typeName, it.Item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func insertChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject, rootID string) error {
	fields := child.WriteFields(item)
	fields[child.FKColumn()] = domain.NewID(rootID) // FK to the root, dialect-encoded by buildInsert
	childID, err := newWriteID()
	if err != nil {
		return err
	}
	sql, args := buildInsert(d, child.Table(), child.PKColumn(), childID, fields, child.InsertNowColumns())
	return tx.Exec(ctx, sql, args...)
}

func updateChild(ctx context.Context, tx WriteTx, d Dialect, child *TableSchema, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("db: cannot update child %q without id", child.Table())
	}
	fields := child.WriteFields(item)
	sql, args := buildUpdate(d, child.Table(), child.PKColumn(), id, fields, child.UpdateNowColumns())
	return execExpectingRow(ctx, tx, sql, args, child.Table(), child.PKColumn(), id)
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

// childSchemaOrErr resolves the declared schema for an aggregate child type,
// erroring loudly when the child is undeclared (every persisted aggregate child
// must have its own TableSchema registered via root.Child(...)).
func childSchemaOrErr(schema *TableSchema, typeName string) (*TableSchema, error) {
	child := schema.ChildSchema(typeName)
	if child == nil {
		return nil, fmt.Errorf("db: aggregate child %q has no TableSchema declared on %q", typeName, schema.Table())
	}
	return child, nil
}
