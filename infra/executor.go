package infra

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/jackc/pgx/v5"
)

// Phase 19 + audit: Postgres.Insert/Update/Archive/Delete/Unarchive receive
// the request-scoped domain.Context (carries actor + threadId + cancellation)
// and an optional RepoConfig (convention overrides). The simple path here
// dispatches to the aggregate variant when AggregateInfo() reports true.
//
// Audit hooks per method:
//   - When the database destination is active, audit.InsertAuditEvent runs
//     INSIDE the TX so the audit row is atomic with the data row + outbox row.
//   - When the slog destination is active, EchoSlog runs POST-COMMIT so a
//     transient slog handler error never blocks the persistence path.
// Both branches share a single audit.AuditEvent built once after the data
// row is materialized (Insert needs the generated id).

func (p *Postgres) Insert(ctx domain.Context, entity domain.Insertable, cfg *RepoConfig, hook writeHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.insertAggregate(ctx, entity, cfg, hook)
	}

	src := entity.Source()
	table := resolveTable(src, cfg)
	fields := resolveFields(src, cfg)
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

	sql, args := buildInsert(table, fields)
	var id string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeOutbox(ctx, tx, table, "INSERTED", id, fields); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildInsertEvent(ctx, entity, domain.NewID(id), p.auditClaims)
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

