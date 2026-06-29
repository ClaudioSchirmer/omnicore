package write

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// SharedBase (Modelagem 2 / Party-Role) write path. A ROLE schema that declares
// .SharedBase(base, fkCol) is persisted across the shared identity table (the
// base, e.g. pessoa) and its own role table (e.g. aluno), linked by an FK to the
// base's DETERMINISTIC id (UUIDv5 of the natural-key value). Two levels of
// existence (design §8.3/§8.4):
//
//   - base identity: id = UUIDv5(naturalKey) → UPSERT (insert-or-update shared
//     fields, last-write-wins). No read-back: app and infra derive the same id.
//   - role: UNIQUE(fk) → 0..1 per identity. On INSERT, an existing ACTIVE role is
//     a 409; an ARCHIVED role is revived; otherwise a new role row is inserted.
//
// The base has no soft-delete; the role controls its own. Hard delete + refcount
// live in hardDelete (the role path) — see deleteRoleBaseRefcount.
//
// Covers the FLAT role path (a role is typically a simple entity). All SQL is
// dialect-agnostic via the Dialect.

// sharedBaseNamespace is the fixed UUIDv5 namespace for deriving a shared base's
// id from its natural-key value — stable across processes so app and infra agree.
var sharedBaseNamespace = uuid.MustParse("9b2e7c4a-1f6d-5a83-b0c1-d2e3f4a5b6c7")

// deterministicBaseID derives base.id = UUIDv5(namespace, naturalKeyValue).
func deterministicBaseID(naturalKeyValue string) string {
	return uuid.NewSHA1(sharedBaseNamespace, []byte(naturalKeyValue)).String()
}

// sharedBaseValues reads the base's column→value map AND the natural-key value
// from the role entity. The base is type-less, so values are read by Go field
// name (the role carries every shared field, validated at .SharedBase).
func sharedBaseValues(base *TableSchema, src domain.Entity) (domain.Fields, string) {
	rv := reflect.ValueOf(src)
	for rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	fields := make(domain.Fields, len(base.GoFields()))
	nkGo, _ := base.GoOf(base.NaturalKeyColumn())
	var nk string
	for _, goName := range base.GoFields() {
		col, _ := base.ColumnOf(goName)
		val := rv.FieldByName(goName).Interface()
		fields[col] = val
		if goName == nkGo {
			nk = scalarString(val)
		}
	}
	return fields, nk
}

// scalarString renders a (possibly pointer) scalar as the string fed to the
// deterministic-id hash. A nil pointer yields "".
func scalarString(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	return fmt.Sprintf("%v", rv.Interface())
}

// roleState is the result of the role existence probe.
type roleState int

const (
	roleNone roleState = iota
	roleActive
	roleArchived
)

// findRoleByFK probes the role table for a row referencing the shared base id,
// returning the role's own id (canonical) + its lifecycle state. UNIQUE(fk)
// guarantees 0..1, so a single LIMIT 1 row decides it.
func (b *BaseEngine) findRoleByFK(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, fkCol, baseID string) (string, roleState, error) {
	sd, hasSD := schema.SoftDeleteColumn()
	cols := d.QuoteIdent(schema.PKColumn())
	if hasSD {
		cols += ", " + d.QuoteIdent(sd)
	}
	q := "SELECT " + cols + " FROM " + d.QuoteIdent(schema.Table()) +
		" WHERE " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1) + " LIMIT 1"
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return "", roleNone, err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", roleNone, rows.Err()
	}
	var keyRaw string
	state := roleActive
	if hasSD {
		var sdRaw []byte
		if err := rows.Scan(&keyRaw, &sdRaw); err != nil {
			return "", roleNone, err
		}
		if sdRaw != nil {
			state = roleArchived
		}
	} else if err := rows.Scan(&keyRaw); err != nil {
		return "", roleNone, err
	}
	id, err := d.DecodeID(keyRaw)
	if err != nil {
		return "", roleNone, err
	}
	return id, state, rows.Err()
}

// insertWithBase is the role INSERT (POST): UPSERT the shared identity, apply the
// two-level existence matrix on the role row, then — when the role is an
// aggregate — its children and siblings. Covers both the flat and the aggregate
// role (root is nil for a flat entity, so insertChildren/insertSiblings no-op).
func (b *BaseEngine) insertWithBase(ctx persistence.RequestContext, entity domain.Insertable, schema *TableSchema, hook WriteHook, base *TableSchema, fkCol string) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	roleFields := schema.WriteFields(src)
	baseFields, nk := sharedBaseValues(base, src)
	baseID := deterministicBaseID(nk)
	roleFields[fkCol] = domain.NewID(baseID)

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
	if err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields); err != nil {
		return domain.WriteResult{}, err
	}
	roleID, state, err := b.findRoleByFK(ctx, tx, d, schema, fkCol, baseID)
	if err != nil {
		return domain.WriteResult{}, err
	}

	var id string
	switch state {
	case roleActive:
		// Already a (live) role for this identity — POST is a conflict.
		return domain.WriteResult{}, SingleNotificationError(entity.EntityName(), schema.PKColumn(), domain.EntityAlreadyAddedNotification{})
	case roleArchived:
		id = roleID
		if err := b.reviveRole(ctx, tx, d, schema, fkCol, baseID, roleFields); err != nil {
			return domain.WriteResult{}, err
		}
	default: // roleNone
		nid, err := newWriteID()
		if err != nil {
			return domain.WriteResult{}, err
		}
		id = nid
		sql, args := buildInsert(d, schema.Table(), schema.PKColumn(), id, roleFields, schema.InsertNowColumns())
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return domain.WriteResult{}, err
		}
	}
	// Aggregate role: children + siblings under the established role id.
	if err := insertChildren(ctx, tx, d, root, schema, id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id, roleFields); err != nil {
		return domain.WriteResult{}, err
	}
	// Fan-out trigger: a base outbox row recomposes the OTHER roles' read models
	// of this identity (SyncEngine.fanOutSharedBase).
	if err := WriteOutbox(ctx, tx, base.Table(), "UPDATED", baseID, nil); err != nil {
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
	return domain.WriteResult{ID: id, Fields: roleFields}, nil
}

