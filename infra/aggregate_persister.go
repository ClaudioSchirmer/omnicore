package infra

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/jackc/pgx/v5"
)

// Aggregate-aware persistence consumes the request persistence.RequestContext
// (audit + cancellation) plus the entity's TableSchema (explicit Go↔column map,
// root + child schemas). Each method threads ctx through pgx and emits the audit
// event in lockstep with the data write — same pattern executor.go follows.
//
// Guarantees (preserved):
//   - Single pgx.Tx for root + all children
//   - Exactly one outbox row per call (granularity B)
//   - Exactly one audit_events row per call when the database destination is on
//   - FK injected from root id before the child INSERT
//   - Status iteration: Added→INSERT; Changed→UPDATE; Removed→Archive (symmetric
//     universal cascade); Constructor→no-op (update) or INSERT (insert).
//   - Archive of root cascades Archive on all active children
//   - Unarchive of root restores all archived children
//   - Hard Delete of root relies on FK ON DELETE CASCADE in the schema

func (p *Postgres) insertAggregate(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook writeHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := schema.Table()
	rootFields := schema.writeFields(src)
	hctx := hookContext{verb: "Insert", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	// Position A — BEFORE root write + children + outbox + audit.
	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildInsert(rootTable, rootFields, schema.PKColumn(), schema.insertNowColumns())
	var rootID string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&rootID); err != nil {
		return domain.WriteResult{}, err
	}

	if err := insertChildren(ctx, tx, root, schema, rootID); err != nil {
		return domain.WriteResult{}, err
	}

	payload := buildAggregatePayload(rootFields, root, schema)
	if err := writeOutbox(ctx, tx, rootTable, "INSERTED", rootID, payload); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildInsertEvent(ctx, entity, domain.NewID(rootID), schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return domain.WriteResult{}, err
	}

	// Position D — AFTER root + children + outbox + audit, BEFORE COMMIT.
	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(rootID), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	p.publishEvents(ctx, entity.Events())
	return domain.WriteResult{ID: rootID, Fields: rootFields}, nil
}

func (p *Postgres) updateAggregate(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook writeHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := schema.Table()
	rootFields := schema.writeFields(src)
	hctx := hookContext{verb: "Update", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildUpdate(rootTable, schema.PKColumn(), entity.ID(), rootFields, schema.updateNowColumns())
	var rootID string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&rootID); err != nil {
		return domain.WriteResult{}, err
	}

	if err := applyChildChanges(ctx, tx, root, schema, rootID); err != nil {
		return domain.WriteResult{}, err
	}

	payload := buildAggregatePayload(rootFields, root, schema)
	if err := writeOutbox(ctx, tx, rootTable, "UPDATED", rootID, payload); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildUpdateEvent(ctx, entity, schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return domain.WriteResult{}, err
	}

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(rootID), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	p.publishEvents(ctx, entity.Events())
	return domain.WriteResult{ID: rootID, Fields: rootFields}, nil
}

