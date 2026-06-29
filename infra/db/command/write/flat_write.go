package write

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// The flat write path, written once on BaseEngine and promoted onto every
// engine. Each verb runs the canonical sequence in one framework-owned TX
// (opened via the engine's WriteBeginner): position-A hook → data write →
// outbox row → in-TX audit row → position-D hook → COMMIT → post-commit echo +
// domain-event publish. The dialect-specific statement bits (placeholders,
// quoting, id/uuid encoding) come from the WriteTx's Dialect; the id is
// Go-generated (UUID v7) on every backend, so no verb reads anything back.
//
// AggregateInfo() routes aggregate roots to the aggregate path (aggregate_write.go).

func (b *BaseEngine) Insert(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.insertAggregate(ctx, entity, schema, hook)
	}
	src := entity.Source()
	fields := schema.WriteFields(src)
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
	sql, args := buildInsert(d, schema.Table(), schema.PKColumn(), id, fields, schema.InsertNowColumns())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id, fields); err != nil {
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
	return domain.WriteResult{ID: id, Fields: fields}, nil
}

func (b *BaseEngine) Update(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.updateAggregate(ctx, entity, schema, hook)
	}
	src := entity.Source()
	fields := schema.WriteFields(src)

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
	sql, args := buildUpdate(d, schema.Table(), schema.PKColumn(), entity.ID(), fields, schema.UpdateNowColumns())
	if err := execExpectingRow(ctx, tx, sql, args, entity.EntityName(), schema.PKColumn(), entity.ID()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID(), fields); err != nil {
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
	return domain.WriteResult{ID: entity.ID(), Fields: fields}, nil
}

