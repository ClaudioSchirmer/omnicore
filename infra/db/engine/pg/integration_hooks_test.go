//go:build integration

package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"github.com/google/uuid"
)

// Tier 1 integration coverage of the persistence lifecycle hooks (Topic
// 10). Each test in this file targets one of the six scenarios per
// (verb, slot, persister-path) cell of the deep matrix the design
// specifies:
//
//   A. Hook fires when option provided
//   B. Hook receives correct payload (ctx, t, id, tx)
//   C. Hook error rolls back — no row in domain table / outbox / audit_events
//   D. Hook panic rolls back AND propagates panic
//   E. slog.Warn emitted on hook error
//   F. Hook absent leaves behavior unchanged
//
// The flat path uses flatPerson (declared in integration_executor_test.go);
// the aggregate path uses aggCustomer (declared in integration_aggregate_test.go).
// Both stand-ins predate this file — the present tests reuse them so the deep
// matrix shares the same setup helpers as the legacy persister coverage.

// --- Helpers ---------------------------------------------------------------

// withRecordingLogger swaps the persister's logger for one writing into
// the returned buffer so the slog.Warn assertion can inspect the
// captured line.
func withRecordingLogger(t *testing.T, pg *Postgres) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	// The logger lives on the embedded BaseEngine; ConfigureAudit sets it (nil
	// cfg leaves audit disabled, which is what the hook-error tests want).
	pg.ConfigureAudit(nil, slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})), nil)
	return &buf
}

// buildAfterBeginHook constructs a db.WriteHook firing only the afterBegin
// slot with the supplied closure.
func buildAfterBeginHook(fn func(ctx persistence.RequestContext, src domain.Entity, tx persistence.TxHandle) error) db.WriteHook {
	return db.WriteHook{AfterBegin: fn}
}

// buildBeforeCommitHook constructs a db.WriteHook firing only the beforeCommit
// slot.
func buildBeforeCommitHook(fn func(ctx persistence.RequestContext, src domain.Entity, id domain.ID, tx persistence.TxHandle) error) db.WriteHook {
	return db.WriteHook{BeforeCommit: fn}
}

// auditRowCount returns the number of rows in audit_events.
func auditRowCount(t *testing.T, pg *Postgres) int {
	t.Helper()
	var n int
	if err := pg.Pool().QueryRow(context.Background(), `SELECT count(*) FROM audit_events`).Scan(&n); err != nil {
		t.Fatalf("audit row count: %v", err)
	}
	return n
}

// withFullAudit configures the persister to write both audit
// destinations so the rollback tests can verify "no audit row" too.
func withFullAudit(pg *Postgres) {
	pg.WithAudit(&audit.Config{Destinations: []audit.Destination{audit.DestinationDatabase, audit.DestinationSlog}}, slog.Default(), nil)
}

