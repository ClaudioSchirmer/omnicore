package infra

import (
	"context"
	"fmt"
	"reflect"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/jackc/pgx/v5"
)

// Phase 19 + audit: aggregate-aware persistence consumes the request
// persistence.RequestContext (audit + cancellation) plus the convention-overrides
// RepoConfig. Each method threads ctx through pgx and emits the audit
// event in lockstep with the data write (in-TX database INSERT + post-
// commit slog echo) — same pattern the simple-path methods follow in
// executor.go.
//
// Guarantees (preserved):
//   - Single pgx.Tx for root + all children
//   - Exactly one outbox row per call (granularity B)
//   - Exactly one audit_events row per call when database destination
//     active (same granularity B; the AuditEvent's children block carries
//     the per-VO cascade)
//   - FK injected from root id before the child INSERT
//   - Status iteration: Added→INSERT; Changed→UPDATE; Removed→Archive (symmetric
//     universal cascade); Constructor→no-op (update) or INSERT (insert).
//   - Archive of root cascades Archive on all active children
//   - Unarchive of root restores all archived children
//   - Hard Delete of root relies on FK ON DELETE CASCADE in the schema

func (p *Postgres) insertAggregate(ctx persistence.RequestContext, entity domain.Insertable, cfg *RepoConfig, hook writeHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := resolveTable(src, cfg)
	rootFields := resolveFields(src, cfg)
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

	sql, args := buildInsert(rootTable, rootFields)
	var rootID string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&rootID); err != nil {
		return domain.WriteResult{}, err
	}

	rootType := reflect.TypeOf(src)
	if err := insertChildren(ctx, tx, root, rootType, cfg, rootID); err != nil {
		return domain.WriteResult{}, err
	}

	payload := buildAggregatePayload(rootFields, root, cfg)
	if err := writeOutbox(ctx, tx, rootTable, "INSERTED", rootID, payload); err != nil {
		return domain.WriteResult{}, err
	}

	var ev *audit.AuditEvent
	if p.auditEnabled() {
		built := BuildInsertEvent(ctx, entity, domain.NewID(rootID), p.auditClaims)
		ev = &built
	}
	if err := p.writeAuditRow(ctx, tx, ev); err != nil {
		return domain.WriteResult{}, err
	}

	// Position D — AFTER root + children + outbox + audit, BEFORE COMMIT.
	// Single firing per orch.Method() call (granularity B); the hook
	// receives the root entity with all its aggregate children available
	// via the usual helpers.
	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(rootID), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	return domain.WriteResult{ID: rootID, Fields: rootFields}, nil
}

