package infra

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/jackc/pgx/v5"
)

// Postgres.Insert/Update/Archive/Delete/Unarchive receive the request-scoped
// persistence.RequestContext (carries actor + threadId + cancellation) and the
// entity's *TableSchema (the explicit Go-field↔column map). The simple path here
// dispatches to the aggregate variant when AggregateInfo() reports true.
//
// Audit hooks per method:
//   - When the database destination is active, audit.InsertAuditEvent runs
//     INSIDE the TX so the audit row is atomic with the data row + outbox row.
//   - When the slog destination is active, EchoSlog runs POST-COMMIT so a
//     transient slog handler error never blocks the persistence path.
// Both branches share a single audit.AuditEvent built once after the data
// row is materialized (Insert needs the generated id).

func (p *Postgres) Insert(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook writeHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.insertAggregate(ctx, entity, schema, hook)
	}

	src := entity.Source()
	table := schema.Table()
	fields := schema.writeFields(src)
	hctx := hookContext{verb: "Insert", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	// Position A — INSIDE the TX, BEFORE any framework write.
	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildInsert(table, fields, schema.PKColumn(), schema.insertNowColumns())
	var id string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeOutbox(ctx, tx, table, "INSERTED", id, fields); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildInsertEvent(ctx, entity, domain.NewID(id), schema, p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return domain.WriteResult{}, err
	}

	// Position D — INSIDE the TX, AFTER all framework writes, BEFORE COMMIT.
	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(id), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	return domain.WriteResult{ID: id, Fields: fields}, nil
}

