package write

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

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
//     fields, last-write-wins). The base is found (and reactivated) regardless of
//     its own soft-delete state — its lifecycle is DERIVED from the roles
//     (convergence below), and the KeepOrphan dormancy contract depends on the
//     natural key finding its way back. No read-back: app and infra derive the
//     same id.
//   - role: at most ONE ACTIVE row per identity per role table — the framework
//     invariant, enforced on INSERT (an existing ACTIVE role is a 409; an
//     ARCHIVED role is INVISIBLE to the probe — soft-delete is delete, here like
//     on every other read/write path — so the insert proceeds) and on UNARCHIVE
//     (reviving a remnant while another row of the same role table is active is
//     the same 409 — vetoUnarchiveWithActiveSibling). TOTAL row multiplicity is
//     the dev's DDL contract on the separate-FK model: a full UNIQUE(fk) index
//     caps the table at 0..1 rows per identity (an archived remnant physically
//     blocks a new insert — the repository's ConstraintBinding maps the
//     violation), while an active-only unique index (partial index on Postgres,
//     unique generated column on MySQL) admits archived remnants NEXT TO one
//     active row — the "keep the old archived role, open a fresh active one"
//     modeling. The shared-PK model has no choice: the PK itself caps the table
//     at one row per identity. Under concurrency the uniqueness index — not the
//     probe — is the arbiter; reviving an archived role is the explicit
//     /unarchive verb's job, never a POST side effect.
//
// The base's lifecycle is driven by its roles (unified lifecycle convergence
// below); a role hard-delete routes through convergeBaseAfterHardDelete — the
// orphan purge (OrphanPolicy DeleteWhenUnreferenced, database-vetoable) or the
// orphan archive (KeepOrphan + a soft-deletable base).
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
	for rv.Kind() == reflect.Pointer {
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
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return ""
		}
		rv = rv.Elem()
	}
	return fmt.Sprintf("%v", rv.Interface())
}

