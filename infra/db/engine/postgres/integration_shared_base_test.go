//go:build integration && postgres

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// SharedBase separate-FK integration: the active-only uniqueness modeling
// against a REAL Postgres — an identity may keep archived role remnants NEXT TO
// one active row (partial unique index on active rows), the framework invariant
// (at most one ACTIVE role row per identity per role table) holds on POST and
// on /unarchive, and the natural-key immutability guard rejects a mutated key
// with the real dialect SQL.

type sbStudent struct {
	domain.BaseEntity
	Name       string // shared (lives on sb_persons)
	Document   string // shared + natural key
	Enrollment string // role-own
}

func (e *sbStudent) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (*sbStudent) BuildRules(string, domain.Service, *domain.Rules) {}

func sbStudentSchema() *core.TableSchema {
	base := core.NewSharedBase("sb_persons").Revision("revision").
		PK("id").
		Field("Document", "document").
		Field("Name", "name").
		NaturalKey("document").
		SoftDelete("deleted_at")
	return core.NewTableSchema[*sbStudent]("sb_students").
		PK("id").
		Field("Enrollment", "enrollment").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		SharedBase(base, "person_id")
}

func createSharedBaseTables(t *testing.T, pg *Postgres) {
	t.Helper()
	createTable(t, pg, `CREATE TABLE sb_persons (
		id UUID PRIMARY KEY,
		document TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		revision BIGINT NOT NULL DEFAULT 0,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	createTable(t, pg, `CREATE TABLE sb_students (
		id UUID PRIMARY KEY,
		person_id UUID NOT NULL REFERENCES sb_persons (id),
		enrollment TEXT NOT NULL,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
	// The documented active-only uniqueness contract: archived remnants may
	// accumulate, but at most one ACTIVE row per identity.
	createTable(t, pg, `CREATE UNIQUE INDEX sb_students_one_active
		ON sb_students (person_id) WHERE deleted_at IS NULL`)
}

func sbInsert(t *testing.T, pg *Postgres, doc, enrollment, actionName string) string {
	t.Helper()
	ins, err := domain.GetInsertable(&sbStudent{Name: "Ana", Document: doc, Enrollment: enrollment}, nil, actionName)
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := pg.Insert(testCtx(), ins, sbStudentSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert(%s): %v", enrollment, err)
	}
	return res.ID.Value()
}

func sbArchive(t *testing.T, pg *Postgres, id, doc string) {
	t.Helper()
	e := &sbStudent{Name: "Ana", Document: doc, Enrollment: "x"}
	e.SetID(domain.NewID(id))
	arch, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := pg.Archive(testCtx(), arch, sbStudentSchema(), noHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
}

func sbUnarchive(t *testing.T, pg *Postgres, id, doc string) error {
	t.Helper()
	e := &sbStudent{Name: "Ana", Document: doc, Enrollment: "x"}
	e.SetID(domain.NewID(id))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	return pg.Unarchive(testCtx(), un, sbStudentSchema(), noHook)
}

func sbScalarString(t *testing.T, pg *Postgres, q string) string {
	t.Helper()
	var v string
	if err := pg.Pool().QueryRow(context.Background(), q).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return v
}

// An archived remnant is invisible to the POST probe and the partial unique
// index admits the new row: 2 physical rows, 1 active, still ONE identity —
// and the fresh active role revives the archived base.
func TestPostgres_SharedBaseSeparateFK_ArchivedRemnantAdmitsNewActive(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSharedBaseTables(t, pg)

	s1 := sbInsert(t, pg, "D1", "M1", "GetInsertable")
	sbArchive(t, pg, s1, "D1")
	if got := activeCount(t, pg, "sb_persons"); got != 0 {
		t.Fatalf("archiving the only role must archive the base, active persons = %d", got)
	}

	s2 := sbInsert(t, pg, "D1", "M2", "GetUpsertable")
	if s1 == s2 {
		t.Fatalf("the new role must be a NEW row (fresh surrogate id), both = %s", s1)
	}
	if got := rowCount(t, pg, "sb_students"); got != 2 {
		t.Errorf("expected 2 role rows (remnant + active), got %d", got)
	}
	if got := activeCount(t, pg, "sb_students"); got != 1 {
		t.Errorf("expected exactly 1 ACTIVE role row, got %d", got)
	}
	if got := rowCount(t, pg, "sb_persons"); got != 1 {
		t.Errorf("expected ONE shared identity, got %d", got)
	}
	if got := activeCount(t, pg, "sb_persons"); got != 1 {
		t.Errorf("the new active role must revive the base, active persons = %d", got)
	}
}

// Reviving the remnant while the newer row is active is the same conflict a
// POST raises; the verb rolls back and the state stays untouched.
func TestPostgres_SharedBaseSeparateFK_UnarchiveNextToActiveIs409(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSharedBaseTables(t, pg)

	s1 := sbInsert(t, pg, "D1", "M1", "GetInsertable")
	sbArchive(t, pg, s1, "D1")
	sbInsert(t, pg, "D1", "M2", "GetUpsertable")

	err := sbUnarchive(t, pg, s1, "D1")
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("unarchiving next to an active sibling must be a conflict NotificationCarrier, got %T (%v)", err, err)
	}
	if got := activeCount(t, pg, "sb_students"); got != 1 {
		t.Errorf("the vetoed unarchive must leave exactly 1 active role, got %d", got)
	}
}

// Without an active sibling the remnant revives normally — and brings the
// archived base back with it.
func TestPostgres_SharedBaseSeparateFK_UnarchiveWithoutSiblingRevives(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSharedBaseTables(t, pg)

	s1 := sbInsert(t, pg, "D1", "M1", "GetInsertable")
	sbArchive(t, pg, s1, "D1")

	if err := sbUnarchive(t, pg, s1, "D1"); err != nil {
		t.Fatalf("Unarchive without sibling: %v", err)
	}
	if got := activeCount(t, pg, "sb_students"); got != 1 {
		t.Errorf("expected the remnant revived, active roles = %d", got)
	}
	if got := activeCount(t, pg, "sb_persons"); got != 1 {
		t.Errorf("the revived role must revive the base, active persons = %d", got)
	}
}

// The natural key is immutable: an UPDATE carrying a diverging key is rejected
// (422) before any write — no second identity appears, no third party's shared
// fields are overwritten — while a same-key update flows normally.
func TestPostgres_SharedBaseSeparateFK_NaturalKeyGuard(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createSharedBaseTables(t, pg)

	s1 := sbInsert(t, pg, "D1", "M1", "GetInsertable")

	mutate := &sbStudent{Name: "Ana", Document: "D-CHANGED", Enrollment: "M1"}
	mutate.SetID(domain.NewID(s1))
	upd, err := domain.GetUpdatable(mutate, func(*sbStudent) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	_, err = pg.Update(testCtx(), upd, sbStudentSchema(), noHook)
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("a mutated natural key must be a NotificationCarrier, got %T (%v)", err, err)
	}
	if got := rowCount(t, pg, "sb_persons"); got != 1 {
		t.Errorf("the rejected update must not create a second identity, persons = %d", got)
	}
	if doc := sbScalarString(t, pg, `SELECT document FROM sb_persons`); doc != "D1" {
		t.Errorf("the persisted natural key must stay D1, got %q", doc)
	}

	legit := &sbStudent{Name: "Ana Maria", Document: "D1", Enrollment: "M1-NEW"}
	legit.SetID(domain.NewID(s1))
	upd2, err := domain.GetUpdatable(legit, func(*sbStudent) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable(legit): %v", err)
	}
	if _, err := pg.Update(testCtx(), upd2, sbStudentSchema(), noHook); err != nil {
		t.Fatalf("a same-key update must pass the guard, got %v", err)
	}
	if enr := sbScalarString(t, pg, `SELECT enrollment FROM sb_students WHERE deleted_at IS NULL`); enr != "M1-NEW" {
		t.Errorf("the legit update must persist, enrollment = %q", enr)
	}
}

// sbpStudent drives the DeleteWhenUnreferenced purge scenarios on its own
// table pair (the engine-scoped role registry forbids re-declaring sb_persons
// with a different OrphanPolicy).
type sbpStudent struct {
	domain.BaseEntity
	Name       string
	Document   string
	Enrollment string
}

func (e *sbpStudent) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*sbpStudent) BuildRules(string, domain.Service, *domain.Rules) {}

func sbpSchema() *core.TableSchema {
	base := core.NewSharedBase("sbp_persons").Revision("revision").
		PK("id").
		Field("Document", "document").
		Field("Name", "name").
		NaturalKey("document").
		OrphanPolicy(core.DeleteWhenUnreferenced)
	return core.NewTableSchema[*sbpStudent]("sbp_students").
		PK("id").
		Field("Enrollment", "enrollment").
		SharedBase(base, "person_id")
}

// TestPostgres_SharedBase_PurgeAndVeto proves BOTH savepoint legs of the
// database-vetoable orphan purge on a real Postgres, now rendered through
// Dialect.Savepoint/RollbackToSavepoint/ReleaseSavepoint: (1) deleting the
// last role purges the base (savepoint released); (2) a foreign table still
// referencing the person vetoes the purge (SQLSTATE 23503 → rollback to the
// savepoint) while the role delete commits.
func TestPostgres_SharedBase_PurgeAndVeto(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE sbp_persons (
		id UUID PRIMARY KEY,
		document TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL,
		revision BIGINT NOT NULL DEFAULT 0
	)`)
	createTable(t, pg, `CREATE TABLE sbp_students (
		id UUID PRIMARY KEY,
		person_id UUID NOT NULL REFERENCES sbp_persons (id) ON DELETE RESTRICT,
		enrollment TEXT NOT NULL
	)`)

	insert := func(doc string) string {
		ins, err := domain.GetInsertable(&sbpStudent{Name: "Ana", Document: doc, Enrollment: "M1"}, nil, "GetInsertable")
		if err != nil {
			t.Fatalf("GetInsertable: %v", err)
		}
		res, err := pg.Insert(testCtx(), ins, sbpSchema(), noHook)
		if err != nil {
			t.Fatalf("Insert: %v", err)
		}
		return res.ID.Value()
	}
	del := func(id, doc string) {
		e := &sbpStudent{Name: "Ana", Document: doc, Enrollment: "x"}
		e.SetID(domain.NewID(id))
		d, err := domain.GetDeletable(e, nil, "GetDeletable")
		if err != nil {
			t.Fatalf("GetDeletable: %v", err)
		}
		if err := pg.Delete(testCtx(), d, sbpSchema(), noHook); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	s1 := insert("DP1")
	del(s1, "DP1")
	if got := rowCount(t, pg, "sbp_persons"); got != 0 {
		t.Fatalf("purge leg: base must be gone, persons = %d", got)
	}
	if got := rowCount(t, pg, "sbp_students"); got != 0 {
		t.Fatalf("purge leg: role must be gone, students = %d", got)
	}

	s2 := insert("DP2")
	createTable(t, pg, `CREATE TABLE sbp_external_refs (
		person_id UUID NOT NULL REFERENCES sbp_persons (id) ON DELETE RESTRICT
	)`)
	if _, err := pg.Pool().Exec(context.Background(), `INSERT INTO sbp_external_refs (person_id) SELECT id FROM sbp_persons`); err != nil {
		t.Fatalf("seed ext: %v", err)
	}
	del(s2, "DP2")
	if got := rowCount(t, pg, "sbp_students"); got != 0 {
		t.Fatalf("veto leg: role delete must commit, students = %d", got)
	}
	if got := rowCount(t, pg, "sbp_persons"); got != 1 {
		t.Fatalf("veto leg: base must survive the vetoed purge, persons = %d", got)
	}
}