func (p *Postgres) Update(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook writeHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.updateAggregate(ctx, entity, schema, hook)
	}

	src := entity.Source()
	table := schema.Table()
	fields := schema.writeFields(src)
	hctx := hookContext{verb: "Update", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildUpdate(table, schema.PKColumn(), entity.ID(), fields, schema.updateNowColumns())
	var id string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeOutbox(ctx, tx, table, "UPDATED", id, fields); err != nil {
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

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(id), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	return domain.WriteResult{ID: id, Fields: fields}, nil
}

func (p *Postgres) Archive(ctx persistence.RequestContext, entity domain.Archivable, schema *TableSchema, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.archiveAggregate(ctx, entity, schema, hook)
	}

	src := entity.Source()
	table := schema.Table()
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

	if _, err := tx.Exec(ctx, archiveSQL(table, sdCol, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "ARCHIVED", entity.ID(), nil); err != nil {
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
	return nil
}

func (p *Postgres) Unarchive(ctx persistence.RequestContext, entity domain.Unarchivable, schema *TableSchema, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.unarchiveAggregate(ctx, entity, schema, hook)
	}

	src := entity.Source()
	table := schema.Table()
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

	if _, err := tx.Exec(ctx, unarchiveSQL(table, sdCol, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "UNARCHIVED", entity.ID(), nil); err != nil {
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
	return nil
}

func (p *Postgres) Delete(ctx persistence.RequestContext, entity domain.Deletable, schema *TableSchema, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.deleteAggregate(ctx, entity, schema, hook)
	}

	src := entity.Source()
	table := schema.Table()
	hctx := hookContext{verb: "Delete", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, deleteSQL(table, schema.PKColumn()), entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "DELETED", entity.ID(), nil); err != nil {
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
	return nil
}

// Batch executes a set of operations in a single TX, each under its own
// TableSchema (the schema is mandatory — there is no convention fallback). The
// caller supplies one schema per operation, aligned positionally with
// entity.Operations(). Audit emission for Batch members is intentionally skipped.
func (p *Postgres) Batch(ctx context.Context, entity domain.Batch, schemas []*TableSchema) ([]domain.WriteResult, error) {
	ops := entity.Operations()
	if len(schemas) != len(ops) {
		return nil, fmt.Errorf("infra: Batch requires one TableSchema per operation (got %d schemas for %d ops)", len(schemas), len(ops))
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	results := make([]domain.WriteResult, 0, len(ops))
	for i, op := range ops {
		wr, err := execWithTx(ctx, tx, op, schemas[i])
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

func execWithTx(ctx context.Context, tx pgx.Tx, entity domain.ValidEntity, schema *TableSchema) (domain.WriteResult, error) {
	switch e := entity.(type) {
	case domain.Insertable:
		fields := schema.writeFields(e.Source())
		sql, args := buildInsert(schema.Table(), fields, schema.PKColumn(), schema.insertNowColumns())
		var id string
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, schema.Table(), "INSERTED", id, fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: id, Fields: fields}, nil

	case domain.Updatable:
		fields := schema.writeFields(e.Source())
		sql, args := buildUpdate(schema.Table(), schema.PKColumn(), e.ID(), fields, schema.updateNowColumns())
		var id string
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, schema.Table(), "UPDATED", id, fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: id, Fields: fields}, nil

	case domain.Archivable:
		sdCol, err := requireSoftDelete(schema, e.EntityName())
		if err != nil {
			return domain.WriteResult{}, err
		}
		if _, err := tx.Exec(ctx, archiveSQL(schema.Table(), sdCol, schema.PKColumn()), e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, schema.Table(), "ARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Unarchivable:
		sdCol, err := requireSoftDelete(schema, e.EntityName())
		if err != nil {
			return domain.WriteResult{}, err
		}
		if _, err := tx.Exec(ctx, unarchiveSQL(schema.Table(), sdCol, schema.PKColumn()), e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, schema.Table(), "UNARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Deletable:
		if _, err := tx.Exec(ctx, deleteSQL(schema.Table(), schema.PKColumn()), e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, schema.Table(), "DELETED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil
	}
	return domain.WriteResult{}, fmt.Errorf("infra: unknown entity type %T", entity)
}

// archiveSQL / unarchiveSQL / deleteSQL build the soft-delete and hard-delete
// statements on the resolved soft-delete column + PK column.
func archiveSQL(table, sdCol, pk string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NOW() WHERE %s = $1",
		validIdentifier(table), validIdentifier(sdCol), validIdentifier(pk))
}

func unarchiveSQL(table, sdCol, pk string) string {
	return fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = $1",
		validIdentifier(table), validIdentifier(sdCol), validIdentifier(pk))
}

func deleteSQL(table, pk string) string {
	return fmt.Sprintf("DELETE FROM %s WHERE %s = $1",
		validIdentifier(table), validIdentifier(pk))
}

// requireSoftDelete is the runtime backstop for the boot-time Modes() ⟺
// SoftDelete check: a write path that needs the soft-delete column on a schema
// that did not declare it fails loudly instead of emitting broken SQL.
func requireSoftDelete(s *TableSchema, entityName string) (string, error) {
	col, ok := s.softDeleteColumn()
	if !ok {
		return "", fmt.Errorf(
			"infra: %s did not declare SoftDelete in its TableSchema — archive/unarchive is unavailable",
			entityName,
		)
	}
	return col, nil
}

// buildInsert builds the INSERT for the given bound columns plus the managed
// NOW() columns (created_at/updated_at when enabled), returning the PK column.
func buildInsert(table string, fields domain.Fields, pk string, nowCols []string) (string, []any) {
	keys := sortedKeys(fields)
	cols := make([]string, 0, len(keys)+len(nowCols))
	params := make([]string, 0, len(keys)+len(nowCols))
	vals := make([]any, 0, len(keys))
	for i, k := range keys {
		cols = append(cols, k)
		params = append(params, fmt.Sprintf("$%d", i+1))
		vals = append(vals, fields[k])
	}
	for _, nc := range nowCols {
		cols = append(cols, validIdentifier(nc))
		params = append(params, "NOW()")
	}
	sql := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) RETURNING %s",
		validIdentifier(table),
		strings.Join(cols, ", "),
		strings.Join(params, ", "),
		validIdentifier(pk),
	)
	return sql, vals
}

// buildUpdate builds the UPDATE for the given bound columns plus the managed
// NOW() columns (updated_at when enabled), keyed on the PK column.
func buildUpdate(table, pk, id string, fields domain.Fields, nowCols []string) (string, []any) {
	keys := sortedKeys(fields)
	sets := make([]string, 0, len(keys)+len(nowCols))
	vals := make([]any, 0, len(keys)+1)
	for i, k := range keys {
		sets = append(sets, fmt.Sprintf("%s = $%d", k, i+1))
		vals = append(vals, fields[k])
	}
	for _, nc := range nowCols {
		sets = append(sets, validIdentifier(nc)+" = NOW()")
	}
	idParam := len(keys) + 1
	vals = append(vals, id)
	sql := fmt.Sprintf(
		"UPDATE %s SET %s WHERE %s = $%d RETURNING %s",
		validIdentifier(table),
		strings.Join(sets, ", "),
		validIdentifier(pk),
		idParam,
		validIdentifier(pk),
	)
	return sql, vals
}

// The column→value map for INSERT/UPDATE comes from TableSchema.writeFields,
// which reads only the schema's declared fields (Go field → column) — no
// reflection-by-convention, no transient tag, no overrides map.

func sortedKeys(fields domain.Fields) []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