// findActiveRoleByFK probes the role table for an ACTIVE row referencing the
// shared base id. Soft-delete IS delete on this probe, exactly like every other
// read/write path: an archived role row is invisible here, so the caller's
// insert proceeds and the dev's DDL arbitrates the collision with any physical
// remnant (shared-PK → the primary key; separate FK → a full UNIQUE(fk) blocks
// it, an active-only unique index admits it next to the remnants). The probe
// asks "is any row active?", so LIMIT 1 decides it regardless of how many
// archived remnants the identity carries; when two inserts race, the
// uniqueness index — not this probe — is the arbiter.
func (b *BaseEngine) findActiveRoleByFK(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, fkCol, baseID string) (bool, error) {
	q := "SELECT 1 FROM " + d.QuoteIdent(schema.Table()) +
		" WHERE " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1)
	if sd, hasSD := schema.SoftDeleteColumn(); hasSD {
		q += " AND " + d.QuoteIdent(sd) + " IS NULL"
	}
	q = d.ApplyLimit(q, 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
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
	// A role either carries a SEPARATE FK column to the base, or shares the base's id
	// as its OWN primary key (fkCol == the role PK — a strict 1:1 where role.id ==
	// base.id). In the shared-PK model the id is written once, as the PK (buildInsert
	// prepends it), so injecting it as a field too would emit a duplicate column;
	// inject the FK only when it is a distinct column.
	sharedPK := fkCol == schema.PKColumn()
	if !sharedPK {
		roleFields[fkCol] = domain.NewID(baseID)
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
	baseRev, err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields, basePreExisted, now)
	if err != nil {
		return domain.WriteResult{}, err
	}
	active, err := b.findActiveRoleByFK(ctx, tx, d, schema, fkCol, baseID)
	if err != nil {
		return domain.WriteResult{}, err
	}
	if active {
		// Already a (live) role for this identity — POST is a conflict.
		return domain.WriteResult{}, SingleNotificationError(entity.EntityName(), schema.PKColumn(), domain.EntityAlreadyAddedNotification{})
	}
	// No ACTIVE role → insert. An ARCHIVED remnant is deliberately not looked
	// for (soft-delete is delete): if one exists, the schema's own constraints
	// veto or admit this insert — shared-PK collides on the primary key (the
	// repository's ConstraintBinding maps it), a separate-FK model decides
	// through its own unique index.
	var id string
	if sharedPK {
		// The role's own PK IS the deterministic base id (role.id == base.id).
		id = baseID
	} else {
		nid, err := newWriteID()
		if err != nil {
			return domain.WriteResult{}, err
		}
		id = nid
	}
	sql, args := buildInsert(d, schema.Table(), schema.PKColumn(), id, roleFields, schema.InsertNowColumns(), now, schema.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	// One pass (writeChildren) handles role children (FK→role id) and shared-base
	// native children (FK→base id) by the OperationOf categorization: a loaded
	// base-child arrives as Constructor → no-op (not re-inserted); only the request's
	// new ones insert (and re-added/changed update, removed delete) — the upsert.
	if err := writeChildren(ctx, tx, d, root, schema, id, baseID, now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := insertSiblings(ctx, tx, d, schema, src, id, now); err != nil {
		return domain.WriteResult{}, err
	}
	// A new active role means the shared identity must be active: if it was
	// archived (every prior role archived), reactivate it + its native children —
	// the base's AUTOMATIC revival (its lifecycle is derived; only ROLE revival
	// is out of the insert's scope).
	if err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
		return domain.WriteResult{}, err
	}
	// ONE outbox row per write: the v2 payload is self-sufficient (role ∪ base ∪
	// sibling fields, children with ops, structural ids + base revision), so the
	// SyncEngine fans out to the OTHER roles' read models from THIS event — the
	// historical empty base-table UPDATED row is no longer emitted.
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id,
		buildWritePayloadV2(schema, src, root, "INSERTED", now, roleFields,
			outboxMeta{ID: id, Revision: 1, BaseID: baseID, BaseRevision: baseRev})); err != nil {
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
	return domain.WriteResult{ID: domain.NewID(id), Fields: roleFields}, nil
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
	if err := guardNaturalKeyImmutable(ctx, tx, d, schema, base, entity.EntityName(), entity.ID().Value(), fkCol, baseID); err != nil {
		return domain.WriteResult{}, err
	}
	sql, args := buildUpdate(d, schema.Table(), schema.PKColumn(), entity.ID().Value(), roleFields, schema.UpdateNowColumns(), now, schema.RevisionColumn())
	if err := execExpectingRow(ctx, tx, sql, args, entity.EntityName(), schema.PKColumn(), entity.ID().Value()); err != nil {
		return domain.WriteResult{}, err
	}
	// Aggregate role: role + shared-base children, persisted by OperationOf
	// (writeChildren) + sibling updates. Update is load-first, so loaded children are
	// Constructor (no-op) and only real changes touch the DB.
	if err := writeChildren(ctx, tx, d, root, schema, entity.ID().Value(), baseID, now); err != nil {
		return domain.WriteResult{}, err
	}
	if err := applySiblingUpdates(ctx, tx, d, schema, src, entity.ID().Value(), entity.IsPartial()); err != nil {
		return domain.WriteResult{}, err
	}
	// The base always exists here (we are updating an existing role, whose FK
	// references it), so this is always the UPDATE branch.
	baseRev, err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields, true, now)
	if err != nil {
		return domain.WriteResult{}, err
	}
	// An updated role is active → keep the shared identity active (reactivate if a
	// prior all-archived state left it archived). Idempotent when already active.
	if err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
		return domain.WriteResult{}, err
	}
	ownRev := int64(0)
	if rc := schema.RevisionColumn(); rc != "" {
		if ownRev, err = readRevision(ctx, tx, d, schema.Table(), rc, schema.PKColumn(), entity.ID().Value()); err != nil {
			return domain.WriteResult{}, err
		}
	}
	// ONE outbox row per write (see insertWithBase): the v2 payload carries the
	// base id + revision, so the fan-out rides this event — no empty base row.
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID().Value(),
		buildWritePayloadV2(schema, src, root, "UPDATED", now, roleFields,
			outboxMeta{ID: entity.ID().Value(), Revision: ownRev, BaseID: baseID, BaseRevision: baseRev})); err != nil {
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
	return domain.WriteResult{ID: entity.ID(), Fields: roleFields}, nil
}

