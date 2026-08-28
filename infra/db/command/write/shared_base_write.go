package write

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// SharedBase (Modelagem 2 / Party-Role) write path. A ROLE schema that declares
// .SharedBase(base, fkCol) is persisted across the shared identity table (the
// base, e.g. pessoa) and its own role table (e.g. aluno), linked by an ParentID to the
// base's DETERMINISTIC id (UUIDv5 of the natural-key value). Two levels of
// existence (design §8.3/§8.4):
//
//   - base identity: id = UUIDv5(naturalKey) → UPSERT (insert-or-update shared
//     fields, last-write-wins). The base is found (and reactivated) regardless of
//     its own DeletedAt state — its lifecycle is DERIVED from the roles
//     (convergence below), and the KeepOrphan dormancy contract depends on the
//     natural key finding its way back. No read-back: app and infra derive the
//     same id.
//   - role: at most ONE ACTIVE row per identity per role table — the framework
//     invariant, enforced on INSERT (an existing ACTIVE role is a 409; an
//     ARCHIVED role is INVISIBLE to the probe — DeletedAt is delete, here like
//     on every other read/write path — so the insert proceeds) and on UNARCHIVE
//     (reviving a remnant while another row of the same role table is active is
//     the same 409 — vetoUnarchiveWithActiveSibling). TOTAL row multiplicity is
//     the dev's DDL contract on the separate-ParentID model: a full UNIQUE(fk) index
//     caps the table at 0..1 rows per identity (an archived remnant physically
//     blocks a new insert — the repository's ConstraintBinding maps the
//     violation), while an active-only unique index (partial index on Postgres,
//     unique generated column on MySQL) admits archived remnants NEXT TO one
//     active row — the "keep the old archived role, open a fresh active one"
//     modeling. The shared-ID model has no choice: the ID itself caps the table
//     at one row per identity. Under concurrency the uniqueness index — not the
//     probe — is the arbiter; reviving an archived role is the explicit
//     /unarchive verb's job, never a POST side effect.
//
// The base's lifecycle is driven by its roles (unified lifecycle convergence
// below); a role hard-delete routes through convergeBaseAfterHardDelete — the
// orphan purge (OrphanPolicy DeleteWhenUnreferenced, database-vetoable) or the
// orphan archive (KeepOrphan + a archivable base).
//
// Covers the FLAT role path (a role is typically a simple entity). All SQL is
// dialect-agnostic via the Dialect.

// sharedBaseNamespace is the fixed UUIDv5 namespace for deriving a shared base's
// id from its natural-key value — stable across processes so app and infra agree.
var sharedBaseNamespace = uuid.MustParse("9b2e7c4a-1f6d-5a83-b0c1-d2e3f4a5b6c7")

// deterministicBaseID derives base.id = UUIDv5(namespace, naturalIDValue).
func deterministicBaseID(naturalIDValue string) string {
	return uuid.NewSHA1(sharedBaseNamespace, []byte(naturalIDValue)).String()
}