func (p *Postgres) archiveAggregate(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook writeHook) error {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := schema.Table()
	hctx := hookContext{verb: "Archive", entityType: entity.EntityName()}

	sdCol, err := requireSoftDelete(schema, entity.EntityName())
	if err != nil {
		return err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, archiveSQL(rootTable, sdCol, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}

	// Cascade Archive. A child with soft-delete disabled has no marker and is skipped.
	if root != nil {
		for typeName := range root.AllAggregateItems() {
			child := schema.childSchema(typeName)
			if child == nil {
				continue
			}
			childSd, ok := child.softDeleteColumn()
			if !ok {
				continue
			}
			cq := fmt.Sprintf(
				"UPDATE %s SET %s = NOW() WHERE %s = $1 AND %s IS NULL",
				validIdentifier(child.Table()), validIdentifier(childSd), validIdentifier(child.FKColumn()), validIdentifier(childSd),
			)
			if _, err := tx.Exec(ctx, cq, entity.ID()); err != nil {
				return err
			}
		}
	}

	if err := writeOutbox(ctx, tx, rootTable, "ARCHIVED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildArchiveEvent(ctx, entity, schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return err
	}

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(entity.ID()), hook, hctx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	p.echoAuditSlog(ctx, ev)
	p.publishEvents(ctx, entity.Events())
	return nil
}

func (p *Postgres) deleteAggregate(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook writeHook) error {
	// Children removed via FK ON DELETE CASCADE in the schema. Only DELETE on the root.
	src := entity.Source()
	rootTable := schema.Table()
	hctx := hookContext{verb: "Delete", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, deleteSQL(rootTable, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, rootTable, "DELETED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildDeleteEvent(ctx, entity, schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return err
	}

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(entity.ID()), hook, hctx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	p.echoAuditSlog(ctx, ev)
	p.publishEvents(ctx, entity.Events())
	return nil
}

func (p *Postgres) unarchiveAggregate(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook writeHook) error {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := schema.Table()
	hctx := hookContext{verb: "Unarchive", entityType: entity.EntityName()}

	sdCol, err := requireSoftDelete(schema, entity.EntityName())
	if err != nil {
		return err
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, unarchiveSQL(rootTable, sdCol, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}

	// Cascade Unarchive. A child with soft-delete disabled is skipped.
	if root != nil {
		for typeName := range root.AllAggregateItems() {
			child := schema.childSchema(typeName)
			if child == nil {
				continue
			}
			childSd, ok := child.softDeleteColumn()
			if !ok {
				continue
			}
			cq := fmt.Sprintf(
				"UPDATE %s SET %s = NULL WHERE %s = $1 AND %s IS NOT NULL",
				validIdentifier(child.Table()), validIdentifier(childSd), validIdentifier(child.FKColumn()), validIdentifier(childSd),
			)
			if _, err := tx.Exec(ctx, cq, entity.ID()); err != nil {
				return err
			}
		}
	}

	if err := writeOutbox(ctx, tx, rootTable, "UNARCHIVED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildUnarchiveEvent(ctx, entity, schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return err
	}

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(entity.ID()), hook, hctx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}
	p.echoAuditSlog(ctx, ev)
	p.publishEvents(ctx, entity.Events())
	return nil
}

// childSchemaOrErr resolves the declared schema for an aggregate child type,
// erroring loudly when the child is undeclared (every persisted aggregate child
// must have its own TableSchema registered via root.Child(...)).
func childSchemaOrErr(schema *TableSchema, typeName string) (*TableSchema, error) {
	child := schema.childSchema(typeName)
	if child == nil {
		return nil, fmt.Errorf("infra: aggregate child %q has no TableSchema declared on %q", typeName, schema.Table())
	}
	return child, nil
}

// insertChildren INSERTs items with status Added or Constructor for each
// typeName discovered in the AggregateRoot. FK injected from rootID.
func insertChildren(ctx context.Context, tx pgx.Tx, root *domain.AggregateRoot, schema *TableSchema, rootID string) error {
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
			if err := insertChild(ctx, tx, child, it.Item, rootID); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyChildChanges processes Added/Changed/Removed items during aggregate
// update. Constructor items are no-op. Removed always performs Archive
// (symmetric universal cascade; no per-child HardDelete).
func applyChildChanges(ctx context.Context, tx pgx.Tx, root *domain.AggregateRoot, schema *TableSchema, rootID string) error {
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
				if err := insertChild(ctx, tx, child, it.Item, rootID); err != nil {
					return err
				}
			case domain.StatusChanged:
				if err := updateChild(ctx, tx, child, it.Item); err != nil {
					return err
				}
			case domain.StatusRemoved:
				if err := archiveChild(ctx, tx, child, typeName, it.Item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func insertChild(ctx context.Context, tx pgx.Tx, child *TableSchema, item domain.AggregateValueObject, rootID string) error {
	fields := child.writeFields(item)
	fields[child.FKColumn()] = rootID
	sql, args := buildInsert(child.Table(), fields, child.PKColumn(), child.insertNowColumns())
	var id string
	return tx.QueryRow(ctx, sql, args...).Scan(&id)
}

func updateChild(ctx context.Context, tx pgx.Tx, child *TableSchema, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("infra: cannot update child %q without id", child.Table())
	}
	fields := child.writeFields(item)
	sql, args := buildUpdate(child.Table(), child.PKColumn(), id, fields, child.updateNowColumns())
	var returned string
	return tx.QueryRow(ctx, sql, args...).Scan(&returned)
}

// archiveChild: Removed → Archive. A child with soft-delete disabled cannot be
// archived — surfaced as an error.
func archiveChild(ctx context.Context, tx pgx.Tx, child *TableSchema, typeName string, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("infra: cannot archive child %q without id", child.Table())
	}
	sdCol, err := requireSoftDelete(child, typeName)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, archiveSQL(child.Table(), sdCol, child.PKColumn()), id)
	return err
}

// buildAggregatePayload assembles the outbox JSON snapshot: root fields + active
// children grouped by typeName.
func buildAggregatePayload(rootFields domain.Fields, root *domain.AggregateRoot, schema *TableSchema) map[string]any {
	payload := map[string]any{
		"root": rootFields,
	}
	if root == nil {
		return payload
	}
	children := map[string][]domain.Fields{}
	for typeName, items := range root.AllAggregateItems() {
		child := schema.childSchema(typeName)
		if child == nil {
			continue
		}
		active := []domain.Fields{}
		for _, it := range items {
			if it.CurrentStatus == domain.StatusRemoved {
				continue
			}
			active = append(active, child.writeFields(it.Item))
		}
		if len(active) > 0 {
			children[typeName] = active
		}
	}
	if len(children) > 0 {
		payload["children"] = children
	}
	return payload
}