// guardNaturalKeyImmutable rejects a role UPDATE whose natural-key value
// diverges from the persisted identity. The natural key derives the
// deterministic base id, so every SharedBase derivation — the identity upsert,
// the refcount, the lifecycle convergence, the CDC fan-out, the payload FKs —
// assumes it never changes after insert; without this guard a request that
// mutated the key would upsert a DIFFERENT identity (last-write-wins over a
// third party's shared fields) while the role row keeps pointing at the old
// base. Shared-PK model: pure arithmetic — the role id IS UUIDv5(naturalKey),
// so the id derived from the request must equal the row's own id. Separate-FK
// model: one PK-indexed SELECT (inside the open TX) projecting the comparison
// as ANSI CASE 1/0 — the same dialect-safe form baseIsArchived uses (a bare
// boolean-valued `fk = ?` in a SELECT list is a PG/MySQL-ism; T-SQL would
// parse it as an alias assignment); a missing row skips the guard (the
// role UPDATE right after reports not-found exactly as before). The Old
// snapshot cannot arbitrate here: a manual handler that skipped load-first
// captures the request itself as Old, so an Old-vs-request comparison would be
// vacuous precisely in the case that needs guarding.
func guardNaturalKeyImmutable(ctx context.Context, tx WriteTx, d Dialect, schema, base *TableSchema, entityName, id, fkCol, baseID string) error {
	nkGo, _ := base.GoOf(base.NaturalKeyColumn())
	if fkCol == schema.PKColumn() {
		if id != baseID {
			return SingleNotificationError(entityName, nkGo, domain.NaturalKeyImmutableNotification{})
		}
		return nil
	}
	q := "SELECT CASE WHEN " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1) + " THEN 1 ELSE 0 END" +
		" FROM " + d.QuoteIdent(schema.Table()) +
		" WHERE " + d.QuoteIdent(schema.PKColumn()) + " = " + d.Placeholder(2)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)), d.EncodeArg(domain.NewID(id)))
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		return rows.Err() // row missing → the role UPDATE right after reports not-found
	}
	var matches int64
	if err := rows.Scan(&matches); err != nil {
		return err
	}
	if matches != 1 {
		return SingleNotificationError(entityName, nkGo, domain.NaturalKeyImmutableNotification{})
	}
	return rows.Err()
}

// sharedBasePurgeSavepoint names the savepoint the orphan purge runs under so a
// foreign-key veto rolls back just the purge, never the role delete around it.
const sharedBasePurgeSavepoint = "omnicore_sb_purge"