// requireNaturalKey guards the one prerequisite the framework cannot enforce in
// DDL: a non-empty natural key. An empty value hashes to a CONSTANT id, collapsing
// every key-less identity into a single base row — a silent, corrupting footgun.
// The UNIQUE(naturalKey) column constraint is otherwise self-enforced by the
// deterministic id ID (equal keys → equal id → ON CONFLICT). Fails loudly instead.
func requireNaturalKey(base *TableSchema, nk string) error {
	if nk == "" {
		return fmt.Errorf(
			"db: shared base %q natural key (column %q) resolved empty — it must be NOT NULL / non-empty: "+
				"its value derives the deterministic identity, and an empty key collapses every record into one",
			base.Table(), base.NaturalIDColumn())
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
	nkGo, _ := base.GoOf(base.NaturalIDColumn())
	var nk string
	for _, goName := range base.GoFields() {
		col, _ := base.ColumnOf(goName)
		// The base is type-less, so a value-object shared field is unwrapped by
		// value here (the same seam writeFields uses for a typed schema): the
		// underlying scalar binds, a nil nullable VO becomes SQL NULL.
		val := rv.FieldByName(goName).Interface()
		if domain.IsValueObject(val) || domain.IsEnumValueObject(val) {
			if u, ok := domain.ValueObjectValue(val); ok {
				val = u
			} else {
				val = nil
			}
		}
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
	if v == nil {
		return ""
	}
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
// shared base id. DeletedAt IS delete on this probe, exactly like every other
// read/write path: an archived role row is invisible here, so the caller's
// insert proceeds and the dev's DDL arbitrates the collision with any physical
// remnant (shared-ID → the primary key; separate ParentID → a full UNIQUE(fk) blocks
// it, an active-only unique index admits it next to the remnants). The probe
// asks "is any row active?", so LIMIT 1 decides it regardless of how many
// archived remnants the identity carries; when two inserts race, the
// uniqueness index — not this probe — is the arbiter.
func (b *BaseEngine) findActiveRoleByFK(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, fkCol, baseID string) (bool, error) {
	q := "SELECT 1 FROM " + d.QuoteIdent(schema.Table()) +
		" WHERE " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1)
	if sd, hasSD := schema.DeletedAtColumn(); hasSD {
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
	// A role either carries a SEPARATE ParentID column to the base, or shares the base's id
	// as its OWN primary key (fkCol == the role ID — a strict 1:1 where role.id ==
	// base.id). In the shared-ID model the id is written once, as the ID (buildInsert
	// prepends it), so injecting it as a field too would emit a duplicate column;
	// inject the ParentID only when it is a distinct column.
	sharedPK := fkCol == schema.IDColumn()
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
		return domain.WriteResult{}, SingleNotificationError(entity.EntityName(), schema.IDColumn(), domain.EntityAlreadyAddedNotification{})
	}
	// No ACTIVE role → insert. An ARCHIVED remnant is deliberately not looked
	// for (DeletedAt is delete): if one exists, the schema's own constraints
	// veto or admit this insert — shared-ID collides on the primary key (the
	// repository's ConstraintBinding maps it), a separate-ParentID model decides
	// through its own unique index.
	var id string
	if sharedPK {
		// The role's own ID IS the deterministic base id (role.id == base.id).
		id = baseID
	} else {
		nid, err := newWriteID()
		if err != nil {
			return domain.WriteResult{}, err
		}
		id = nid
	}
	sql, args := buildInsert(d, schema.Table(), schema.IDColumn(), id, roleFields, schema.InsertNowColumns(), now, schema.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return domain.WriteResult{}, err
	}
	// One pass (writeChildren) handles role children (ParentID→role id) and shared-base
	// native children (ParentID→base id) by the OperationOf categorization: a loaded
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
	if _, err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
		return domain.WriteResult{}, err
	}
	// ONE outbox row per write: the payload is self-sufficient (role ∪ base ∪
	// sibling fields, children with ops, structural ids + base revision), so the
	// SyncEngine fans out to the OTHER roles' read models from THIS event — the
	// historical empty base-table UPDATED row is no longer emitted.
	if err := WriteOutbox(ctx, tx, schema.Table(), "INSERTED", id,
		buildWritePayload(schema, src, root, "INSERTED", now, CascadeStamps{}, roleFields,
			outboxMeta{ID: id, Revision: 1, CreatedAt: insertCreatedAt(schema, now), BaseID: baseID, BaseRevision: baseRev})); err != nil {
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

// updateWithBase is the role UPDATE (PUT/PATCH): UPDATE the role row by ID, apply
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
	rev := loadedRevision(src)
	sql, args := buildUpdate(d, schema.Table(), schema.IDColumn(), entity.ID().Value(), roleFields, schema.UpdateNowColumns(), now, schema.RevisionColumn(), rev)
	if err := execExpectingRow(ctx, tx, d, sql, args, schema.Table(), entity.EntityName(), schema.IDColumn(), entity.ID().Value(), rev); err != nil {
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
	// The base always exists here (we are updating an existing role, whose ParentID
	// references it), so this is always the UPDATE branch.
	baseRev, err := b.upsertSharedBase(ctx, tx, d, base, baseID, baseFields, true, now)
	if err != nil {
		return domain.WriteResult{}, err
	}
	// An updated role is active → keep the shared identity active (reactivate if a
	// prior all-archived state left it archived). Idempotent when already active.
	if _, err := b.reactivateBaseIfArchived(ctx, tx, d, schema, src); err != nil {
		return domain.WriteResult{}, err
	}
	ownRev := int64(0)
	var ownCreatedAt time.Time
	if rc := schema.RevisionColumn(); rc != "" {
		if ownRev, ownCreatedAt, err = readRevisionCreatedAt(ctx, tx, d, schema.Table(), rc, schema.CreatedAtColumn(), schema.IDColumn(), entity.ID().Value()); err != nil {
			return domain.WriteResult{}, err
		}
	}
	// ONE outbox row per write (see insertWithBase): the payload carries the
	// base id + revision, so the fan-out rides this event — no empty base row.
	if err := WriteOutbox(ctx, tx, schema.Table(), "UPDATED", entity.ID().Value(),
		buildWritePayload(schema, src, root, "UPDATED", now, CascadeStamps{}, roleFields,
			outboxMeta{ID: entity.ID().Value(), Revision: ownRev, CreatedAt: ownCreatedAt, BaseID: baseID, BaseRevision: baseRev})); err != nil {
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
// base. Shared-ID model: pure arithmetic — the role id IS UUIDv5(naturalKey),
// so the id derived from the request must equal the row's own id. Separate-ParentID
// model: one ID-indexed SELECT (inside the open TX) projecting the comparison
// as ANSI CASE 1/0 — the dialect-safe form for a boolean answer (a bare
// boolean-valued `fk = ?` in a SELECT list is a PG/MySQL-ism; T-SQL would
// parse it as an alias assignment); a missing row skips the guard (the
// role UPDATE right after reports not-found exactly as before). The Old
// snapshot cannot arbitrate here: a manual handler that skipped load-first
// captures the request itself as Old, so an Old-vs-request comparison would be
// vacuous precisely in the case that needs guarding.
func guardNaturalKeyImmutable(ctx context.Context, tx WriteTx, d Dialect, schema, base *TableSchema, entityName, id, fkCol, baseID string) error {
	nkGo, _ := base.GoOf(base.NaturalIDColumn())
	if fkCol == schema.IDColumn() {
		if id != baseID {
			return SingleNotificationError(entityName, nkGo, domain.NaturalIDImmutableNotification{})
		}
		return nil
	}
	q := "SELECT CASE WHEN " + d.QuoteIdent(fkCol) + " = " + d.Placeholder(1) + " THEN 1 ELSE 0 END" +
		" FROM " + d.QuoteIdent(schema.Table()) +
		" WHERE " + d.QuoteIdent(schema.IDColumn()) + " = " + d.Placeholder(2)
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
		return SingleNotificationError(entityName, nkGo, domain.NaturalIDImmutableNotification{})
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
//     lifecycle convergence: with no ACTIVE role left, a archivable base
//     archives (with its native children); without DeletedAt on the base,
//     nothing happens.
//
// A no-op for a role without a shared base, and — before any identity work —
// for a base with neither DeleteWhenUnreferenced nor DeletedAt (nothing could
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
	_, baseHasSD := base.DeletedAtColumn()
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
				// The purge row carries _ids too — every framework-produced event
				// does; the SyncEngine reads the base id from it to fan out.
				if err := WriteOutbox(ctx, tx, base.Table(), "DELETED", baseID,
					domain.Fields{
						base.IDColumn(): domain.NewID(baseID),
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
	_, archErr := b.archiveBaseIfNoActiveRole(ctx, tx, d, schema, src, now)
	return AuditBundle{}, false, archErr
}

// anyRoleRowReferences reports whether any role row — active OR archived, from
// any role in the base's registry (instance ∪ engine) — still references the
// base id.
func (b *BaseEngine) anyRoleRowReferences(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	for _, rr := range b.effectiveReferencingRoles(base) {
		q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(rr.Table)+
			" WHERE "+d.QuoteIdent(rr.ParentIDColumn)+" = "+d.Placeholder(1), 1)
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

// purgeOrphanBase hard-deletes the base's native children (ParentID = base id) then
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
			if err := tx.Exec(ctx, childDeleteSQL(d, bc.Table(), bc.ParentIDColumn()), d.EncodeArg(domain.NewID(baseID))); err != nil {
				return err
			}
		}
		return tx.Exec(ctx, deleteSQL(d, base.Table(), base.IDColumn()), d.EncodeArg(domain.NewID(baseID)))
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
// The shared base is a mini-root of its native children: it has DeletedAt and
// its lifecycle is DRIVEN by its roles. The single rule, per verb:
//   - a role becomes/stays active (insert / update / unarchive) → the base must be
//     active: reactivateBaseIfArchived.
//   - a role is archived (archive) → if no role is left active, the base archives:
//     archiveBaseIfNoActiveRole.
//   - a role is hard-deleted (delete) → the base converges per OrphanPolicy:
//     convergeBaseAfterHardDelete (the vetoable purge, or the orphan archive).
// All three no-op without a shared base; the first two also no-op when the base
// has no DeletedAt (then only the orphan branch applies). They read the base /
// role state and only write on a real transition (idempotent), so a steady-state
// write costs at most one probing SELECT and no redundant UPDATE.

// reactivateBaseIfArchived un-archives the base + its native children when a role
// is/becomes active and the base was archived (the revive direction).
//
// The probe reads the base's archive STAMP, not a yes/no: a zero stamp is the
// same "nothing to do" the boolean gave (the base is active), and a non-zero one
// is what the native-children cascade needs — the instant their own archive
// bound, so the restore reaches exactly the children the base's archive put to
// sleep and leaves the ones archived on their own where they are.
// It answers the instant it undid — zero when there was nothing to undo — so the
// caller can describe the base-children segment with the stamp THEIR statement
// used, which is the base's own and not necessarily the role's.
func (b *BaseEngine) reactivateBaseIfArchived(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity) (time.Time, error) {
	base, sd, baseID, ok, err := baseLifecycleTarget(schema, src)
	if !ok || err != nil {
		return time.Time{}, err
	}
	stamp, err := readArchiveStamp(ctx, tx, d, base.Table(), sd, base.IDColumn(), baseID)
	if err != nil || stamp.IsZero() {
		return time.Time{}, err
	}
	return stamp, unarchiveBaseCascade(ctx, tx, d, base, sd, baseID, stamp)
}

// archiveBaseIfNoActiveRole archives the base + its native children once the role
// just archived leaves NO active role referencing the base — stamped with the
// SAME writeNow() instant the triggering role operation bound.
//
// It answers that instant when it fired and the zero time when it did not (no
// shared base, no DeletedAt on it, or another role still active): the base
// children were then NOT touched, and the event must not claim they were.
func (b *BaseEngine) archiveBaseIfNoActiveRole(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, now time.Time) (time.Time, error) {
	base, sd, baseID, ok, err := baseLifecycleTarget(schema, src)
	if !ok || err != nil {
		return time.Time{}, err
	}
	active, err := b.anyActiveRole(ctx, tx, d, base, baseID)
	if err != nil || active {
		return time.Time{}, err
	}
	return now, archiveBaseCascade(ctx, tx, d, base, sd, baseID, now)
}

// convergeBaseAfterSoftWrite routes a role's archive/unarchive to the matching
// base lifecycle step (shared by the flat + aggregate soft-write paths). The
// unarchive active-sibling veto does NOT live here: it must probe BEFORE the
// role row flips to active (vetoUnarchiveWithActiveSibling, called by the
// soft-write paths ahead of the root UPDATE) — otherwise the dev's active-only
// unique index vetoes the UPDATE itself first, surfacing a raw constraint
// error instead of the friendly conflict.
func (b *BaseEngine) convergeBaseAfterSoftWrite(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, eventType string, now time.Time) (time.Time, error) {
	switch eventType {
	case "ARCHIVED":
		return b.archiveBaseIfNoActiveRole(ctx, tx, d, schema, src, now)
	case "UNARCHIVED":
		return b.reactivateBaseIfArchived(ctx, tx, d, schema, src)
	}
	return time.Time{}, nil
}

// vetoUnarchiveWithActiveSibling enforces, on the /unarchive verb, the invariant
// the INSERT probe enforces on POST: at most one ACTIVE role row per identity
// per role table. Under the separate-ParentID model the identity may carry archived
// remnants NEXT TO a newer active row (the dev's active-only uniqueness
// contract); reviving a remnant would then put two active roles on the same
// identity, so it is the same 409 a POST raises. A no-op for a role without a
// shared base and for the shared-ID model (the ID itself caps the table at one
// row per identity). It runs BEFORE the role's own unarchive UPDATE — probing
// after would never fire under an active-only unique index (the index vetoes
// the UPDATE first, as a raw constraint error). The probe excludes the row
// being unarchived, keeping an already-active row's unarchive idempotent.
func (b *BaseEngine) vetoUnarchiveWithActiveSibling(ctx context.Context, tx WriteTx, d Dialect, schema *TableSchema, src domain.Entity, id, entityName string) error {
	base, fkCol, ok := schema.SharedBaseRef()
	if !ok || fkCol == schema.IDColumn() {
		return nil
	}
	sd, hasSD := schema.DeletedAtColumn()
	if !hasSD {
		return nil // unreachable on the unarchive verb (requireDeletedAt gates it); defensive
	}
	_, nk := sharedBaseValues(base, src)
	if err := requireNaturalKey(base, nk); err != nil {
		return err
	}
	baseID := deterministicBaseID(nk)
	q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(schema.Table())+
		" WHERE "+d.QuoteIdent(fkCol)+" = "+d.Placeholder(1)+
		" AND "+d.QuoteIdent(schema.IDColumn())+" <> "+d.Placeholder(2)+
		" AND "+d.QuoteIdent(sd)+" IS NULL", 1)
	rows, err := tx.Query(ctx, q, d.EncodeArg(domain.NewID(baseID)), d.EncodeArg(domain.NewID(id)))
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return SingleNotificationError(entityName, schema.IDColumn(), domain.EntityAlreadyAddedNotification{})
	}
	return rows.Err()
}

// baseLifecycleTarget resolves the shared base + its DeletedAt column + the
// deterministic id from the role schema and entity, reporting ok=false (skip)
// when there is no shared base or the base has no DeletedAt (lifecycle is then
// hard-only, governed by the orphan refcount).
func baseLifecycleTarget(schema *TableSchema, src domain.Entity) (base *TableSchema, sd, baseID string, ok bool, err error) {
	base, _, has := schema.SharedBaseRef()
	if !has {
		return nil, "", "", false, nil
	}
	sd, hasSD := base.DeletedAtColumn()
	if !hasSD {
		return nil, "", "", false, nil
	}
	_, nk := sharedBaseValues(base, src)
	if err := requireNaturalKey(base, nk); err != nil {
		return nil, "", "", false, err
	}
	return base, sd, deterministicBaseID(nk), true, nil
}

// archiveBaseCascade / unarchiveBaseCascade are the base's two lifecycle
// directions, and they are the SAME cascade the aggregate root runs over its own
// children (cascadeChildren) — a shared base is a mini-root, so it converges by
// the same rule and around the same single instant:
//
//	archive   → stamp `now` (the triggering role operation's one writeNow(), the
//	            very value the role row and the role's own children carry) on the
//	            base row and on every native child still ACTIVE
//	unarchive → clear it from the base row and from the native children carrying
//	            EXACTLY the base's archive stamp — never from a child that was
//	            archived on its own before the base went down
//
// Both are gated so they are idempotent (a no-op when already in the target
// state). The BASE-ROW statement also bumps `revision = revision + 1` — a
// lifecycle transition is a base-data change and must move the last-writer-wins
// token like any other base write; the gate guarantees the bump fires only on a
// real transition.
func archiveBaseCascade(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID string, stamp time.Time) error {
	sql := fmt.Sprintf("UPDATE %s SET %s = %s%s WHERE %s = %s AND %s IS NULL",
		d.QuoteIdent(base.Table()), d.QuoteIdent(sd), d.Placeholder(1), baseRevisionBump(d, base),
		d.QuoteIdent(base.IDColumn()), d.Placeholder(2), d.QuoteIdent(sd))
	if err := tx.Exec(ctx, sql, d.EncodeArg(stamp), d.EncodeArg(domain.NewID(baseID))); err != nil {
		return err
	}
	for _, bc := range base.ChildSchemas() {
		csd, ok := bc.DeletedAtColumn()
		if !ok {
			continue
		}
		if err := tx.Exec(ctx, archiveCascadeSQL(d, bc.Table(), csd, bc.ParentIDColumn()),
			d.EncodeArg(stamp), d.EncodeArg(domain.NewID(baseID))); err != nil {
			return err
		}
	}
	return nil
}

func unarchiveBaseCascade(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, sd, baseID string, stamp time.Time) error {
	sql := fmt.Sprintf("UPDATE %s SET %s = NULL%s WHERE %s = %s AND %s IS NOT NULL",
		d.QuoteIdent(base.Table()), d.QuoteIdent(sd), baseRevisionBump(d, base),
		d.QuoteIdent(base.IDColumn()), d.Placeholder(1), d.QuoteIdent(sd))
	if err := tx.Exec(ctx, sql, d.EncodeArg(domain.NewID(baseID))); err != nil {
		return err
	}
	for _, bc := range base.ChildSchemas() {
		csd, ok := bc.DeletedAtColumn()
		if !ok {
			continue
		}
		if err := tx.Exec(ctx, unarchiveCascadeSQL(d, bc.Table(), csd, bc.ParentIDColumn()),
			d.EncodeArg(domain.NewID(baseID)), d.EncodeArg(stamp)); err != nil {
			return err
		}
	}
	return nil
}

// baseRevisionBump renders the base row's ", revision = revision + 1" tail,
// shared by the two lifecycle directions.
func baseRevisionBump(d Dialect, base *TableSchema) string {
	rev := d.QuoteIdent(base.RevisionColumn())
	return ", " + rev + " = " + rev + " + 1"
}

// anyActiveRole reports whether any role row referencing the base (instance ∪
// engine registry) is ACTIVE (not archived). A role without a DeletedAt
// column has no archived state, so every existing row counts as active.
func (b *BaseEngine) anyActiveRole(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	for _, rr := range b.effectiveReferencingRoles(base) {
		q := "SELECT 1 FROM " + d.QuoteIdent(rr.Table) + " WHERE " + d.QuoteIdent(rr.ParentIDColumn) + " = " + d.Placeholder(1)
		if rr.DeletedAtCol != "" {
			q += " AND " + d.QuoteIdent(rr.DeletedAtCol) + " IS NULL"
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

// readBaseRevision reads the shared base's current revision inside the write
// TX — after the base ops of the operation ran, so the value stamped on the
// outbox payload is the one THIS operation's lock scope produced. A vanished
// base row (purged) answers 0.
func readBaseRevision(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (int64, error) {
	return readRevision(ctx, tx, d, base.Table(), base.RevisionColumn(), base.IDColumn(), baseID)
}

// bumpBaseRevision advances the shared identity's revision — the IDENTITY
// CLOCK — under the base row's lock. EVERY write that touches the identity
// (any role verb, base upsert, lifecycle convergence, role hard-delete)
// advances it, so the base revision totally orders the identity's closure: shared
// scalars, base children, the remnant pick AND the role rows themselves (a
// role-only write bumps it too). The read side depends on that totality — a
// SharedBaseView composition, a fan-out and a consult repair are all guarded
// by this value, and revisions of different rows are not comparable without
// one shared counter.
//
// The verbs that already write the base row bump inside their own statement
// (upsertSharedBase's warm UPDATE, cascadeBaseLifecycle); this helper covers
// the verbs that otherwise would not touch the base at all (role archive /
// unarchive without a base transition, role hard-delete, batch role ops).
// Double-bumping in one TX (helper + a real transition) is harmless — the
// revision must be monotone, not dense. Lock order stays role row → base row on
// every verb that uses it (the role statement always precedes). A vanished
// base row (purge) makes the UPDATE a 0-row no-op.
func bumpBaseRevision(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) error {
	rc := base.RevisionColumn()
	if rc == "" {
		return nil // unreachable on the canonical path (Revision is boot-mandatory); defensive
	}
	rev := d.QuoteIdent(rc)
	sql := "UPDATE " + d.QuoteIdent(base.Table()) + " SET " + rev + " = " + rev + " + 1" +
		" WHERE " + d.QuoteIdent(base.IDColumn()) + " = " + d.Placeholder(1)
	return tx.Exec(ctx, sql, d.EncodeArg(domain.NewID(baseID)))
}

// baseExists probes whether the shared base row already exists (the identity
// pre-dates this write) — the signal the SharedBase insert forgot-guard pairs with
// the actionName.
func baseExists(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string) (bool, error) {
	q := d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(base.Table())+
		" WHERE "+d.QuoteIdent(base.IDColumn())+" = "+d.Placeholder(1), 1)
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
// ID), a new-identity write whose email already exists would hijack the upsert
// onto the wrong persons row on MySQL (new base never inserted → role ParentID fails →
// 500). An explicit INSERT/UPDATE branch keyed on the ID behaves identically on
// both dialects: the second unique column raises a clean unique violation that
// the repo's ConstraintBinding maps to 409. (Trade-off: two concurrent COLD
// inserts of the same brand-new identity now yield one ID-conflict 409 instead
// of a silent last-write-wins merge — the more correct outcome.)
// Managed columns are honored when the base DECLARES them: CreatedAt(+UpdatedAt)
// stamped on the identity's creation, UpdatedAt on every role-driven change of the
// shared fields (the warm upsert and the role update both land here) — always the
// operation's writeNow() stamp, shared with the role row.
//
// REVISION: the warm UPDATE bumps `revision = revision + 1` in the SAME
// statement (buildUpdate's revCol append) — server-side, under the base row's
// lock, so concurrent role writes of one identity serialize in real commit
// order; the cold INSERT initializes it to 1 as a plain bound field. The new
// value is read back in-TX and returned so the caller stamps it on the outbox
// payload (_ids.base_revision) — the deterministic last-writer-wins token of
// every read-model write of base data.
func (b *BaseEngine) upsertSharedBase(ctx context.Context, tx WriteTx, d Dialect, base *TableSchema, baseID string, baseFields domain.Fields, baseExists bool, now time.Time) (int64, error) {
	if baseExists {
		// Unguarded on purpose: several roles converge on the shared identity and
		// the base is last-write-wins by design — guarding it would turn an
		// unrelated role's write into a conflict on this one.
		sql, args := buildUpdate(d, base.Table(), base.IDColumn(), baseID, baseFields, base.UpdateNowColumns(), now, base.RevisionColumn(), 0)
		if err := tx.Exec(ctx, sql, args...); err != nil {
			return 0, err
		}
		return readBaseRevision(ctx, tx, d, base, baseID)
	}
	sql, args := buildInsert(d, base.Table(), base.IDColumn(), baseID, baseFields, base.InsertNowColumns(), now, base.RevisionColumn())
	if err := tx.Exec(ctx, sql, args...); err != nil {
		return 0, err
	}
	return 1, nil
}
