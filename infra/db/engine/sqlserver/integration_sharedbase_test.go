//go:build integration && sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// SharedBase separate-FK integration against a REAL SQL Server: the
// active-only uniqueness modeling — archived role remnants NEXT TO one active
// row — via a native FILTERED UNIQUE INDEX (`WHERE deleted_at IS NULL`), the
// clean form MySQL has to emulate with a generated column. Covers the
// POST-over-remnant admit, the /unarchive active-sibling veto (409), and the
// natural-key immutability guard (422) with the real dialect SQL and the
// BINARY(16) UUID codec.

type sbMsStudent struct {
	domain.BaseEntity
	Name       string // shared (lives on sb_persons)
	Document   string // shared + natural key
	Enrollment string // role-own
}

func (e *sbMsStudent) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (*sbMsStudent) BuildRules(string, domain.Service, *domain.Rules) {}

func sbMsSchema() *core.TableSchema {
	base := core.NewSharedBase("sb_persons").
		PK("id").
		Field("Document", "document").
		Field("Name", "name").
		NaturalKey("document").
		SoftDelete("deleted_at")
	return core.NewTableSchema[*sbMsStudent]("sb_students").
		PK("id").
		Field("Enrollment", "enrollment").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at").
		SharedBase(base, "person_id")
}