// convergeBaseAfterHardDelete, after a role row is hard-deleted, drives the
// shared identity to its post-delete state:
//
//   - OrphanPolicy DeleteWhenUnreferenced + no role row (of any type, active or
//     archived) still references the base → purge it (native children first,
//     then the base row, same TX). The purge is DATABASE-VETOABLE: it runs
//     under a savepoint, and a foreign-key violation from ANY referencing
//     table — including one outside the schema registry (another system
//     sharing the database) — keeps the base and lets the role delete commit.
//     An actual purge emits its own outbox row (base table, DELETED) and its
//     own audit event (built by buildPurgeEvent), so the identity's
//     destruction is never invisible; the returned bundle carries the event to
//     the caller's post-commit echo.
//   - otherwise (KeepOrphan, still referenced, or vetoed) → the standing
//     lifecycle convergence: with no ACTIVE role left, a soft-deletable base
//     archives (with its native children); without SoftDelete on the base,
//     nothing happens.
//
// A no-op for a role without a shared base, and — before any identity work —
// for a base with neither DeleteWhenUnreferenced nor SoftDelete (nothing could
// change). Roles to count come from the base's registry (every role that
// declared .SharedBase); the just-deleted role row is already gone, so it
// never counts itself.
func (b *BaseEngine) convergeBaseAfterHardDelete(
	ctx persistence.RequestContext,
	tx WriteTx,
	d Dialect,
	schema *TableSchema,
	src domain.Entity,
	now time.Time,
	buildPurgeEvent func(baseID string) audit.AuditEvent,
) (AuditBundle, bool, error) {
	base, _, ok := schema.SharedBaseRef()
	if !ok {
		return AuditBundle{}, false, nil
	}
	_, baseHasSD := base.SoftDeleteColumn()
	wantsPurge := base.OrphanPolicyValue() == DeleteWhenUnreferenced
	if !wantsPurge && !baseHasSD {
		return AuditBundle{}, false, nil // no lifecycle to drive
	}
	_, nk := sharedBaseValues(base, src)
	if err := requireNaturalKey(base, nk); err != nil {
		return AuditBundle{}, false, err
	}
	baseID := deterministicBaseID(nk)
	if wantsPurge {
		referenced, err := b.anyRoleRowReferences(ctx, tx, d, base, baseID)
		if err != nil {
			return AuditBundle{}, false, err
		}
		if !referenced {
			purged, err := b.purgeOrphanBase(ctx, tx, d, base, baseID)
			if err != nil {
				return AuditBundle{}, false, err
			}
			if purged {
				// The purge row carries _ids too: EVERY framework-produced event is
				// v2 (the consumer warns-and-skips anything without the block).
				if err := WriteOutbox(ctx, tx, base.Table(), "DELETED", baseID,
					domain.Fields{
						base.PKColumn(): domain.NewID(baseID),
						payloadKeyIDs:   outboxMeta{ID: baseID, BasePurged: true}.idsBlock(),
					}); err != nil {
					return AuditBundle{}, false, err
				}
				ab := b.BuildAudit(func() audit.AuditEvent { return buildPurgeEvent(baseID) }, nil)
				if err := b.WriteAuditRow(ctx, tx, ab.Ev); err != nil {
					return AuditBundle{}, false, err
				}
				return ab, true, nil
			}
			// vetoed — the base survives; fall through to the archive convergence.
		}
	}
	return AuditBundle{}, false, b.archiveBaseIfNoActiveRole(ctx, tx, d, schema, src, now)
}

// anyRoleRowReferences reports whether any role row — active OR archived, from
// any role in the base's registry (instance ∪ engine) — still references the
// base id.
func (b *BaseEngine) anyRoleRowReferences(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	for _, rr := range b.effectiveReferencingRoles(base) {
		q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(rr.Table)+
			" WHERE "+d.QuoteIdent(rr.FKColumn)+" = "+d.Placeholder(1), 1)
		rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
		if err != nil {
			return false, err
		}
		referenced := rows.Next()
		errClose := rows.Err()
		rows.Close()
		if errClose != nil {
			return false, errClose
		}
		if referenced {
			return true, nil
		}
	}
	return false, nil
}

// purgeOrphanBase hard-deletes the base's native children (FK = base id) then
// the base row itself, explicitly in Go (same TX, C7), under a savepoint. A
// foreign-key violation on any statement — a referencing table the registry
// does not know about — rolls back to the savepoint and reports (false, nil):
// the database vetoed the purge, the base stays, the surrounding role delete
// proceeds. Any other error propagates. (true, nil) means the base is gone.
func (b *BaseEngine) purgeOrphanBase(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	if err := tx.Exec(ctx, d.Savepoint(sharedBasePurgeSavepoint)); err != nil {
		return false, err
	}
	err := func() error {
		for _, bc := range base.ChildSchemas() {
			if err := tx.Exec(ctx, childDeleteSQL(d, bc.Table(), bc.FKColumn()), d.EncodeArg(domain.NewID(baseID))); err != nil {
				return err
			}
		}
		return tx.Exec(ctx, deleteSQL(d, base.Table(), base.PKColumn()), d.EncodeArg(domain.NewID(baseID)))
	}()
	if err != nil {
		if _, vetoed := d.IsForeignKeyViolation(err); vetoed {
			if rbErr := tx.Exec(ctx, d.RollbackToSavepoint(sharedBasePurgeSavepoint)); rbErr != nil {
				return false, rbErr
			}
			return false, nil
		}
		return false, err
	}
	// T-SQL has no release statement (ReleaseSavepoint returns "" there — the
	// savepoint is discarded at COMMIT); every other dialect frees it now.
	if rel := d.ReleaseSavepoint(sharedBasePurgeSavepoint); rel != "" {
		if err := tx.Exec(ctx, rel); err != nil {
			return false, err
		}
	}
	return true, nil
}

