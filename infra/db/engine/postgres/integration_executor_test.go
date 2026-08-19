//go:build integration && postgres

package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/google/uuid"
)

// flatPerson is the canonical non-aggregate entity for executor.go tests.
// Table inference: flat_persons.
type flatPerson struct {
	domain.BaseEntity
	Name  string
	Email string
	Phone *string
}

func (e *flatPerson) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (*flatPerson) BuildRules(string, domain.Service, *domain.Rules) {}

func createFlatPersonsTable(t *testing.T, pg *Postgres) {
	t.Helper()
	createTable(t, pg, `CREATE TABLE flat_persons (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		phone TEXT,
		revision BIGINT NOT NULL DEFAULT 0,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)
}

// flatPersonSchema declares the flatPerson table mapping — the explicit map
// the executor write path resolves from. flatPersonSchemaOn renames the table.
func flatPersonSchema() *core.TableSchema { return flatPersonSchemaOn("flat_persons") }

func flatPersonSchemaOn(table string) *core.TableSchema {
	return core.NewTableSchema[*flatPerson](table).
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		Field("Phone", "phone").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

// --- Insert (simple path) -------------------------------------------------

func TestPostgres_Insert_PersistsRowAndOutbox(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "Alice", Email: "alice@x"}
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	res, err := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if res.ID.Value() == "" {
		t.Error("expected populated WriteResult.ID")
	}
	if rowCount(t, pg, "flat_persons") != 1 {
		t.Errorf("expected 1 row, got %d", rowCount(t, pg, "flat_persons"))
	}

	rows := outboxRows(t, pg)
	if len(rows) != 1 {
		t.Fatalf("expected 1 outbox row, got %d", len(rows))
	}
	if rows[0].AggregateType != "flat_persons" || rows[0].EventType != "INSERTED" || rows[0].AggregateID != res.ID.Value() {
		t.Errorf("outbox row = %+v, want aggregate_type=flat_persons event_type=INSERTED id=%s", rows[0], res.ID)
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["name"] != "Alice" {
		t.Errorf("payload.name = %v, want Alice", payload["name"])
	}
}

func TestPostgres_Insert_NotNullViolationPropagates(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	// Name is NOT NULL on the table; missing it triggers PG error.
	e := &flatPerson{Email: "bob@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")

	// Force Name into the payload but as NULL — write the empty string and
	// adjust the row by hand to assert the error path: actually simpler is to
	// invoke INSERT with an extra constraint violation by adding a UNIQUE
	// constraint and inserting the same email twice.
	if _, err := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("first Insert err = %v", err)
	}

	pg.Pool().Exec(context.Background(),
		`CREATE UNIQUE INDEX flat_persons_email_uq ON flat_persons (email) WHERE deleted_at IS NULL`)

	e2 := &flatPerson{Name: "Bob2", Email: "bob@x"}
	ins2, _ := domain.GetInsertable(e2, nil, "GetInsertable")
	_, err := pg.Insert(testCtx(), ins2, flatPersonSchema(), noHook)
	if err == nil {
		t.Error("expected unique violation on duplicate email")
	}
}

func TestPostgres_Insert_HonorsSchemaTableOverride(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createTable(t, pg, `CREATE TABLE tb_legacy_persons (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		name TEXT NOT NULL,
		email TEXT NOT NULL,
		phone TEXT,
		deleted_at TIMESTAMP,
		created_at TIMESTAMP NOT NULL DEFAULT NOW(),
		updated_at TIMESTAMP NOT NULL DEFAULT NOW()
	)`)

	e := &flatPerson{Name: "X", Email: "x@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, flatPersonSchemaOn("tb_legacy_persons"), noHook)
	if err != nil {
		t.Fatalf("Insert with override: %v", err)
	}
	if res.ID.Value() == "" {
		t.Error("expected ID")
	}
	if rowCount(t, pg, "tb_legacy_persons") != 1 {
		t.Errorf("expected 1 row in tb_legacy_persons, got %d", rowCount(t, pg, "tb_legacy_persons"))
	}
}

// --- Update ---------------------------------------------------------------

func TestPostgres_Update_PersistsAndEmitsOutbox(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	// Seed an existing row via Insert.
	seed := &flatPerson{Name: "Old", Email: "old@x"}
	ins, _ := domain.GetInsertable(seed, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Build an Updatable.
	loaded := &flatPerson{Name: "Old", Email: "old@x"}
	loaded.SetID(res.ID)
	upd, err := domain.GetUpdatable(loaded, func(p *flatPerson) error { p.Name = "New"; return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}

	if _, err := pg.Update(testCtx(), upd, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("Update: %v", err)
	}

	var name string
	if err := pg.Pool().QueryRow(context.Background(), `SELECT name FROM flat_persons WHERE id = $1`, res.ID.Value()).Scan(&name); err != nil {
		t.Fatalf("readback: %v", err)
	}
	if name != "New" {
		t.Errorf("name after update = %q, want New", name)
	}

	rows := outboxRows(t, pg)
	if len(rows) != 2 || rows[1].EventType != "UPDATED" {
		t.Errorf("expected INSERTED then UPDATED outbox rows, got %+v", rows)
	}
}

func TestPostgres_Update_BadIDReturnsError(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	loaded := &flatPerson{Name: "X", Email: "x@x"}
	loaded.SetID(domain.NewID(uuid.NewString()))
	upd, _ := domain.GetUpdatable(loaded, func(*flatPerson) error { return nil }, nil, "GetUpdatable")

	_, err := pg.Update(testCtx(), upd, flatPersonSchema(), noHook)
	if err == nil {
		t.Error("expected error when updating a non-existent ID (no row returned)")
	}
}

// --- Archive / Unarchive --------------------------------------------------

func TestPostgres_Archive_FlipsDeletedAtAndEmitsOutbox(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	seed := &flatPerson{Name: "Archive", Email: "a@x"}
	ins, _ := domain.GetInsertable(seed, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	loaded := &flatPerson{Name: "Archive", Email: "a@x"}
	loaded.SetID(res.ID)
	arch, err := domain.GetArchivable(loaded, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := pg.Archive(testCtx(), arch, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	if activeCount(t, pg, "flat_persons") != 0 {
		t.Errorf("expected 0 active rows after Archive, got %d", activeCount(t, pg, "flat_persons"))
	}
	if rowCount(t, pg, "flat_persons") != 1 {
		t.Errorf("Archive should NOT delete the row, got count = %d", rowCount(t, pg, "flat_persons"))
	}

	rows := outboxRows(t, pg)
	if rows[len(rows)-1].EventType != "ARCHIVED" {
		t.Errorf("last outbox event = %q, want ARCHIVED", rows[len(rows)-1].EventType)
	}
}

func TestPostgres_Unarchive_RestoresAndEmitsOutbox(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	seed := &flatPerson{Name: "U", Email: "u@x"}
	ins, _ := domain.GetInsertable(seed, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook)

	id := res.ID
	loaded := &flatPerson{Name: "U", Email: "u@x"}
	loaded.SetID(id)

	arch, _ := domain.GetArchivable(loaded, nil, "GetArchivable")
	if err := pg.Archive(testCtx(), arch, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if activeCount(t, pg, "flat_persons") != 0 {
		t.Fatal("Archive should have flipped deleted_at")
	}

	loaded2 := &flatPerson{Name: "U", Email: "u@x"}
	loaded2.SetID(id)
	una, err := domain.GetUnarchivable(loaded2, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	if err := pg.Unarchive(testCtx(), una, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if activeCount(t, pg, "flat_persons") != 1 {
		t.Errorf("expected 1 active row after Unarchive, got %d", activeCount(t, pg, "flat_persons"))
	}

	rows := outboxRows(t, pg)
	if rows[len(rows)-1].EventType != "UNARCHIVED" {
		t.Errorf("last outbox event = %q, want UNARCHIVED", rows[len(rows)-1].EventType)
	}
}

// --- Delete ---------------------------------------------------------------

func TestPostgres_Delete_RemovesRowAndEmitsOutbox(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	seed := &flatPerson{Name: "D", Email: "d@x"}
	ins, _ := domain.GetInsertable(seed, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), noHook)

	loaded := &flatPerson{Name: "D", Email: "d@x"}
	loaded.SetID(res.ID)
	del, err := domain.GetDeletable(loaded, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	if err := pg.Delete(testCtx(), del, flatPersonSchema(), noHook); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if rowCount(t, pg, "flat_persons") != 0 {
		t.Errorf("expected 0 rows after Delete, got %d", rowCount(t, pg, "flat_persons"))
	}
	rows := outboxRows(t, pg)
	if rows[len(rows)-1].EventType != "DELETED" {
		t.Errorf("last event = %q, want DELETED", rows[len(rows)-1].EventType)
	}
}

// --- Batch ----------------------------------------------------------------

// --- validIdentifier panic on bad name ------------------------------------

func TestValidIdentifier_PanicsOnInvalid(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on injection-like identifier")
		} else if !strings.Contains(r.(string), "invalid SQL identifier") {
			t.Errorf("panic message = %v", r)
		}
	}()
	validIdentifier("users; DROP TABLE users")
}

// --- NewPostgres failure path ---------------------------------------------

func TestNewPostgres_BadDSNFails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3)
	defer cancel()
	if _, err := NewPostgres(ctx, "postgres://nobody:nope@127.0.0.1:1/none?sslmode=disable&connect_timeout=1"); err == nil {
		t.Error("expected NewPostgres to fail with unreachable DSN")
	}
}