func (p *Postgres) updateAggregate(ctx persistence.RequestContext, entity domain.Updatable, cfg *RepoConfig, hook writeHook) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := resolveTable(src, cfg)
	rootFields := resolveFields(src, cfg)
	hctx := hookContext{verb: "Update", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return domain.WriteResult{}, err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	sql, args := buildUpdate(rootTable, entity.ID(), rootFields)
	var rootID string
	if err := tx.QueryRow(ctx, sql, args...).Scan(&rootID); err != nil {
		return domain.WriteResult{}, err
	}

	rootType := reflect.TypeOf(src)
	if err := applyChildChanges(ctx, tx, root, rootType, cfg, rootID); err != nil {
		return domain.WriteResult{}, err
	}

	payload := buildAggregatePayload(rootFields, root, cfg)
	if err := writeOutbox(ctx, tx, rootTable, "UPDATED", rootID, payload); err != nil {
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

	if err := p.fireBeforeCommit(ctx, tx, src, domain.NewID(rootID), hook, hctx); err != nil {
		return domain.WriteResult{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.WriteResult{}, err
	}
	p.echoAuditSlog(ctx, ev)
	return domain.WriteResult{ID: rootID, Fields: rootFields}, nil
}

func (p *Postgres) archiveAggregate(ctx persistence.RequestContext, entity domain.Archivable, cfg *RepoConfig, hook writeHook) error {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := resolveTable(src, cfg)
	rootType := reflect.TypeOf(src)
	hctx := hookContext{verb: "Archive", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("UPDATE %s SET deleted_at = NOW() WHERE id = $1", validIdentifier(rootTable))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}

	// Cascade Archive: iterates typeNames discovered in the AggregateRoot.
	if root != nil {
		for typeName := range root.AllAggregateItems() {
			childTable := resolveChildTable(typeName, cfg)
			fkCol := resolveChildFK(rootType, typeName, cfg)
			cq := fmt.Sprintf(
				"UPDATE %s SET deleted_at = NOW() WHERE %s = $1 AND deleted_at IS NULL",
				validIdentifier(childTable), validIdentifier(fkCol),
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

func (p *Postgres) deleteAggregate(ctx persistence.RequestContext, entity domain.Deletable, cfg *RepoConfig, hook writeHook) error {
	// Children removed via FK ON DELETE CASCADE in the schema. Only DELETE on the root.
	src := entity.Source()
	rootTable := resolveTable(src, cfg)
	hctx := hookContext{verb: "Delete", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("DELETE FROM %s WHERE id = $1", validIdentifier(rootTable))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}
	if err := writeOutbox(ctx, tx, rootTable, "DELETED", entity.ID(), nil); err != nil {
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

func (p *Postgres) unarchiveAggregate(ctx persistence.RequestContext, entity domain.Unarchivable, cfg *RepoConfig, hook writeHook) error {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	rootTable := resolveTable(src, cfg)
	rootType := reflect.TypeOf(src)
	hctx := hookContext{verb: "Unarchive", entityType: entity.EntityName()}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := p.fireAfterBegin(ctx, tx, src, hook, hctx); err != nil {
		return err
	}

	q := fmt.Sprintf("UPDATE %s SET deleted_at = NULL WHERE id = $1", validIdentifier(rootTable))
	if _, err := tx.Exec(ctx, q, entity.ID()); err != nil {
		return err
	}

	// Cascade Unarchive.
	if root != nil {
		for typeName := range root.AllAggregateItems() {
			childTable := resolveChildTable(typeName, cfg)
			fkCol := resolveChildFK(rootType, typeName, cfg)
			cq := fmt.Sprintf(
				"UPDATE %s SET deleted_at = NULL WHERE %s = $1 AND deleted_at IS NOT NULL",
				validIdentifier(childTable), validIdentifier(fkCol),
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

// insertChildren INSERTs items with status Added or Constructor for each
// typeName discovered in the AggregateRoot. FK injected from rootID.
func insertChildren(ctx context.Context, tx pgx.Tx, root *domain.AggregateRoot, rootType reflect.Type, cfg *RepoConfig, rootID string) error {
	if root == nil {
		return nil
	}
	for typeName, items := range root.AllAggregateItems() {
		childTable := resolveChildTable(typeName, cfg)
		fkCol := resolveChildFK(rootType, typeName, cfg)
		for _, it := range items {
			if it.CurrentStatus != domain.StatusAdded && it.CurrentStatus != domain.StatusConstructor {
				continue
			}
			if err := insertChild(ctx, tx, childTable, fkCol, cfg, it.Item, rootID); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyChildChanges processes Added/Changed/Removed items during aggregate
// update. Constructor items are no-op. Phase 19: symmetric universal cascade
// — Removed always performs Archive (UPDATE deleted_at); no HardDelete flag.
func applyChildChanges(ctx context.Context, tx pgx.Tx, root *domain.AggregateRoot, rootType reflect.Type, cfg *RepoConfig, rootID string) error {
	if root == nil {
		return nil
	}
	for typeName, items := range root.AllAggregateItems() {
		childTable := resolveChildTable(typeName, cfg)
		fkCol := resolveChildFK(rootType, typeName, cfg)
		for _, it := range items {
			switch it.CurrentStatus {
			case domain.StatusAdded:
				if err := insertChild(ctx, tx, childTable, fkCol, cfg, it.Item, rootID); err != nil {
					return err
				}
			case domain.StatusChanged:
				if err := updateChild(ctx, tx, childTable, cfg, it.Item); err != nil {
					return err
				}
			case domain.StatusRemoved:
				if err := archiveChild(ctx, tx, childTable, it.Item); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func insertChild(ctx context.Context, tx pgx.Tx, table, fkCol string, cfg *RepoConfig, item domain.AggregateValueObject, rootID string) error {
	var overrides map[string]string
	if cfg != nil {
		overrides = cfg.FieldOverrides
	}
	fields := FieldsFromEntity(item, overrides)
	fields[fkCol] = rootID
	sql, args := buildInsert(table, fields)
	var id string
	return tx.QueryRow(ctx, sql, args...).Scan(&id)
}

func updateChild(ctx context.Context, tx pgx.Tx, table string, cfg *RepoConfig, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("infra: cannot update child %q without id", table)
	}
	var overrides map[string]string
	if cfg != nil {
		overrides = cfg.FieldOverrides
	}
	fields := FieldsFromEntity(item, overrides)
	sql, args := buildUpdate(table, id, fields)
	var returned string
	return tx.QueryRow(ctx, sql, args...).Scan(&returned)
}

// Phase 19: Removed → always Archive (no per-child HardDelete).
func archiveChild(ctx context.Context, tx pgx.Tx, table string, item domain.AggregateValueObject) error {
	id := item.GetID()
	if id == "" {
		return fmt.Errorf("infra: cannot archive child %q without id", table)
	}
	q := fmt.Sprintf("UPDATE %s SET deleted_at = NOW() WHERE id = $1", validIdentifier(table))
	_, err := tx.Exec(ctx, q, id)
	return err
}

// buildAggregatePayload assembles the outbox JSON snapshot: root fields + active
// children grouped by typeName.
func buildAggregatePayload(rootFields domain.Fields, root *domain.AggregateRoot, cfg *RepoConfig) map[string]any {
	payload := map[string]any{
		"root": rootFields,
	}
	if root == nil {
		return payload
	}
	var overrides map[string]string
	if cfg != nil {
		overrides = cfg.FieldOverrides
	}
	children := map[string][]domain.Fields{}
	for typeName, items := range root.AllAggregateItems() {
		active := []domain.Fields{}
		for _, it := range items {
			if it.CurrentStatus == domain.StatusRemoved {
				continue
			}
			active = append(active, FieldsFromEntity(it.Item, overrides))
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

// resolveChildTable: cfg.ChildTableOverrides[typeName] takes priority; otherwise
// InferTableName by convention (PluralizeSnake + PascalToSnake of the typeName).
func resolveChildTable(typeName string, cfg *RepoConfig) string {
	if cfg != nil {
		if t, ok := cfg.ChildTableOverrides[typeName]; ok && t != "" {
			return t
		}
	}
	return domain.PluralizeSnake(domain.PascalToSnake(typeName))
}

// resolveChildFK: cfg.ChildFKOverrides[typeName] takes priority; otherwise
// InferForeignKey(rootType).
func resolveChildFK(rootType reflect.Type, typeName string, cfg *RepoConfig) string {
	if cfg != nil {
		if fk, ok := cfg.ChildFKOverrides[typeName]; ok && fk != "" {
			return fk
		}
	}
	return InferForeignKey(rootType)
}