// --- unified lifecycle convergence -------------------------------------------
//
// The shared base is a mini-root of its native children: it has soft-delete and
// its lifecycle is DRIVEN by its roles. The single rule, per verb:
//   - a role becomes/stays active (insert / update / unarchive) → the base must be
//     active: reactivateBaseIfArchived.
//   - a role is archived (archive) → if no role is left active, the base archives:
//     archiveBaseIfNoActiveRole.
//   - a role is hard-deleted (delete) → the base converges per OrphanPolicy:
//     convergeBaseAfterHardDelete (the vetoable purge, or the orphan archive).
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
	return cascadeBaseLifecycle(ctx, tx, d, base, sd, baseID, nil)
}

// archiveBaseIfNoActiveRole archives the base + its native children once the role
// just archived leaves NO active role referencing the base — stamped with the
// SAME writeNow() instant the triggering role operation bound.
func (b *BaseEngine) archiveBaseIfNoActiveRole(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, now time.Time) error {
	base, sd, baseID, ok, err := baseLifecycleTarget(schema, src)
	if !ok || err != nil {
		return err
	}
	active, err := b.anyActiveRole(ctx, tx, d, base, baseID)
	if err != nil || active {
		return err
	}
	return cascadeBaseLifecycle(ctx, tx, d, base, sd, baseID, &now)
}

// convergeBaseAfterSoftWrite routes a role's archive/unarchive to the matching
// base lifecycle step (shared by the flat + aggregate soft-write paths). The
// unarchive active-sibling veto does NOT live here: it must probe BEFORE the
// role row flips to active (vetoUnarchiveWithActiveSibling, called by the
// soft-write paths ahead of the root UPDATE) — otherwise the dev's active-only
// unique index vetoes the UPDATE itself first, surfacing a raw constraint
// error instead of the friendly conflict.
func (b *BaseEngine) convergeBaseAfterSoftWrite(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, eventType string, now time.Time) error {
	switch eventType {
	case "ARCHIVED":
		return b.archiveBaseIfNoActiveRole(ctx, tx, d, schema, src, now)
	case "UNARCHIVED":
		return b.reactivateBaseIfArchived(ctx, tx, d, schema, src)
	}
	return nil
}

// vetoUnarchiveWithActiveSibling enforces, on the /unarchive verb, the invariant
// the INSERT probe enforces on POST: at most one ACTIVE role row per identity
// per role table. Under the separate-FK model the identity may carry archived
// remnants NEXT TO a newer active row (the dev's active-only uniqueness
// contract); reviving a remnant would then put two active roles on the same
// identity, so it is the same 409 a POST raises. A no-op for a role without a
// shared base and for the shared-PK model (the PK itself caps the table at one
// row per identity). It runs BEFORE the role's own unarchive UPDATE — probing
// after would never fire under an active-only unique index (the index vetoes
// the UPDATE first, as a raw constraint error). The probe excludes the row
// being unarchived, keeping an already-active row's unarchive idempotent.
func (b *BaseEngine) vetoUnarchiveWithActiveSibling(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, id, entityName string) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok || fkCol == schema.PKColumn() {
		return nil
	}
	sd, hasSD := schema.SoftDeleteColumn()
	if !hasSD {
		return nil // unreachable on the unarchive verb (requireSoftDelete gates it); defensive
	}
	_, nk := sharedBaseValues(base, src)
	if err := requireNaturalKey(base, nk); err != nil {
		return err
	}
	baseID := deterministicBaseID(nk)
	q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(schema.Table())+
		" WHERE "+d.QuoteIdent(fkCol)+" = "+d.Placeholder(1)+
		" AND "+d.QuoteIdent(schema.PKColumn())+" <> "+d.Placeholder(2)+
		" AND "+d.QuoteIdent(sd)+" IS NULL", 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)), d.EncodeArg(domain.NewID(id)))
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return SingleNotificationError(entityName, schema.PKColumn(), domain.EntityAlreadyAddedNotification{})
	}
	return rows.Err()
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

