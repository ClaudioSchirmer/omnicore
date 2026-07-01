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

// requireNaturalKey guards the one prerequisite the framework cannot enforce in
// DDL: a non-empty natural key. An empty value hashes to a CONSTANT id, collapsing
// every key-less identity into a single base row — a silent, corrupting footgun.
// The UNIQUE(naturalKey) column constraint is otherwise self-enforced by the
// deterministic id PK (equal keys → equal id → ON CONFLICT). Fails loudly instead.
func requireNaturalKey(base *TableSchema, nk string) error {
	if nk == "" {
		return fmt.Errorf(
			"db: shared base %q natural key (column %q) resolved empty — it must be NOT NULL / non-empty: "+
				"its value derives the deterministic identity, and an empty key collapses every record into one",
			base.Table(), base.NaturalKeyColumn())
	}
	return nil
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
		// Project the archived state as a boolean — scanning a non-null timestamp
		// into a []byte fails under Postgres' binary protocol, and this probe hits
		// exactly that case for an archived role (the revive path).
		cols += ", " + d.QuoteIdent(sd) + " IS NOT NULL"
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
		var archived bool
		if err := rows.Scan(&keyRaw, &archived); err != nil {
			return "", roleNone, err
		}
		if archived {
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
	if err := requireNaturalKey(base, nk); err != nil {
		return domain.WriteResult{}, err
	}
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
	// Forgot-guard (manual path): the identity already exists but this insert did
	// NOT go through the SharedBase upsert (its actionName is not the upsert one) —
	// a blind insert that would duplicate the shared identity's native data. The
	// Auto handlers enforce the marriage by capability; this catches a hand-rolled
	// manual handler that skipped repo.LoadForSharedBaseInsert.
	basePreExisted, err := baseExists(ctx, tx, d, base, baseID)
	if err != nil {
		return domain.WriteResult{}, err
	}
	if basePreExisted && entity.ActionName() != "GetUpsertable" {
		return domain.WriteResult{}, fmt.Errorf(
			"db: %s has a SharedBase and the identity already exists — load it first "+
				"(SharedBaseInsertCommandHandler, or repo.LoadForSharedBaseInsert in a manual handler) before "+
				"Insert; a blind insert would duplicate the shared identity's native data", entity.EntityName())
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
	// One pass (writeChildren) handles role children (FK→role id) and shared-base
	// native children (FK→base id) by the OperationOf categorization: a loaded
	// base-child arrives as Constructor → no-op (not re-inserted); only the request's
	// new ones insert (and re-added/changed update, removed delete) — the upsert.
	if err := writeChildren(ctx, tx, d, root, schema, id, baseID); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id); err != nil {
		return domain.WriteResult{}, err
	}
	// A new/revived active role means the shared identity must be active: if it was
	// archived (every prior role archived), reactivate it + its native children.
	if err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
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
	if err := requireNaturalKey(base, nk); err != nil {
		return domain.WriteResult{}, err
	}
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
	// Aggregate role: role + shared-base children, persisted by OperationOf
	// (writeChildren) + sibling updates. Update is load-first, so loaded children are
	// Constructor (no-op) and only real changes touch the DB.
	if err := writeChildren(ctx, tx, d, root, schema, entity.ID(), baseID); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	if err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields); err != nil {
		return domain.WriteResult{}, err
	}
	// An updated role is active → keep the shared identity active (reactivate if a
	// prior all-archived state left it archived). Idempotent when already active.
	if err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
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
	// No role references the base — remove its native children (FK = base id),
	// then the base row itself, explicitly in Go (same TX, C7), so the base's
	// children never outlive the base they belong to.
	for _, bc := range base.ChildSchemas() {
		if err := tx.Exec(ctx, childDeleteSQL(d, bc.Table(), bc.FKColumn()), d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	}
	return tx.Exec(ctx, deleteSQL(d, base.Table(), base.PKColumn()), d.EncodeArg(domain.NewID(baseID)))
}

// --- unified lifecycle convergence -------------------------------------------
//
// The shared base is a mini-root of its native children: it has soft-delete and
// its lifecycle is DRIVEN by its roles. The single rule, per verb:
//   - a role becomes/stays active (insert / update / unarchive) → the base must be
//     active: reactivateBaseIfArchived.
//   - a role is archived (archive) → if no role is left active, the base archives:
//     archiveBaseIfNoActiveRole.
//   - a role is hard-deleted (delete) → if no role ROW remains, the base is removed
//     per OrphanPolicy: refcountSharedBase (the hard branch).
// All three no-op without a shared base; the first two also no-op when the base
// has no soft-delete (then only the orphan branch applies). They read the base /
// role state and only write on a real transition (idempotent), so a steady-state
// write costs at most one probing SELECT and no redundant UPDATE.

// reactivateBaseIfArchived un-archives the base + its native children when a role
// is/becomes active and the base was archived (the revive direction).
func (b *BaseEngine) reactivateBaseIfArchived(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity) error {
	base, sd, baseID, ok, err := baseLifecycleTarget(schema, src)
	if !ok || err != nil {
		return err
	}
	archived, err := baseIsArchived(ctx, tx, d, base, sd, baseID)
	if err != nil || !archived {
		return err
	}
	return cascadeBaseLifecycle(ctx, tx, d, base, sd, baseID, "NULL", " IS NOT NULL")
}

// archiveBaseIfNoActiveRole archives the base + its native children once the role
// just archived leaves NO active role referencing the base.
func (b *BaseEngine) archiveBaseIfNoActiveRole(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity) error {
	base, sd, baseID, ok, err := baseLifecycleTarget(schema, src)
	if !ok || err != nil {
		return err
	}
	active, err := anyActiveRole(ctx, tx, d, base, baseID)
	if err != nil || active {
		return err
	}
	return cascadeBaseLifecycle(ctx, tx, d, base, sd, baseID, "NOW()", " IS NULL")
}

// convergeBaseAfterSoftWrite routes a role's archive/unarchive to the matching
// base lifecycle step (shared by the flat + aggregate soft-write paths).
func (b *BaseEngine) convergeBaseAfterSoftWrite(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, eventType string) error {
	switch eventType {
	case "ARCHIVED":
		return b.archiveBaseIfNoActiveRole(ctx, tx, d, schema, src)
	case "UNARCHIVED":
		return b.reactivateBaseIfArchived(ctx, tx, d, schema, src)
	}
	return nil
}

// baseLifecycleTarget resolves the shared base + its soft-delete column + the
// deterministic id from the role schema and entity, reporting ok=false (skip)
// when there is no shared base or the base has no soft-delete (lifecycle is then
// hard-only, governed by the orphan refcount).
func baseLifecycleTarget(schema *TableSchema, src domain.Entity) (base *TableSchema, sd, baseID string, ok bool, err error) {
	base, _, has := schema.SharedBaseRef()
	if !has {
		return nil, "", "", false, nil
	}
	sd, hasSD := base.SoftDeleteColumn()
	if !hasSD {
		return nil, "", "", false, nil
	}
	_, nk := sharedBaseValues(base, src)
	if err := requireNaturalKey(base, nk); err != nil {
		return nil, "", "", false, err
	}
	return base, sd, deterministicBaseID(nk), true, nil
}

// cascadeBaseLifecycle archives (NOW()/" IS NULL") or unarchives (NULL/" IS NOT
// NULL") the base row and each soft-deletable native child, gated so it is
// idempotent (a no-op when already in the target state).
func cascadeBaseLifecycle(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID, setExpr, gate string) error {
	if err := tx.Exec(ctx, childCascadeSQL(d, base.Table(), sd, base.PKColumn(), setExpr, gate), d.EncodeArg(domain.NewID(baseID))); err != nil {
		return err
	}
	for _, bc := range base.ChildSchemas() {
		csd, ok := bc.SoftDeleteColumn()
		if !ok {
			continue
		}
		if err := tx.Exec(ctx, childCascadeSQL(d, bc.Table(), csd, bc.FKColumn(), setExpr, gate), d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	}
	return nil
}

// baseIsArchived reports whether the base row currently carries a non-null
// soft-delete marker (read once, for idempotency).
func baseIsArchived(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID string) (bool, error) {
	// Project the archived state as a boolean rather than scanning the raw
	// soft-delete timestamp: a non-null timestamp cannot be scanned into a
	// []byte under Postgres' binary protocol (it works only while NULL), which is
	// exactly the case reactivateBaseIfArchived probes for. IS NOT NULL scans
	// cleanly into a bool on every dialect.
	q := "SELECT " + d.QuoteIdent(sd) + " IS NOT NULL FROM " + d.QuoteIdent(base.Table()) +
		" WHERE " + d.QuoteIdent(base.PKColumn()) + " = " + d.Placeholder(1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var archived bool
	if err := rows.Scan(&archived); err != nil {
		return false, err
	}
	return archived, rows.Err()
}

// anyActiveRole reports whether any role row referencing the base is ACTIVE (not
// soft-deleted). A role without a soft-delete column has no archived state, so
// every existing row counts as active.
func anyActiveRole(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	for _, rr := range base.ReferencingRoles() {
		q := "SELECT 1 FROM " + d.QuoteIdent(rr.Table) + " WHERE " + d.QuoteIdent(rr.FKColumn) + " = " + d.Placeholder(1)
		if rr.SoftDeleteCol != "" {
			q += " AND " + d.QuoteIdent(rr.SoftDeleteCol) + " IS NULL"
		}
		q += " LIMIT 1"
		rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
		if err != nil {
			return false, err
		}
		active := rows.Next()
		cerr := rows.Err()
		rows.Close()
		if cerr != nil {
			return false, cerr
		}
		if active {
			return true, nil
		}
	}
	return false, nil
}

// baseExists probes whether the shared base row already exists (the identity
// pre-dates this write) — the signal the SharedBase insert forgot-guard pairs with
// the actionName.
func baseExists(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	q := "SELECT 1 FROM " + d.QuoteIdent(base.Table()) +
		" WHERE " + d.QuoteIdent(base.PKColumn()) + " = " + d.Placeholder(1) + " LIMIT 1"
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	exists := rows.Next()
	return exists, rows.Err()
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
