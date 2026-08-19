package write

import (
	"context"
	"time"

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
	// A role with a SharedBase routes here whether it is flat OR an aggregate:
	// insertWithBase establishes the shared identity + role existence and then
	// layers the aggregate's children/siblings on top.
	if base, fkCol, ok := schema.SharedBaseRef(); ok {
		return b.insertWithBase(ctx, entity, schema, hook, base, fkCol)
	}
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.insertAggregate(ctx, entity, schema, hook)
	}
	src := entity.Source()
	fields := schema.WriteFields(src)
	id, err := newWriteID()
	if err != nil {
		return domain.WriteResult{}, err
	}
	now := writeNow()

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
	sql, args := buildInsert(d, schema.Table(), schema.IDColumn(), id, fields, schema.InsertNowColumns(), now, schema.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id, now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id,
		buildWritePayload(schema, src, nil, "INSERTED", now, fields, outboxMeta{ID: id, Revision: 1, CreatedAt: insertCreatedAt(schema, now)})); err != nil {
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
	return domain.WriteResult{ID: domain.NewID(id), Fields: fields}, nil
}

func (b *BaseEngine) Update(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook WriteHook) (domain.WriteResult, error) {
	// What this write IS comes from the SEALED value — one field, always
	// definitive, never combined with anything else. A domain rule may have
	// finished the update as an archive (domain.CompleteAsArchive); nothing
	// between Get* and here could have introduced or removed that. Honored
	// literally: past the domain's guards this IS the archive verb, carrying the
	// update's field changes along.
	if entity.EntityMode() == domain.ModeArchive {
		root, _ := entity.AggregateInfo()
		return domain.WriteResult{ID: entity.ID()}, b.softWrite(ctx, entity.Source(), root, entity.ID().Value(), schema, hook,
			HookContext{Verb: "Archive", EntityType: entity.EntityName()}, "ARCHIVED", writeNow(),
			func() audit.AuditEvent { return BuildArchiveEvent(ctx, entity, schema, b.auditClaims) },
			entity.Events())
	}
	if base, fkCol, ok := schema.SharedBaseRef(); ok {
		return b.updateWithBase(ctx, entity, schema, hook, base, fkCol)
	}
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.updateAggregate(ctx, entity, schema, hook)
	}
	src := entity.Source()
	fields := schema.WriteFields(src)
	now := writeNow()

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
	rev := loadedRevision(src)
	sql, args := buildUpdate(d, schema.Table(), schema.IDColumn(), entity.ID().Value(), fields, schema.UpdateNowColumns(), now, schema.RevisionColumn(), rev)
	if err := execExpectingRow(ctx, tx, d, sql, args, schema.Table(), entity.EntityName(), schema.IDColumn(), entity.ID().Value(), rev); err != nil {
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
		buildWritePayload(schema, src, nil, "UPDATED", now, fields, meta)); err != nil {
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
	return domain.WriteResult{ID: entity.ID(), Fields: fields}, nil
}