// cascadeBaseLifecycle archives (stamp != nil — the operation's writeNow()
// value bound as the soft-delete stamp) or unarchives (stamp == nil — SQL NULL)
// the base row and each soft-deletable native child, gated so it is idempotent
// (a no-op when already in the target state). The BASE-ROW statement also bumps
// `revision = revision + 1` — a lifecycle transition is a base-data change and
// must move the last-writer-wins token like any other base write; the gate
// guarantees the bump fires only on a real transition.
func cascadeBaseLifecycle(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID string, stamp *time.Time) error {
	rev := d.QuoteIdent(base.RevisionColumn())
	bump := ", " + rev + " = " + rev + " + 1"
	if stamp != nil {
		sql := fmt.Sprintf("UPDATE %s SET %s = %s%s WHERE %s = %s AND %s IS NULL",
			d.QuoteIdent(base.Table()), d.QuoteIdent(sd), d.Placeholder(1), bump,
			d.QuoteIdent(base.PKColumn()), d.Placeholder(2), d.QuoteIdent(sd))
		if err := tx.Exec(ctx, sql, d.EncodeArg(*stamp), d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	} else {
		sql := fmt.Sprintf("UPDATE %s SET %s = NULL%s WHERE %s = %s AND %s IS NOT NULL",
			d.QuoteIdent(base.Table()), d.QuoteIdent(sd), bump,
			d.QuoteIdent(base.PKColumn()), d.Placeholder(1), d.QuoteIdent(sd))
		if err := tx.Exec(ctx, sql, d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	}
	for _, bc := range base.ChildSchemas() {
		csd, ok := bc.SoftDeleteColumn()
		if !ok {
			continue
		}
		if stamp != nil {
			if err := tx.Exec(ctx, archiveCascadeSQL(d, bc.Table(), csd, bc.FKColumn()),
				d.EncodeArg(*stamp), d.EncodeArg(domain.NewID(baseID))); err != nil {
				return err
			}
			continue
		}
		if err := tx.Exec(ctx, childCascadeSQL(d, bc.Table(), csd, bc.FKColumn(), nullSetExpr(d), " IS NOT NULL"),
			d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	}
	return nil
}

// baseIsArchived reports whether the base row currently carries a non-null
// soft-delete marker (read once, for idempotency).
func baseIsArchived(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID string) (bool, error) {
	// Project the archived state as ANSI CASE 1/0 rather than scanning the raw
	// soft-delete timestamp (a non-null timestamp cannot be scanned into a
	// []byte under Postgres' binary protocol — it works only while NULL, which
	// is exactly the case reactivateBaseIfArchived probes for) or a bare
	// boolean-valued expression (`col IS NOT NULL` in a SELECT list is a
	// PG/MySQL-ism — T-SQL has no boolean expressions outside predicates).
	// CASE WHEN … THEN 1 ELSE 0 END scans into an int on every dialect.
	q := "SELECT CASE WHEN " + d.QuoteIdent(sd) + " IS NOT NULL THEN 1 ELSE 0 END FROM " + d.QuoteIdent(base.Table()) +
		" WHERE " + d.QuoteIdent(base.PKColumn()) + " = " + d.Placeholder(1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var archived int64
	if err := rows.Scan(&archived); err != nil {
		return false, err
	}
	return archived == 1, rows.Err()
}

// anyActiveRole reports whether any role row referencing the base (instance ∪
// engine registry) is ACTIVE (not soft-deleted). A role without a soft-delete
// column has no archived state, so every existing row counts as active.
func (b *BaseEngine) anyActiveRole(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	for _, rr := range b.effectiveReferencingRoles(base) {
		q := "SELECT 1 FROM " + d.QuoteIdent(rr.Table) + " WHERE " + d.QuoteIdent(rr.FKColumn) + " = " + d.Placeholder(1)
		if rr.SoftDeleteCol != "" {
			q += " AND " + d.QuoteIdent(rr.SoftDeleteCol) + " IS NULL"
		}
		q = d.ApplyLimit(q, 1)
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

// buildBaseUpdate renders the shared-base field UPDATE with the in-place
// revision bump appended to the SET list — ONE statement, one row lock:
// `UPDATE base SET <fields…>, <updatedAt?>, revision = revision + 1 WHERE pk`.
// The increment runs server-side under the base row's lock, so concurrent role
// writes of the same identity serialize in real commit order; the caller reads
// the resulting value back inside the same TX (readBaseRevision) to stamp the
// outbox payload.
func buildBaseUpdate(d Dialect, base *TableSchema, baseID string, fields domain.Fields, now time.Time) (string, []any) {
	keys := SortedKeys(fields)
	nowCols := base.UpdateNowColumns()
	sets := make([]string, 0, len(keys)+len(nowCols)+1)
	args := make([]any, 0, len(keys)+len(nowCols)+1)
	n := 0
	for _, k := range keys {
		n++
		sets = append(sets, d.QuoteIdent(k)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(fields[k]))
	}
	for _, nc := range nowCols {
		n++
		sets = append(sets, d.QuoteIdent(nc)+" = "+d.Placeholder(n))
		args = append(args, d.EncodeArg(now))
	}
	rev := d.QuoteIdent(base.RevisionColumn())
	sets = append(sets, rev+" = "+rev+" + 1")
	n++
	sql := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s",
		d.QuoteIdent(base.Table()), strings.Join(sets, ", "), d.QuoteIdent(base.PKColumn()), d.Placeholder(n))
	args = append(args, d.EncodeArg(domain.NewID(baseID)))
	return sql, args
}

// readBaseRevision reads the shared base's current revision inside the write
// TX — after the base ops of the operation ran, so the value stamped on the
// outbox payload is the one THIS operation's lock scope produced. A vanished
// base row (purged) answers 0.
func readBaseRevision(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (int64, error) {
	return readRevision(ctx, tx, d, base.Table(), base.RevisionColumn(), base.PKColumn(), baseID)
}

// baseExists probes whether the shared base row already exists (the identity
// pre-dates this write) — the signal the SharedBase insert forgot-guard pairs with
// the actionName.
func baseExists(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(base.Table())+
		" WHERE "+d.QuoteIdent(base.PKColumn())+" = "+d.Placeholder(1), 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	exists := rows.Next()
	return exists, rows.Err()
}

// upsertSharedBase persists the shared identity row: a plain INSERT when the
// identity is NEW, or an UPDATE of its shared fields (last-write-wins) by the
// deterministic base id when it ALREADY exists. `baseExists` is the caller's
// already-probed existence signal (baseExists()).
//
// It deliberately does NOT use a DB-native upsert (ON CONFLICT / ON DUPLICATE
// KEY). Postgres' `ON CONFLICT (pk)` is scoped to the primary key, but MySQL's
// `ON DUPLICATE KEY UPDATE` fires on ANY unique key — so if the shared base
// carries a SECOND unique column (e.g. a unique email beside the natural-key
// PK), a new-identity write whose email already exists would hijack the upsert
// onto the wrong persons row on MySQL (new base never inserted → role FK fails →
// 500). An explicit INSERT/UPDATE branch keyed on the PK behaves identically on
// both dialects: the second unique column raises a clean unique violation that
// the repo's ConstraintBinding maps to 409. (Trade-off: two concurrent COLD
// inserts of the same brand-new identity now yield one PK-conflict 409 instead
// of a silent last-write-wins merge — the more correct outcome.)
// Managed columns are honored when the base DECLARES them: CreatedAt(+UpdatedAt)
// stamped on the identity's creation, UpdatedAt on every role-driven change of the
// shared fields (the warm upsert and the role update both land here) — always the
// operation's writeNow() stamp, shared with the role row.
//
// REVISION: the warm UPDATE bumps `revision = revision + 1` in the SAME
// statement (buildBaseUpdate) — server-side, under the base row's lock, so
// concurrent role writes of one identity serialize in real commit order; the
// cold INSERT initializes it to 1 as a plain bound field. The new value is read
// back in-TX and returned so the caller stamps it on the outbox payload
// (_ids.base_revision) — the deterministic last-writer-wins token of every
// read-model write of base data.
func (b *BaseEngine) upsertSharedBase(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string, baseFields domain.Fields, baseExists bool, now time.Time) (int64, error) {
	if baseExists {
		sql, args := buildBaseUpdate(d, base, baseID, baseFields, now)
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return 0, err
		}
		return readBaseRevision(ctx, tx, d, base, baseID)
	}
	sql, args := buildInsert(d, base.Table(), base.PKColumn(), baseID, baseFields, base.InsertNowColumns(), now, base.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return 0, err
	}
	return 1, nil
}