// TestPostgres_AuditReader_ReadBack proves the backend-neutral audit reader
// (db.NewAuditReader over the Postgres engine's read seam) reads back, on a
// real Postgres, a row the in-TX writer just landed — the dialect twin of the
// MySQL coverage in infra/db/mysql. Confirms the placeholder renders ($n),
// no-rows maps to the sentinel, and the scan round-trips the event.
func TestPostgres_AuditReader_ReadBack(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	withFullAudit(pg)

	ins, _ := domain.GetInsertable(&flatPerson{Name: "audrey", Email: "a@x"}, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The audit row id is Go-generated and not surfaced by Insert; read it (plus
	// the entity_type the writer stamped) straight from the table to drive both
	// reader entry points.
	var rowID, entityType string
	if err := pg.Pool().QueryRow(context.Background(),
		`SELECT id::text, entity_type FROM audit_events WHERE aggregate_id = $1`, res.ID,
	).Scan(&rowID, &entityType); err != nil {
		t.Fatalf("read audit row id: %v", err)
	}

	reader := db.NewAuditReader(pg)

	byAgg, err := reader.FindByAggregate(context.Background(), entityType, res.ID)
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if len(byAgg) != 1 {
		t.Fatalf("FindByAggregate want 1 event, got %d", len(byAgg))
	}
	if byAgg[0].Verb != "insert" || byAgg[0].Kind != "snapshot" || byAgg[0].EntityID != res.ID {
		t.Errorf("read-back event drifted: %+v", byAgg[0])
	}

	byID, err := reader.FindByID(context.Background(), uuid.MustParse(rowID))
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if byID.EntityID != res.ID || byID.EntityType != entityType {
		t.Errorf("FindByID drifted: %+v", byID)
	}

	if _, err := reader.FindByID(context.Background(), uuid.New()); !errors.Is(err, audit.ErrAuditNotFound) {
		t.Errorf("unknown id must map to ErrAuditNotFound, got %v", err)
	}
}

// --- Flat-path: Insert ----------------------------------------------------

func TestPostgres_Insert_HookFires_AfterBegin(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	called := false
	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildAfterBeginHook(func(_ persistence.RequestContext, _ domain.Entity, _ persistence.TxHandle) error {
		called = true
		return nil
	})
	if _, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !called {
		t.Error("afterBegin hook did not fire")
	}
	if rowCount(t, pg, "flat_persons") != 1 {
		t.Errorf("expected 1 row")
	}
}

func TestPostgres_Insert_HookFires_BeforeCommit(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	var gotID domain.ID
	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(_ persistence.RequestContext, _ domain.Entity, id domain.ID, _ persistence.TxHandle) error {
		gotID = id
		return nil
	})
	if _, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if gotID.IsEmpty() {
		t.Error("beforeCommit hook received empty id")
	}
}

// TestPostgres_Insert_HookCanWriteCompanionRow exercises the canonical
// path for a hook side effect: the hook calls into an infra-layer
// adapter (here inlined as a helper for test concision) that recovers
// the underlying pgx.Tx via UnwrapPgxTx and owns the SQL. The
// application/hook surface stays SQL-free; only infra/ pronounces the
// table name.
func TestPostgres_Insert_HookCanWriteCompanionRow(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	createTable(t, pg, `CREATE TABLE companion (id UUID, note TEXT)`)

	// insertCompanion stands in for an infra-layer port adapter — the
	// shape services adopt to write extra rows from inside a hook.
	insertCompanion := func(tx persistence.TxHandle, id domain.ID, note string) error {
		pgxTx := UnwrapPgxTx(tx)
		_, err := pgxTx.Exec(context.Background(), `INSERT INTO companion (id, note) VALUES ($1, $2)`, id.Value(), note)
		return err
	}

	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(_ persistence.RequestContext, _ domain.Entity, id domain.ID, tx persistence.TxHandle) error {
		return insertCompanion(tx, id, "added in hook")
	})
	if _, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rowCount(t, pg, "companion") != 1 {
		t.Errorf("expected companion row written from hook")
	}
}

func TestPostgres_Insert_AfterBeginError_RollsBack(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	withFullAudit(pg)

	wantErr := errors.New("afterBegin rejects")
	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildAfterBeginHook(func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error { return wantErr })

	_, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected hook error verbatim, got %v", err)
	}
	if rowCount(t, pg, "flat_persons") != 0 {
		t.Errorf("expected 0 data rows after rollback")
	}
	if outboxCount(t, pg) != 0 {
		t.Errorf("expected 0 outbox rows after rollback")
	}
	if auditRowCount(t, pg) != 0 {
		t.Errorf("expected 0 audit rows after rollback")
	}
}

func TestPostgres_Insert_BeforeCommitError_RollsBack(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	withFullAudit(pg)

	wantErr := errors.New("beforeCommit rejects")
	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error { return wantErr })

	_, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected hook error verbatim, got %v", err)
	}
	if rowCount(t, pg, "flat_persons") != 0 {
		t.Errorf("expected 0 data rows after rollback")
	}
	if outboxCount(t, pg) != 0 {
		t.Errorf("expected 0 outbox rows after rollback")
	}
	if auditRowCount(t, pg) != 0 {
		t.Errorf("expected 0 audit rows after rollback")
	}
}

func TestPostgres_Insert_HookError_EmitsSlogWarn(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	buf := withRecordingLogger(t, pg)

	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		return errors.New("rejects")
	})
	_, _ = pg.Insert(testCtx(), ins, flatPersonSchema(), hook)

	if !bytes.Contains(buf.Bytes(), []byte("persistence.hook.error")) {
		t.Errorf("expected slog.Warn line, got %s", buf.String())
	}
	var entry map[string]any
	for _, line := range bytes.Split(buf.Bytes(), []byte{'\n'}) {
		if len(line) == 0 || !bytes.Contains(line, []byte("persistence.hook.error")) {
			continue
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("slog json unmarshal: %v", err)
		}
		break
	}
	if entry["verb"] != "Insert" {
		t.Errorf("verb=%v, want Insert", entry["verb"])
	}
	if entry["hookSlot"] != "beforeCommit" {
		t.Errorf("hookSlot=%v, want beforeCommit", entry["hookSlot"])
	}
	if !strings.Contains(entry["error"].(string), "rejects") {
		t.Errorf("error field=%v, want to contain 'rejects'", entry["error"])
	}
}