// updateWithBase is the role UPDATE (PUT/PATCH): UPDATE the role row by PK, apply
// aggregate child changes + sibling updates, and UPSERT the shared identity
// (shared fields last-write-wins). The natural-key value is immutable — it
// derives the id, so the UPSERT keys on the derived id and never rewrites the key.
func (b *BaseEngine) updateWithBase(ctx persistence.RequestContext, entity domain.Updatable, schema *TableSchema, hook WriteHook, base *TableSchema, fkCol string) (domain.WriteResult, error) {
	root, _ := entity.AggregateInfo()
	src := entity.Source()
	roleFields := schema.WriteFields(src)
	baseFields, nk := sharedBaseValues(base, src)
	baseID := deterministicBaseID(nk)

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
	sql, args := buildUpdate(d, schema.Table(), schema.PKColumn(), entity.ID(), roleFields, schema.UpdateNowColumns())
	if err := execExpectingRow(ctx, tx, sql, args, entity.EntityName(), schema.PKColumn(), entity.ID()); err != nil {
		return domain.WriteResult{}, err
	}
	// Aggregate role: child Added/Changed/Removed + sibling updates.
	if err := applyChildChanges(ctx, tx, d, root, schema, entity.ID()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields); err != nil {
		return domain.WriteResult{}, err
	}
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID(), roleFields); err != nil {
		return domain.WriteResult{}, err
	}
	// Fan-out trigger: a base outbox row recomposes the OTHER roles' read models.
	if err := WriteOutbox(ctx, tx, base.Table(), "UPDATED", baseID, nil); err != nil {
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
	return domain.WriteResult{ID: entity.ID(), Fields: roleFields}, nil
}

// refcountSharedBase, after a role row is hard-deleted, removes the shared base
// row if no role still references it (OrphanPolicy DeleteWhenUnreferenced). The
// base id is derived from the (loaded) entity's natural-key value. Roles to
// count come from the base's registry (every role that declared .SharedBase). A
// no-op for KeepOrphan or a role without a shared base. The just-deleted role
// row is already gone, so it never counts itself.
func (b *BaseEngine) refcountSharedBase(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity) error {
	base, _, ok := schema.SharedBaseRef()
	if !ok || base.OrphanPolicyValue() != DeleteWhenUnreferenced {
		return nil
	}
	_, nk := sharedBaseValues(base, src)
	baseID := deterministicBaseID(nk)
	for _, rr := range base.ReferencingRoles() {
		q := "SELECT 1 FROM " + d.QuoteIdent(rr.Table) +
			" WHERE " + d.QuoteIdent(rr.FKColumn) + " = " + d.Placeholder(1) + " LIMIT 1"
		rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
		if err != nil {
			return err
		}
		referenced := rows.Next()
		errClose := rows.Err()
		rows.Close()
		if errClose != nil {
			return errClose
		}
		if referenced {
			return nil // still referenced — keep the base
		}
	}
	return tx.Exec(ctx, deleteSQL(d, base.Table(), base.PKColumn()), d.EncodeArg(domain.NewID(baseID)))
}

// upsertSharedBase INSERTs the identity or updates its shared fields on conflict
// (last-write-wins), keyed on the base's deterministic id.
func (b *BaseEngine) upsertSharedBase(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string, baseFields domain.Fields) error {
	sql, args := buildSiblingUpsert(d, base, base.PKColumn(), baseID, baseFields)
	return tx.Exec(ctx, sql, args...)
}

// reviveRole clears the role's soft-delete and rewrites its business fields,
// keyed on the FK to the identity. The injected FK is excluded from the SET (it
// is the key, unchanged).
func (b *BaseEngine) reviveRole(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, fkCol, baseID string, roleFields domain.Fields) error {
	sd, ok := schema.SoftDeleteColumn()
	if !ok {
		return fmt.Errorf("db: role %q cannot revive without a SoftDelete column", schema.Table())
	}
	biz := make(domain.Fields, len(roleFields))
	for k, v := range roleFields {
		if k == fkCol {
			continue
		}
		biz[k] = v
	}
	sets := []string{d.QuoteIdent(sd) + " = NULL"}
	args := make([]any, 0, len(biz)+1)
	n := 0
	for _, k := range SortedKeys(biz) {
		n++
		sets = append(sets, d.QuoteIdent(k)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(biz[k]))
	}
	for _, nc := range schema.UpdateNowColumns() {
		sets = append(sets, d.QuoteIdent(nc)+" = NOW()")
	}
	n++
	sql := "UPDATE " + d.QuoteIdent(schema.Table()) + " SET " + strings.Join(sets, ", ") +
		" WHERE " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(n)
	args = append(args, d.EncodeArg(domain.NewID(baseID)))
	return tx.Exec(ctx, sql, args...)
}