func (p *Postgres) Update(ctx domain.Context, entity domain.Updatable, cfg *RepoConfig, hook writeHook) (domain.WriteResult, error) {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.updateAggregate(ctx, entity, cfg, hook)
	}

	src := entity.Source()
	table := resolveTable(src, cfg)
	fields := resolveFields(src, cfg)
	hctx := hookContext{verb: "Update", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildUpdate(table, entity.ID(), fields)
	var id string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := writeOutbox(ctx, tx, table, "UPDATED", id, fields); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildUpdateEvent(ctx, entity, p.auditClaims)
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

func (p *Postgres) Archive(ctx domain.Context, entity domain.Archivable, cfg *RepoConfig, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.archiveAggregate(ctx, entity, cfg, hook)
	}

	src := entity.Source()
	table := resolveTable(src, cfg)
	hctx := hookContext{verb: "Archive", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("UPDATE %s SET deleted_at = NOW() WHERE id = $1", validIdentifier(table))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "ARCHIVED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildArchiveEvent(ctx, entity, p.auditClaims)
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

func (p *Postgres) Unarchive(ctx domain.Context, entity domain.Unarchivable, cfg *RepoConfig, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.unarchiveAggregate(ctx, entity, cfg, hook)
	}

	src := entity.Source()
	table := resolveTable(src, cfg)
	hctx := hookContext{verb: "Unarchive", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("UPDATE %s SET deleted_at = NULL WHERE id = $1", validIdentifier(table))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "UNARCHIVED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildUnarchiveEvent(ctx, entity, p.auditClaims)
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

func (p *Postgres) Delete(ctx domain.Context, entity domain.Deletable, cfg *RepoConfig, hook writeHook) error {
	if _, isAggregate := entity.AggregateInfo(); isAggregate {
		return p.deleteAggregate(ctx, entity, cfg, hook)
	}

	src := entity.Source()
	table := resolveTable(src, cfg)
	hctx := hookContext{verb: "Delete", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("DELETE FROM %s WHERE id = $1", validIdentifier(table))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, table, "DELETED", entity.ID(), nil); err != nil {
		return err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildDeleteEvent(ctx, entity, p.auditClaims)
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

// Batch executes a set of operations in a single TX. Each op resolves
// table/fields via inference (no cfg — batch is the simple path). Audit
// emission for Batch members is intentionally skipped today; the helper
// has no public surface in the framework's Repository interface and the
// rollout focuses on the per-verb write methods.
func (p *Postgres) Batch(ctx context.Context, entity domain.Batch) ([]domain.WriteResult, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	results := make([]domain.WriteResult, 0, len(entity.Operations()))
	for _, op := range entity.Operations() {
		wr, err := execWithTx(ctx, tx, op, nil)
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

func execWithTx(ctx context.Context, tx pgx.Tx, entity domain.ValidEntity, cfg *RepoConfig) (domain.WriteResult, error) {
	switch e := entity.(type) {
	case domain.Insertable:
		table := resolveTable(e.Source(), cfg)
		fields := resolveFields(e.Source(), cfg)
		sql, args := buildInsert(table, fields)
		var id string
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, table, "INSERTED", id, fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: id, Fields: fields}, nil

	case domain.Updatable:
		table := resolveTable(e.Source(), cfg)
		fields := resolveFields(e.Source(), cfg)
		sql, args := buildUpdate(table, e.ID(), fields)
		var id string
		if err := tx.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, table, "UPDATED", id, fields); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: id, Fields: fields}, nil

	case domain.Archivable:
		table := resolveTable(e.Source(), cfg)
		q := fmt.Sprintf("UPDATE %s SET deleted_at = NOW() WHERE id = $1", validIdentifier(table))
		if _, err := tx.Exec(ctx, q, e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, table, "ARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Unarchivable:
		table := resolveTable(e.Source(), cfg)
		q := fmt.Sprintf("UPDATE %s SET deleted_at = NULL WHERE id = $1", validIdentifier(table))
		if _, err := tx.Exec(ctx, q, e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, table, "UNARCHIVED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil

	case domain.Deletable:
		table := resolveTable(e.Source(), cfg)
		q := fmt.Sprintf("DELETE FROM %s WHERE id = $1", validIdentifier(table))
		if _, err := tx.Exec(ctx, q, e.ID()); err != nil {
			return domain.WriteResult{}, err
		}
		if err := writeOutbox(ctx, tx, table, "DELETED", e.ID(), nil); err != nil {
			return domain.WriteResult{}, err
		}
		return domain.WriteResult{ID: e.ID()}, nil
	}
	return domain.WriteResult{}, fmt.Errorf("infra: unknown entity type %T", entity)
}

// resolveTable: cfg.Table takes priority; otherwise InferTableName(typeof(e)).
func resolveTable(e domain.Entity, cfg *RepoConfig) string {
	if cfg != nil && cfg.Table != "" {
		return cfg.Table
	}
	return InferTableName(reflect.TypeOf(e))
}

// resolveFields: uses cfg.FieldOverrides (if any) to map GoField→column.
func resolveFields(e domain.Entity, cfg *RepoConfig) domain.Fields {
	var overrides map[string]string
	if cfg != nil {
		overrides = cfg.FieldOverrides
	}
	return FieldsFromEntity(e, overrides)
}

func buildInsert(table string, fields domain.Fields) (string, []any) {
	keys := sortedKeys(fields)
	cols := make([]string, len(keys))
	params := make([]string, len(keys))
	vals := make([]any, len(keys))
	for i, k := range keys {
		cols[i] = k
		params[i] = fmt.Sprintf("$%d", i+1)
		vals[i] = fields[k]
	}
	sql := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s) RETURNING id",
		validIdentifier(table),
		strings.Join(cols, ", "),
		strings.Join(params, ", "),
	)
	return sql, vals
}

func buildUpdate(table, id string, fields domain.Fields) (string, []any) {
	keys := sortedKeys(fields)
	sets := make([]string, len(keys))
	vals := make([]any, len(keys)+1)
	for i, k := range keys {
		sets[i] = fmt.Sprintf("%s = $%d", k, i+1)
		vals[i] = fields[k]
	}
	vals[len(keys)] = id
	sql := fmt.Sprintf(
		"UPDATE %s SET %s, updated_at = NOW() WHERE id = $%d RETURNING id",
		validIdentifier(table),
		strings.Join(sets, ", "),
		len(keys)+1,
	)
	return sql, vals
}

// FieldsFromEntity is the canonical helper that returns map[column]value
// via reflection on the struct (skips anonymous embeds, unexported fields,
// tag `transient:"-"`, field "ID"). Overrides applied via
// fieldOverrides[goFieldName] = columnName.
// (defined in infer.go)

func sortedKeys(fields domain.Fields) []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