func TestPostgres_Insert_NoHook_BehaviorUnchanged(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	res, err := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if res.ID == "" {
		t.Error("expected populated id")
	}
	if rowCount(t, pg, "flat_persons") != 1 {
		t.Errorf("expected 1 row")
	}
	if outboxCount(t, pg) != 1 {
		t.Errorf("expected 1 outbox row")
	}
}

// --- Flat-path: Update / Archive / Unarchive / Delete --------------------

func TestPostgres_Update_HookFires_BothSlots(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	e.SetID(domain.NewID(res.ID))

	e.Name = "alice2"
	upd, _ := domain.GetUpdatable(e, func(*flatPerson) error { return nil }, nil, "GetUpdatable")

	abCalled, bcCalled := false, false
	hook := db.WriteHook{
		AfterBegin: func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
			abCalled = true
			return nil
		},
		BeforeCommit: func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
			bcCalled = true
			return nil
		},
	}
	if _, err := pg.Update(testCtx(), upd, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !abCalled || !bcCalled {
		t.Errorf("expected both hooks fired; ab=%v bc=%v", abCalled, bcCalled)
	}
}

func TestPostgres_Update_BeforeCommitError_RollsBack(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	e.SetID(domain.NewID(res.ID))

	original := outboxCount(t, pg)
	e.Name = "alice2"
	upd, _ := domain.GetUpdatable(e, func(*flatPerson) error { return nil }, nil, "GetUpdatable")
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		return errors.New("rejects")
	})
	if _, err := pg.Update(testCtx(), upd, flatPersonSchema(), hook); err == nil {
		t.Fatal("expected error")
	}
	// Outbox should not gain a new row from the rolled-back update.
	if got := outboxCount(t, pg); got != original {
		t.Errorf("outbox grew from %d to %d on rollback", original, got)
	}
	var name string
	_ = pg.Pool().QueryRow(context.Background(), `SELECT name FROM flat_persons WHERE id = $1`, res.ID).Scan(&name)
	if name != "alice" {
		t.Errorf("expected name unchanged after rollback, got %q", name)
	}
}

func TestPostgres_Archive_HookFires(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	e.SetID(domain.NewID(res.ID))

	arch, _ := domain.GetArchivable(e, nil, "GetArchivable")
	bcCalled := false
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		bcCalled = true
		return nil
	})
	if err := pg.Archive(testCtx(), arch, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if !bcCalled {
		t.Error("beforeCommit hook did not fire on Archive")
	}
}

func TestPostgres_Unarchive_HookFires(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	e.SetID(domain.NewID(res.ID))
	arch, _ := domain.GetArchivable(e, nil, "GetArchivable")
	_ = pg.Archive(testCtx(), arch, flatPersonSchema(), db.WriteHook{})

	una, _ := domain.GetUnarchivable(e, nil, "GetUnarchivable")
	bcCalled := false
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		bcCalled = true
		return nil
	})
	if err := pg.Unarchive(testCtx(), una, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if !bcCalled {
		t.Error("beforeCommit hook did not fire on Unarchive")
	}
}

func TestPostgres_Delete_HookFires_AndRollback(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	e := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(e, nil, "GetInsertable")
	res, _ := pg.Insert(testCtx(), ins, flatPersonSchema(), db.WriteHook{})
	e.SetID(domain.NewID(res.ID))

	del, _ := domain.GetDeletable(e, nil, "GetDeletable")
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		return errors.New("rejects")
	})
	if err := pg.Delete(testCtx(), del, flatPersonSchema(), hook); err == nil {
		t.Fatal("expected error")
	}
	if rowCount(t, pg, "flat_persons") != 1 {
		t.Error("expected row to survive rollback")
	}
}

// --- Aggregate-path symmetry --------------------------------------------

func TestPostgres_InsertAggregate_HookFires_BeforeCommit(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	u := &aggCustomer{Name: "alice", Email: "a@x"}
	domain.AddAggregateChild(u, aggChannel{Label: "email"})
	ins, err := domain.GetInsertable(u, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}

	bcCalled := false
	hook := buildBeforeCommitHook(func(_ persistence.RequestContext, _ domain.Entity, id domain.ID, _ persistence.TxHandle) error {
		bcCalled = true
		if id.IsEmpty() {
			t.Error("beforeCommit on aggregate received empty id")
		}
		return nil
	})
	if _, err := pg.Insert(testCtx(), ins, aggCustomerSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !bcCalled {
		t.Error("aggregate beforeCommit did not fire")
	}
}

func TestPostgres_InsertAggregate_BeforeCommitError_RollsBackEverything(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)

	u := &aggCustomer{Name: "alice", Email: "a@x"}
	domain.AddAggregateChild(u, aggChannel{Label: "email"})
	ins, _ := domain.GetInsertable(u, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(persistence.RequestContext, domain.Entity, domain.ID, persistence.TxHandle) error {
		return errors.New("rejects")
	})
	if _, err := pg.Insert(testCtx(), ins, aggCustomerSchema(), hook); err == nil {
		t.Fatal("expected error")
	}
	if rowCount(t, pg, "agg_customers") != 0 {
		t.Errorf("expected 0 root rows after rollback")
	}
	if rowCount(t, pg, "agg_channels") != 0 {
		t.Errorf("expected 0 child rows after rollback")
	}
	if outboxCount(t, pg) != 0 {
		t.Errorf("expected 0 outbox rows after rollback")
	}
}