func (b *BaseEngine) Archive(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook WriteHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.archiveAggregate(ctx, entity, schema, hook)
	}
	sdCol, err := requireSoftDelete(schema, entity.EntityName())
	if err != nil {
		return err
	}
	return b.flatSoftWrite(ctx, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Archive", EntityType: entity.EntityName()}, "ARCHIVED",
		func(d Dialect) string { return archiveSQL(d, schema.Table(), sdCol, schema.PKColumn()) },
		func() audit.AuditEvent { return BuildArchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

func (b *BaseEngine) Unarchive(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook WriteHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.unarchiveAggregate(ctx, entity, schema, hook)
	}
	sdCol, err := requireSoftDelete(schema, entity.EntityName())
	if err != nil {
		return err
	}
	return b.flatSoftWrite(ctx, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Unarchive", EntityType: entity.EntityName()}, "UNARCHIVED",
		func(d Dialect) string { return unarchiveSQL(d, schema.Table(), sdCol, schema.PKColumn()) },
		func() audit.AuditEvent { return BuildUnarchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

func (b *BaseEngine) Delete(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.deleteAggregate(ctx, entity, schema, hook)
	}
	// Flat path: hardDelete clears any owner siblings (by the shared PK) before
	// the root DELETE, in the same TX. A schema without siblings collapses to the
	// single root DELETE — behavior-identical to before.
	return b.hardDelete(ctx, entity.Source(), entity.ID(), schema, hook,
		HookContext{Verb: "Delete", EntityType: entity.EntityName()},
		func() audit.AuditEvent { return BuildDeleteEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

// flatSoftWrite is the shared body of the bodyless flat verbs
// (Archive/Unarchive/Delete): one single-statement write keyed on the PK + the
// outbox row + the in-TX audit row + the A/D hooks, then the post-commit echo +
// publish. buildStmt renders the verb's SQL via the tx Dialect; the id is the
// sole bound arg (encoded per dialect).
func (b *BaseEngine) flatSoftWrite(
	ctx persistence.RequestContext,
	src domain.Entity,
	id string,
	schema *TableSchema,
	hook WriteHook,
	hctx HookContext,
	eventType string,
	buildStmt func(Dialect) string,
	buildEvent func() audit.AuditEvent,
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
	if err := tx.Exec(ctx, buildStmt(d), d.EncodeArg(domain.NewID(id))); err != nil {
		return err
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

// Batch executes a set of operations in a single TX, each under its own
// TableSchema (mandatory — no convention fallback), aligned positionally with
// entity.Operations(). Audit emission for Batch members is intentionally skipped
// (the outbox row per op is kept). Available on every engine.
func (b *BaseEngine) Batch(ctx context.Context, entity domain.Batch, schemas []*TableSchema) ([]domain.WriteResult, error) {
	ops := entity.Operations()
	if len(schemas) != len(ops) {
		return nil, fmt.Errorf("db: Batch requires one TableSchema per operation (got %d schemas for %d ops)", len(schemas), len(ops))
	}
	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()

	results := make([]domain.WriteResult, 0, len(ops))
	for i, op := range ops {
		wr, err := execOneWithTx(ctx, tx, d, op, schemas[i])
		if err != nil {
			return nil, err
		}
		results = append(results, wr)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return results, nil
}

// execOneWithTx runs one Batch member (no hooks, no audit) through the shared
// builders. A missing-row Update maps to RecordNotFoundNotification, identical
// to the standalone Update verb.
func execOneWithTx(ctx context.Context, tx WriteTx, d Dialect, entity domain.ValidEntity, schema *TableSchema) (domain.WriteResult, error) {
	switch e := entity.(type) {
	case domain.Insertable:
		fields := schema.WriteFields(e.Source())
		id, err := newWriteID()
		if err != nil {
			return domain.WriteResult{}, err
		}
		sql, args := buildInsert(d, schema.Table(), schema.PKColumn(), id, fields, schema.InsertNowColumns())
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return domain.WriteResult{}, err
		}
		if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id, fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: id, Fields: fields}, nil

	case domain.Updatable:
		fields := schema.WriteFields(e.Source())
		sql, args := buildUpdate(d, schema.Table(), schema.PKColumn(), e.ID(), fields, schema.UpdateNowColumns())
		if err := execExpectingRow(ctx, tx, sql, args, e.EntityName(), schema.PKColumn(), e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", e.ID(), fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID(), Fields: fields}, nil

	case domain.Archivable:
		sdCol, err := requireSoftDelete(schema, e.EntityName())
		if err != nil {
			return domain.WriteResult{}, err
		}
		if err := tx.Exec(ctx, archiveSQL(d, schema.Table(), sdCol, schema.PKColumn()), d.EncodeArg(domain.NewID(e.ID()))); err != nil {
			return domain.WriteResult{}, err
		}
		if err := WriteOutbox(ctx, tx, schema.Table(), "ARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Unarchivable:
		sdCol, err := requireSoftDelete(schema, e.EntityName())
		if err != nil {
			return domain.WriteResult{}, err
		}
		if err := tx.Exec(ctx, unarchiveSQL(d, schema.Table(), sdCol, schema.PKColumn()), d.EncodeArg(domain.NewID(e.ID()))); err != nil {
			return domain.WriteResult{}, err
		}
		if err := WriteOutbox(ctx, tx, schema.Table(), "UNARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Deletable:
		if err := tx.Exec(ctx, deleteSQL(d, schema.Table(), schema.PKColumn()), d.EncodeArg(domain.NewID(e.ID()))); err != nil {
			return domain.WriteResult{}, err
		}
		if err := WriteOutbox(ctx, tx, schema.Table(), "DELETED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil
	}
	return domain.WriteResult{}, fmt.Errorf("db: unknown entity type %T", entity)
}

// execExpectingRow runs an UPDATE that must match exactly one row and maps a
// zero-row result to the canonical RecordNotFoundNotification (404) — uniform
// across dialects via WriteTx.ExecCount (pgconn.CommandTag / sql.Result both
// expose RowsAffected). On Postgres an UPDATE counts the matched row even when
// no column changes; on MySQL clientFoundRows gives the same "matched, not
// changed" count — so a no-op update of an existing row is never a 404. Any
// driver error passes through unchanged.
func execExpectingRow(ctx context.Context, tx WriteTx, sql string, args []any, contextName, field, value string) error {
	n, err := tx.ExecCount(ctx, sql, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.NotFoundError(contextName, field, value)
	}
	return nil
}