func sbMsSetup(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	eng, raw := setup(t) // throw-away DB + outbox provisioning reused; adds the SharedBase pair below
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE sb_persons (
			id BINARY(16) NOT NULL PRIMARY KEY,
			document VARCHAR(64) NOT NULL UNIQUE,
			name NVARCHAR(255) NOT NULL,
			deleted_at DATETIME2(6) NULL,
			created_at DATETIME2(6) NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME2(6) NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE sb_students (
			id BINARY(16) NOT NULL PRIMARY KEY,
			person_id BINARY(16) NOT NULL,
			enrollment VARCHAR(64) NOT NULL,
			deleted_at DATETIME2(6) NULL,
			created_at DATETIME2(6) NOT NULL,
			updated_at DATETIME2(6) NOT NULL,
			CONSTRAINT fk_sb_student_person FOREIGN KEY (person_id) REFERENCES sb_persons (id)
		)`,
		// Active-only uniqueness, natively: one ACTIVE row per identity, any
		// number of archived remnants (a filtered unique index — the shape PG
		// writes as a partial index and MySQL emulates with a generated column).
		`CREATE UNIQUE INDEX sb_students_one_active ON sb_students (person_id) WHERE deleted_at IS NULL`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl: %v\n%s", err, stmt)
		}
	}
	return eng, raw
}

func sbMsInsert(t *testing.T, eng *Engine, doc, enrollment, actionName string) string {
	t.Helper()
	ins, err := domain.GetInsertable(&sbMsStudent{Name: "Ana", Document: doc, Enrollment: enrollment}, nil, actionName)
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctxFor(), ins, sbMsSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert(%s): %v", enrollment, err)
	}
	return res.ID.Value()
}

func sbMsArchive(t *testing.T, eng *Engine, id, doc string) {
	t.Helper()
	e := &sbMsStudent{Name: "Ana", Document: doc, Enrollment: "x"}
	e.SetID(domain.NewID(id))
	arch, err := domain.GetArchivable(e, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := eng.Archive(ctxFor(), arch, sbMsSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
}

func sbMsUnarchive(t *testing.T, eng *Engine, id, doc string) error {
	t.Helper()
	e := &sbMsStudent{Name: "Ana", Document: doc, Enrollment: "x"}
	e.SetID(domain.NewID(id))
	un, err := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	return eng.Unarchive(ctxFor(), un, sbMsSchema(), core.WriteHook{})
}

func sbMsCount(t *testing.T, raw *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := raw.QueryRowContext(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func TestSQLServer_SharedBaseSeparateFK_ArchivedRemnantAdmitsNewActive(t *testing.T) {
	eng, raw := sbMsSetup(t)

	s1 := sbMsInsert(t, eng, "D1", "M1", "GetInsertable")
	sbMsArchive(t, eng, s1, "D1")
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_persons WHERE deleted_at IS NULL`); got != 0 {
		t.Fatalf("archiving the only role must archive the base, active persons = %d", got)
	}

	s2 := sbMsInsert(t, eng, "D1", "M2", "GetUpsertable")
	if s1 == s2 {
		t.Fatalf("the new role must be a NEW row (fresh surrogate id), both = %s", s1)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_students`); got != 2 {
		t.Errorf("expected 2 role rows (remnant + active), got %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_students WHERE deleted_at IS NULL`); got != 1 {
		t.Errorf("expected exactly 1 ACTIVE role row, got %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_persons`); got != 1 {
		t.Errorf("expected ONE shared identity, got %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_persons WHERE deleted_at IS NULL`); got != 1 {
		t.Errorf("the new active role must revive the base, active persons = %d", got)
	}
}

func TestSQLServer_SharedBaseSeparateFK_UnarchiveNextToActiveIs409(t *testing.T) {
	eng, raw := sbMsSetup(t)

	s1 := sbMsInsert(t, eng, "D1", "M1", "GetInsertable")
	sbMsArchive(t, eng, s1, "D1")
	sbMsInsert(t, eng, "D1", "M2", "GetUpsertable")

	err := sbMsUnarchive(t, eng, s1, "D1")
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("unarchiving next to an active sibling must be a conflict NotificationCarrier, got %T (%v)", err, err)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_students WHERE deleted_at IS NULL`); got != 1 {
		t.Errorf("the vetoed unarchive must leave exactly 1 active role, got %d", got)
	}
}

func TestSQLServer_SharedBaseSeparateFK_UnarchiveWithoutSiblingRevives(t *testing.T) {
	eng, raw := sbMsSetup(t)

	s1 := sbMsInsert(t, eng, "D1", "M1", "GetInsertable")
	sbMsArchive(t, eng, s1, "D1")

	if err := sbMsUnarchive(t, eng, s1, "D1"); err != nil {
		t.Fatalf("Unarchive without sibling: %v", err)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_students WHERE deleted_at IS NULL`); got != 1 {
		t.Errorf("expected the remnant revived, active roles = %d", got)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_persons WHERE deleted_at IS NULL`); got != 1 {
		t.Errorf("the revived role must revive the base, active persons = %d", got)
	}
}

func TestSQLServer_SharedBaseSeparateFK_NaturalKeyGuard(t *testing.T) {
	eng, raw := sbMsSetup(t)

	s1 := sbMsInsert(t, eng, "D1", "M1", "GetInsertable")

	mutate := &sbMsStudent{Name: "Ana", Document: "D-CHANGED", Enrollment: "M1"}
	mutate.SetID(domain.NewID(s1))
	upd, err := domain.GetUpdatable(mutate, func(*sbMsStudent) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	_, err = eng.Update(ctxFor(), upd, sbMsSchema(), core.WriteHook{})
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("a mutated natural key must be a NotificationCarrier, got %T (%v)", err, err)
	}
	if got := sbMsCount(t, raw, `SELECT COUNT(*) FROM sb_persons`); got != 1 {
		t.Errorf("the rejected update must not create a second identity, persons = %d", got)
	}
	var doc string
	if err := raw.QueryRowContext(context.Background(), `SELECT document FROM sb_persons`).Scan(&doc); err != nil {
		t.Fatalf("read document: %v", err)
	}
	if doc != "D1" {
		t.Errorf("the persisted natural key must stay D1, got %q", doc)
	}

	legit := &sbMsStudent{Name: "Ana Maria", Document: "D1", Enrollment: "M1-NEW"}
	legit.SetID(domain.NewID(s1))
	upd2, err := domain.GetUpdatable(legit, func(*sbMsStudent) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable(legit): %v", err)
	}
	if _, err := eng.Update(ctxFor(), upd2, sbMsSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("a same-key update must pass the guard, got %v", err)
	}
	var enr string
	if err := raw.QueryRowContext(context.Background(), `SELECT enrollment FROM sb_students WHERE deleted_at IS NULL`).Scan(&enr); err != nil {
		t.Fatalf("read enrollment: %v", err)
	}
	if enr != "M1-NEW" {
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
	base := core.NewSharedBase("sbp_persons").
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

// TestSQLServer_SharedBase_PurgeAndVeto proves BOTH savepoint legs of the
// database-vetoable orphan purge on a real SQL Server — the T-SQL flavor
// (SAVE TRANSACTION / ROLLBACK TRANSACTION / no release) the audit e2e suite
// caught missing: (1) deleting the last role purges the base (the savepoint is
// simply discarded at COMMIT — the empty-release path); (2) with a foreign
// table still referencing the person, the FK violation (error 547) vetoes the
// purge — role delete commits, base stays.
func TestSQLServer_SharedBase_PurgeAndVeto(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()
	for _, stmt := range []string{
		`CREATE TABLE sbp_persons (
			id BINARY(16) NOT NULL PRIMARY KEY,
			document VARCHAR(64) NOT NULL UNIQUE,
			name NVARCHAR(255) NOT NULL
		)`,
		`CREATE TABLE sbp_students (
			id BINARY(16) NOT NULL PRIMARY KEY,
			person_id BINARY(16) NOT NULL,
			enrollment VARCHAR(64) NOT NULL,
			CONSTRAINT fk_sbp_student_person FOREIGN KEY (person_id) REFERENCES sbp_persons (id) ON DELETE NO ACTION
		)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	count := func(q string) int {
		var n int
		if err := raw.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}
	insert := func(doc string) string {
		ins, err := domain.GetInsertable(&sbpStudent{Name: "Ana", Document: doc, Enrollment: "M1"}, nil, "GetInsertable")
		if err != nil {
			t.Fatalf("GetInsertable: %v", err)
		}
		res, err := eng.Insert(ctx, ins, sbpSchema(), core.WriteHook{})
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
		if err := eng.Delete(ctx, d, sbpSchema(), core.WriteHook{}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	// Leg 1: purge succeeds — savepoint opened, work kept, discarded at COMMIT.
	s1 := insert("DP1")
	del(s1, "DP1")
	if got := count(`SELECT COUNT(*) FROM sbp_persons`); got != 0 {
		t.Fatalf("purge leg: base must be gone, persons = %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM sbp_students`); got != 0 {
		t.Fatalf("purge leg: role must be gone, students = %d", got)
	}

	// Leg 2: an UNREGISTERED table references the person → FK 547 vetoes the
	// purge (ROLLBACK TRANSACTION to the savepoint), the role delete commits.
	s2 := insert("DP2")
	if _, err := raw.ExecContext(ctx, `CREATE TABLE sbp_external_refs (
		person_id BINARY(16) NOT NULL,
		CONSTRAINT fk_sbp_external_person FOREIGN KEY (person_id) REFERENCES sbp_persons (id) ON DELETE NO ACTION
	)`); err != nil {
		t.Fatalf("ddl ext: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO sbp_external_refs (person_id) SELECT id FROM sbp_persons`); err != nil {
		t.Fatalf("seed ext: %v", err)
	}
	del(s2, "DP2")
	if got := count(`SELECT COUNT(*) FROM sbp_students`); got != 0 {
		t.Fatalf("veto leg: role delete must commit, students = %d", got)
	}
	if got := count(`SELECT COUNT(*) FROM sbp_persons`); got != 1 {
		t.Fatalf("veto leg: base must survive the vetoed purge, persons = %d", got)
	}
}