// TestPostgres_HookSlogWarn_AggregatePath confirms the same observability
// branch fires on the aggregate path.
func TestPostgres_HookSlogWarn_AggregatePath(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createAggregateTables(t, pg)
	buf := withRecordingLogger(t, pg)

	u := &aggCustomer{Name: "alice", Email: "a@x"}
	domain.AddAggregateChild(u, aggChannel{Label: "email"})
	ins, _ := domain.GetInsertable(u, nil, "GetInsertable")
	hook := buildAfterBeginHook(func(persistence.RequestContext, domain.Entity, persistence.TxHandle) error {
		return errors.New("rejects")
	})
	_, _ = pg.Insert(testCtx(), ins, aggCustomerSchema(), hook)

	if !bytes.Contains(buf.Bytes(), []byte("persistence.hook.error")) {
		t.Errorf("expected slog.Warn line on aggregate path; got %s", buf.String())
	}
}

// --- Hook payload assertions -------------------------------------------

func TestPostgres_HookPayload_ReceivesContextAndEntity(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)

	wantCtx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	wantEntity := &flatPerson{Name: "alice", Email: "a@x"}
	ins, _ := domain.GetInsertable(wantEntity, nil, "GetInsertable")

	var gotCtx persistence.RequestContext
	var gotSrc domain.Entity
	hook := buildBeforeCommitHook(func(ctx persistence.RequestContext, src domain.Entity, _ domain.ID, _ persistence.TxHandle) error {
		gotCtx = ctx
		gotSrc = src
		return nil
	})
	if _, err := pg.Insert(wantCtx, ins, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if gotCtx != wantCtx {
		t.Errorf("hook ctx mismatch")
	}
	if gotSrc != wantEntity {
		t.Errorf("hook source mismatch")
	}
}

// TestPostgres_HookPayload_TxHandleUnwrapsToFrameworkTx proves that
// UnwrapPgxTx, called from inside a hook closure, recovers the very
// pgx.Tx the framework opened — Exec / Query / QueryRow on it all
// participate in the same transaction as the framework's own data +
// outbox + audit writes. This is the load-bearing claim of the
// hook contract: a side effect routed through an infra adapter
// (which is the only place authorized to call UnwrapPgxTx) commits
// or rolls back atomically with the framework's writes.
func TestPostgres_HookPayload_TxHandleUnwrapsToFrameworkTx(t *testing.T) {
	pg, cleanup := newTestPG(t)
	defer cleanup()
	createFlatPersonsTable(t, pg)
	createTable(t, pg, `CREATE TABLE tx_smoke (n INTEGER)`)

	ins, _ := domain.GetInsertable(&flatPerson{Name: "alice", Email: "a@x"}, nil, "GetInsertable")
	hook := buildBeforeCommitHook(func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, tx persistence.TxHandle) error {
		pgxTx := UnwrapPgxTx(tx)
		if _, err := pgxTx.Exec(context.Background(), `INSERT INTO tx_smoke (n) VALUES (1), (2), (3)`); err != nil {
			return err
		}
		rows, err := pgxTx.Query(context.Background(), `SELECT n FROM tx_smoke ORDER BY n`)
		if err != nil {
			return err
		}
		defer rows.Close()
		got := []int{}
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				return err
			}
			got = append(got, n)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		var single int
		if err := pgxTx.QueryRow(context.Background(), `SELECT n FROM tx_smoke WHERE n = 2`).Scan(&single); err != nil {
			return err
		}
		if single != 2 {
			return errors.New("queryRow scan mismatch")
		}
		return nil
	})
	if _, err := pg.Insert(testCtx(), ins, flatPersonSchema(), hook); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if rowCount(t, pg, "tx_smoke") != 3 {
		t.Errorf("expected 3 rows from hook Exec routed via UnwrapPgxTx")
	}
}