func (b *BaseEngine) Archive(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook WriteHook) error {
	root, _ := entity.AggregateInfo()
	return b.softWrite(ctx, entity.Source(), root, entity.ID().Value(), schema, hook,
		HookContext{Verb: "Archive", EntityType: entity.EntityName()}, "ARCHIVED", writeNow(),
		func() audit.AuditEvent { return BuildArchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

func (b *BaseEngine) Unarchive(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook WriteHook) error {
	root, _ := entity.AggregateInfo()
	return b.softWrite(ctx, entity.Source(), root, entity.ID().Value(), schema, hook,
		HookContext{Verb: "Unarchive", EntityType: entity.EntityName()}, "UNARCHIVED", writeNow(),
		func() audit.AuditEvent { return BuildUnarchiveEvent(ctx, entity, schema, b.auditClaims) },
		entity.Events())
}

func (b *BaseEngine) Delete(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook WriteHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return b.deleteAggregate(ctx, entity, schema, hook)
	}
	// Flat path: hardDelete clears any owner siblings (by the shared ID) before
	// the root DELETE, in the same TX. A schema without siblings collapses to the
	// single root DELETE — behavior-identical to before.
	return b.hardDelete(ctx, entity.Source(), entity.ID().Value(), schema, hook,
		HookContext{Verb: "Delete", EntityType: entity.EntityName()},
		func() audit.AuditEvent { return BuildDeleteEvent(ctx, entity, schema, b.auditClaims) },
		func(baseID string) audit.AuditEvent {
			return BuildSharedBasePurgeEvent(ctx, entity, schema, baseID, b.auditClaims)
		},
		entity.Events())
}

// softWrite is the body of the two bodyless verbs (Archive/Unarchive), and it
// is DELIBERATELY the update path: the framework has ONE rule — the entity's
// field set at write time is what gets persisted — and these verbs used to be
// its only exception. They wrote a single column while the outbox payload
// announced every one of them, so any state the domain produced on the way here
// (a Command's ApplyTo, an IfArchive closure flipping a status) vanished from
// the system of record and appeared in the read model as if it had been stored.
//
// So the row write here is exactly the UPDATE the other verbs emit — full field
// set, managed timestamps, revision bump, guarded on the loaded revision — with
// the transition riding along as one more written column: the DeletedAt column
// bound to `now` (archive) or to SQL NULL (unarchive). The payload then
// describes what the statement wrote, which is what makes it true.
//
// What archive/unarchive keep of their own — the reason this is not simply
// Update:
//
//   - the DeletedAt column is REQUIRED (requireDeletedAt): no column, no verb;
//   - the one-active-role probe runs BEFORE the row flips to active, or an
//     active-only unique index vetoes the UPDATE itself with a raw constraint
//     error instead of the canonical conflict;
//   - the child cascade is SET-BASED (one statement per child table, not one
//     per item): the verb archives every child row under the ParentID, which is
//     not the per-item categorization an update performs;
//   - the shared identity converges by LIFECYCLE (archive the base once no
//     active role remains, reactivate it on unarchive) — the base's business
//     fields are NOT rewritten here: several roles share that row, it is
//     last-write-wins and deliberately unguarded, so a bodyless verb must not
//     restate it;
//   - the outbox event type stays ARCHIVED / UNARCHIVED, because the read side
//     routes on it (DeleteOnArchive removal, the upstream mirror, the
//     base-revision pull repair).
//
// Siblings are written as a PARTIAL update: an archive must never delete a 1:1
// facet because its columns happen to be all-nil.
func (b *BaseEngine) softWrite(
	ctx persistence.RequestContext,
	src domain.Entity,
	root *domain.AggregateRoot,
	id string,
	schema *TableSchema,
	hook WriteHook,
	hctx HookContext,
	eventType string,
	now time.Time,
	buildEvent func() audit.AuditEvent,
	evs []domain.DomainEvent,
) error {
	sdCol, err := requireDeletedAt(schema, hctx.EntityType)
	if err != nil {
		return err
	}
	archive := eventType == "ARCHIVED"

	tx, err := b.beginner.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	d := tx.Dialect()

	if err := b.FireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}
	// SharedBase role: the one-active-role invariant must be probed BEFORE the
	// row flips to active — an active-only unique index would veto the UPDATE
	// itself with a raw constraint error otherwise.
	if !archive {
		if err := b.vetoUnarchiveWithActiveSibling(ctx, tx, d, schema, src, id, hctx.EntityType); err != nil {
			return err
		}
	}
	// The transition is a written column like any other: `now` archives, SQL
	// NULL restores. WriteFields never carries it (DeletedAt is managed), so the
	// verb adds it to the same map the statement and the payload both read.
	fields := schema.WriteFields(src)
	if archive {
		fields[sdCol] = now
	} else {
		fields[sdCol] = nil
	}
	rev := loadedRevision(src)
	sql, args := buildUpdate(d, schema.Table(), schema.IDColumn(), id, fields, schema.UpdateNowColumns(), now, schema.RevisionColumn(), rev)
	if err := execExpectingRow(ctx, tx, d, sql, args, schema.Table(), hctx.EntityType, schema.IDColumn(), id, rev); err != nil {
		return err
	}
	if err := cascadeChildren(ctx, tx, d, root, schema, id, archive, now); err != nil {
		return err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, id, true); err != nil {
		return err
	}
	// SharedBase role: drive the shared identity's lifecycle from this verb
	// (archive once no role stays active; reactivate on unarchive). No-op otherwise.
	if err := b.convergeBaseAfterSoftWrite(ctx, tx, d, schema, src, eventType, now); err != nil {
		return err
	}
	// Payload meta AFTER the convergence, so its base_revision reflects any
	// lifecycle transition this verb caused on the base row.
	meta, err := outboxMetaFor(ctx, tx, d, schema, src, id)
	if err != nil {
		return err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), eventType, id,
		buildWritePayload(schema, src, root, eventType, now, fields, meta)); err != nil {
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

// cascadeChildren applies the root's transition to every declared child table
// that has a DeletedAt column, ONE statement per table (WHERE ParentID = root).
// This is what an aggregate archive means and it is why the soft verbs do not
// route children through writeChildren: the cascade reaches every child row of
// the aggregate, including any the caller never loaded, at a cost that does not
// grow with the collection. A flat entity (root == nil) contributes nothing.
func cascadeChildren(ctx context.Context, tx WriteTx, d Dialect, root *domain.AggregateRoot, schema *TableSchema, id string, archive bool, now time.Time) error {
	if root == nil {
		return nil
	}
	for typeName := range root.AllAggregateItems() {
		child := schema.ChildSchema(typeName)
		if child == nil {
			continue
		}
		childSd, ok := child.DeletedAtColumn()
		if !ok {
			continue
		}
		var err error
		if archive {
			err = tx.Exec(ctx, archiveCascadeSQL(d, child.Table(), childSd, child.ParentIDColumn()),
				d.EncodeArg(now), d.EncodeArg(domain.NewID(id)))
		} else {
			err = tx.Exec(ctx, childCascadeSQL(d, child.Table(), childSd, child.ParentIDColumn(), nullSetExpr(d), " IS NOT NULL"),
				d.EncodeArg(domain.NewID(id)))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// loadedRevision answers the optimistic-concurrency token the entity carries
// from its load. A persisted row is always >= 1 (an INSERT initializes it), so 0
// means the entity never came from the loader — a hand-built value, or a
// repository outside the framework's read path — and the write goes unguarded
// rather than failing every time. Probed, never required: the capability comes
// from the domain.Managed carrier every entity embeds.
func loadedRevision(src domain.Entity) int64 {
	if rc, ok := any(src).(interface{ GetRevision() int64 }); ok {
		return rc.GetRevision()
	}
	return 0
}

// execExpectingRow runs an UPDATE that must match exactly one row — uniform
// across dialects via WriteTx.ExecCount (pgconn.CommandTag / sql.Result both
// expose RowsAffected). On Postgres an UPDATE counts the matched row even when
// no column changes; on MySQL clientFoundRows gives the same "matched, not
// changed" count — so a no-op update of an existing row is never a 404. Any
// driver error passes through unchanged.
//
// Zero rows has TWO causes once the statement is revision-guarded
// (guardedRevision > 0, see buildUpdate), and they map to different answers: the
// row is gone (RecordNotFoundNotification, 404) or it moved past the caller's
// revision (ConcurrentModificationNotification, 409). One SELECT tells them
// apart, and it runs ONLY here on the failure path — the happy path pays
// nothing, because the guard rides the UPDATE's own WHERE. An unguarded
// statement skips the probe: zero rows can only mean the row is gone.
func execExpectingRow(ctx context.Context, tx WriteTx, d Dialect, sql string, args []any, table, contextName, field, value string, guardedRevision int64) error {
	n, err := tx.ExecCount(ctx, sql, args...)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if guardedRevision > 0 {
		exists, err := rowExists(ctx, tx, d, table, field, value)
		if err != nil {
			return err
		}
		if exists {
			return SingleNotificationError(contextName, field, domain.ConcurrentModificationNotification{})
		}
	}
	return domain.NotFoundError(contextName, field, value)
}

// rowExists probes whether the row still exists, to split a zero-row guarded
// UPDATE into "gone" and "moved". Runs inside the write TX, so it sees this
// transaction's own writes.
func rowExists(ctx context.Context, tx WriteTx, d Dialect, table, pkCol, id string) (bool, error) {
	rows, err := tx.Query(ctx, rowExistsSQL(d, table, pkCol), d.EncodeArg(domain.NewID(id)))
	if err != nil || rows == nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}
