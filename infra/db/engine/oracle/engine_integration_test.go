//go:build integration && oracle

package oracle

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/read"
	"github.com/ClaudioSchirmer/omnicore/infra/db/command/write"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/db/criteria"
)

// Integration tests for the Oracle engine's synchronous write path against a
// real Oracle Free 23ai container (the QA bench's `oracle` profile service,
// host :15211, SYSTEM password OmnicoreQA!2026 by convention).
//
//	go test -tags=integration,oracle ./infra/db/engine/oracle/ -count=1
//
// Each test creates a THROW-AWAY SCHEMA — Oracle's databases are heavyweight
// PDBs, so the per-test isolation unit is a user/schema (CREATE USER … / DROP
// USER … CASCADE), the platform's idiomatic equivalent of the pg/mysql/mssql
// throw-away databases. That requires an admin-capable connection (SYSTEM);
// override it via OMNICORE_TEST_ORACLE_ADMIN_DSN. The harness grants the test
// user EXECUTE ON DBMS_LOCK (the engine's documented operational requirement)
// and SELECT_CATALOG_ROLE (the rebuild lock's best-effort holder diagnostic).
// Verifies each write verb persists the row (UUID v7 round-tripping through
// RAW(16)) and lands the matching outbox row in the same TX.

// The admin connection is SYS AS SYSDBA (go-ora's `dba privilege` option):
// the harness grants EXECUTE ON SYS.DBMS_LOCK to each throw-away user, and
// only SYS can grant a SYS-owned object.
const defaultOracleAdminDSN = "oracle://sys:OmnicoreQA!2026@127.0.0.1:15211/FREEPDB1?dba+privilege=sysdba"

func oracleAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_ORACLE_ADMIN_DSN"); v != "" {
		return v
	}
	return defaultOracleAdminDSN
}

// testDSN is the DSN of the current test's throw-away schema, set by
// newTestOracleSchema. The tests in this package do not use t.Parallel, so the
// package variable is race-free; dsn() hands it to tests that open a SECOND
// connection to the same schema (e.g. the tracing engine).
var testDSN string

func dsn() string { return testDSN }

// newTestOracleSchema creates a throw-away user/schema on the bench Oracle,
// points testDSN at it and registers its DROP as cleanup (LIFO: callers' own
// cleanups — closing engines/connections — run first; the drop retries while
// a straggler session still pins the user). Skips the test when Oracle is
// unreachable.
func newTestOracleSchema(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	admin, err := sql.Open("oracle", oracleAdminDSN())
	if err != nil {
		t.Skipf("Oracle not reachable (%v) — start the QA bench's oracle profile", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("Oracle not reachable (%v) — start the QA bench's oracle profile", err)
	}

	user := "omnicore_ora_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	const pw = "OmnicoreQA_2026"
	for _, stmt := range []string{
		fmt.Sprintf("CREATE USER %s IDENTIFIED BY %s QUOTA UNLIMITED ON users", user, pw),
		"GRANT CREATE SESSION, CREATE TABLE TO " + user,
		"GRANT EXECUTE ON SYS.DBMS_LOCK TO " + user,
		"GRANT SELECT_CATALOG_ROLE TO " + user,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			_ = admin.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	adminURL, err := url.Parse(oracleAdminDSN())
	if err != nil {
		t.Fatalf("parse admin DSN: %v", err)
	}
	adminURL.User = url.UserPassword(user, pw)
	adminURL.RawQuery = "" // the admin's options (dba privilege) must not leak to the test user
	testDSN = adminURL.String()

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// A pooled session may take a beat to fully log off after Close —
		// retry the drop through ORA-01940 (user currently connected).
		var dropErr error
		for i := 0; i < 5; i++ {
			if _, dropErr = admin.ExecContext(c, "DROP USER "+user+" CASCADE"); dropErr == nil {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
		if dropErr != nil {
			t.Logf("DROP USER %s CASCADE: %v (leftover schema on the bench)", user, dropErr)
		}
		_ = admin.Close()
		testDSN = ""
	})
	return testDSN
}

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

func flatSchema() *core.TableSchema {
	return core.NewTableSchema[*flatPerson]("flat_persons").
		PK("id").
		Field("Name", "name").
		Field("Email", "email").
		Field("Phone", "phone").
		SoftDelete("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

// refEntity carries a secondary identity reference (TenantID) typed
// domain.ID — the field TYPE is the declaration: it pairs with a RAW(16)
// column that is neither the PK nor an aggregate FK, binding as 16 bytes on
// write and restoring canonical through the id scan proxy on read.
type refEntity struct {
	domain.BaseEntity
	Name     string
	TenantID domain.ID
}

func (e *refEntity) Modes() []domain.EntityMode {
	return []domain.EntityMode{domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete}
}
func (*refEntity) BuildRules(string, domain.Service, *domain.Rules) {}

func refSchema() *core.TableSchema {
	return core.NewTableSchema[*refEntity]("refs").
		PK("id").
		Field("Name", "name").
		Field("TenantID", "tenant_id")
}

// A secondary RAW(16) identity column (not the PK, not an aggregate FK) typed
// domain.ID must round-trip: written as 16 bytes (the typed EncodeArg case)
// and auto-scanned back to the canonical value (the id scan proxy) —
// Postgres/MySQL/SQL Server parity for a cross-aggregate reference.
func TestOracleEngine_SecondaryUUIDColumn(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `CREATE TABLE refs (
		id RAW(16) NOT NULL PRIMARY KEY,
		name VARCHAR2(255 CHAR) NOT NULL,
		tenant_id RAW(16) NOT NULL
	)`); err != nil {
		t.Fatalf("create refs: %v", err)
	}

	tenant := uuid.NewString()
	ins, err := domain.GetInsertable(&refEntity{Name: "Acme", TenantID: domain.NewID(tenant)}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, refSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Stored as RAW(16), not text.
	var rawTenant []byte
	if err := raw.QueryRow(`SELECT tenant_id FROM refs`).Scan(&rawTenant); err != nil {
		t.Fatalf("select tenant_id: %v", err)
	}
	if len(rawTenant) != 16 {
		t.Fatalf("tenant_id stored as %d bytes, want RAW(16)", len(rawTenant))
	}

	// Auto-scan decodes it back to the canonical UUID string.
	loader := read.NewAggregateLoader[*refEntity](eng, func() *refEntity { return &refEntity{} }).
		WithSchema(refSchema())
	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.TenantID.Value() != tenant {
		t.Fatalf("secondary id column = %q, want canonical %q (RAW(16) not decoded on scan)", got.TenantID.Value(), tenant)
	}

	// A bare-string criteria probe on the domain.ID-typed field is lifted by
	// the translator and matches the RAW(16) column.
	byRef, err := loader.FindOne(ctx, criteria.Where(criteria.Eq("TenantID", tenant)))
	if err != nil {
		t.Fatalf("FindOne by lifted criteria: %v", err)
	}
	if byRef.GetID().Value() != res.ID.Value() {
		t.Fatalf("criteria matched id %q, want %q", byRef.GetID().Value(), res.ID)
	}
}

// TestNew_AppliesPoolConfig proves EngineConfig.Pool reaches the live *sql.DB:
// an explicit MaxOpenConns caps the pool (database/sql defaults to unlimited).
func TestNew_AppliesPoolConfig(t *testing.T) {
	ctx := context.Background()
	eng, err := New(ctx, core.EngineConfig{DSN: newTestOracleSchema(t), Pool: core.PoolConfig{MaxOpenConns: 7, MaxIdleConns: 3}})
	if err != nil {
		t.Fatalf("New on the test schema: %v", err)
	}
	defer eng.Close()
	if got := eng.(*Engine).db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7 (pool config not applied to *sql.DB)", got)
	}
}

func setup(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	testDB := newTestOracleSchema(t)
	eng, err := New(ctx, core.EngineConfig{DSN: testDB})
	if err != nil {
		t.Fatalf("New on the test schema: %v", err)
	}
	raw, err := sql.Open("oracle", testDB)
	if err != nil {
		t.Fatalf("open assert conn: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE flat_persons (
			id RAW(16) NOT NULL PRIMARY KEY,
			name VARCHAR2(255 CHAR) NOT NULL,
			email VARCHAR2(255 CHAR) NOT NULL,
			phone VARCHAR2(32) NULL,
			deleted_at TIMESTAMP(6) NULL,
			created_at TIMESTAMP(6) NOT NULL,
			updated_at TIMESTAMP(6) NOT NULL,
			CONSTRAINT uniq_email UNIQUE (email)
		)`,
		`CREATE TABLE outbox (
			id RAW(16) NOT NULL PRIMARY KEY,
			aggregate_type VARCHAR2(100) NOT NULL,
			event_type VARCHAR2(50) NOT NULL,
			aggregate_id VARCHAR2(36) NOT NULL,
			payload JSON NOT NULL,
			traceparent VARCHAR2(64) NULL,
			created_at TIMESTAMP(6) NOT NULL
		)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		// The throw-away schema is dropped by newTestOracleSchema's cleanup
		// (LIFO: these closes run first); no per-table teardown needed.
		_ = raw.Close()
		eng.Close()
	})
	return eng.(*Engine), raw
}

func ctxFor() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

func outboxCount(t *testing.T, raw *sql.DB, eventType, aggID string) int {
	t.Helper()
	var n int
	if err := raw.QueryRow(
		`SELECT COUNT(*) FROM outbox WHERE event_type = :1 AND aggregate_id = :2`, eventType, aggID,
	).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	return n
}

func TestOracleEngine_WritePath(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	// --- Insert ---
	e := &flatPerson{Name: "Alice", Email: "alice@x"}
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := uuid.Parse(res.ID.Value()); err != nil {
		t.Fatalf("Insert returned non-UUID id %q: %v", res.ID, err)
	}
	id := res.ID

	// Row persisted with RAW(16) PK round-tripping back to the same UUID.
	var rawID []byte
	var name, email string
	if err := raw.QueryRow(`SELECT id, name, email FROM flat_persons`).Scan(&rawID, &name, &email); err != nil {
		t.Fatalf("select after insert: %v", err)
	}
	gotID, err := uuid.FromBytes(rawID)
	if err != nil || gotID.String() != id.Value() {
		t.Fatalf("RAW(16) PK did not round-trip: bytes=%x got=%v want=%s err=%v", rawID, gotID, id, err)
	}
	if name != "Alice" || email != "alice@x" {
		t.Fatalf("row mismatch: name=%q email=%q", name, email)
	}
	if c := outboxCount(t, raw, "INSERTED", id.Value()); c != 1 {
		t.Fatalf("expected 1 INSERTED outbox row, got %d", c)
	}

	// --- Update ---
	e2 := &flatPerson{Name: "Alice B", Email: "alice@x"}
	e2.SetID(domain.NewID(id.Value()))
	upd, err := domain.GetUpdatable(e2, func(*flatPerson) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	if _, err := eng.Update(ctx, upd, flatSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := raw.QueryRow(`SELECT name FROM flat_persons`).Scan(&name); err != nil {
		t.Fatalf("select after update: %v", err)
	}
	if name != "Alice B" {
		t.Fatalf("update did not persist: name=%q", name)
	}
	if c := outboxCount(t, raw, "UPDATED", id.Value()); c != 1 {
		t.Fatalf("expected 1 UPDATED outbox row, got %d", c)
	}

	// --- Archive (soft delete set) ---
	ea := &flatPerson{Name: "Alice B", Email: "alice@x"}
	ea.SetID(domain.NewID(id.Value()))
	arch, err := domain.GetArchivable(ea, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := eng.Archive(ctx, arch, flatSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	var deletedAt sql.NullTime
	if err := raw.QueryRow(`SELECT deleted_at FROM flat_persons`).Scan(&deletedAt); err != nil {
		t.Fatalf("select after archive: %v", err)
	}
	if !deletedAt.Valid {
		t.Fatal("archive did not set deleted_at")
	}
	if c := outboxCount(t, raw, "ARCHIVED", id.Value()); c != 1 {
		t.Fatalf("expected 1 ARCHIVED outbox row, got %d", c)
	}

	// --- Unarchive (soft delete cleared) ---
	eu := &flatPerson{Name: "Alice B", Email: "alice@x"}
	eu.SetID(domain.NewID(id.Value()))
	un, err := domain.GetUnarchivable(eu, nil, "GetUnarchivable")
	if err != nil {
		t.Fatalf("GetUnarchivable: %v", err)
	}
	if err := eng.Unarchive(ctx, un, flatSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if err := raw.QueryRow(`SELECT deleted_at FROM flat_persons`).Scan(&deletedAt); err != nil {
		t.Fatalf("select after unarchive: %v", err)
	}
	if deletedAt.Valid {
		t.Fatal("unarchive did not clear deleted_at")
	}
	if c := outboxCount(t, raw, "UNARCHIVED", id.Value()); c != 1 {
		t.Fatalf("expected 1 UNARCHIVED outbox row, got %d", c)
	}

	// --- Delete (hard) ---
	ed := &flatPerson{Name: "Alice B", Email: "alice@x"}
	ed.SetID(domain.NewID(id.Value()))
	del, err := domain.GetDeletable(ed, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	if err := eng.Delete(ctx, del, flatSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	var remaining int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM flat_persons`).Scan(&remaining); err != nil {
		t.Fatalf("count after delete: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("delete did not remove the row, %d remain", remaining)
	}
	if c := outboxCount(t, raw, "DELETED", id.Value()); c != 1 {
		t.Fatalf("expected 1 DELETED outbox row, got %d", c)
	}
}

// TestOracleEngine_UpdateMatchSemantics covers the RowsAffected-driven match
// detection:
//   - an UPDATE for an id that does not exist surfaces RecordNotFound (404),
//     never a silent success;
//   - a no-op UPDATE of an EXISTING row (identical values) still succeeds — on
//     Oracle rows-affected counts MATCHED rows natively (the row is rewritten
//     even when values are identical), so an idempotent PUT is not mistaken
//     for a missing row without any DSN flag (MySQL needs clientFoundRows for
//     the same guarantee).
func TestOracleEngine_UpdateMatchSemantics(t *testing.T) {
	eng, _ := setup(t)
	ctx := ctxFor()

	// Missing id → RecordNotFound, no silent success.
	missing := &flatPerson{Name: "Ghost", Email: "ghost@x"}
	missing.SetID(domain.NewID(uuid.NewString()))
	updMissing, err := domain.GetUpdatable(missing, func(*flatPerson) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	_, err = eng.Update(ctx, updMissing, flatSchema(), core.WriteHook{})
	if err == nil {
		t.Fatal("expected RecordNotFound updating a non-existent id, got nil (silent lost update)")
	}
	var carrier domain.NotificationCarrier
	if !errors.As(err, &carrier) {
		t.Fatalf("expected a NotificationCarrier, got %T: %v", err, err)
	}
	if key := domain.NotificationKey(carrier.NotificationContexts()[0].Messages()[0].Notification); key != "RecordNotFoundNotification" {
		t.Fatalf("notification = %q, want RecordNotFoundNotification", key)
	}

	// Insert a row, then UPDATE it to identical values: still a match (no false
	// not-found).
	e := &flatPerson{Name: "Stable", Email: "stable@x"}
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	noop := &flatPerson{Name: "Stable", Email: "stable@x"}
	noop.SetID(res.ID)
	updNoop, err := domain.GetUpdatable(noop, func(*flatPerson) error { return nil }, nil, "GetUpdatable")
	if err != nil {
		t.Fatalf("GetUpdatable: %v", err)
	}
	if _, err := eng.Update(ctx, updNoop, flatSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("no-op UPDATE of an existing row should succeed, got: %v", err)
	}
}

// TestOracleEngine_FindByID proves the read seam: the framework's
// AggregateLoader + criteria engine run over the Oracle engine via the neutral
// Querier/Dialect — the RAW(16) id is encoded into the WHERE arg and decoded
// back on scan, so a UUID v7 round-trips through a real read.
func TestOracleEngine_FindByID(t *testing.T) {
	eng, _ := setup(t)
	ctx := ctxFor()

	phone := "551199"
	e := &flatPerson{Name: "Bruno", Email: "bruno@x", Phone: &phone}
	ins, err := domain.GetInsertable(e, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	loader := read.NewAggregateLoader[*flatPerson](eng, func() *flatPerson { return &flatPerson{} }).
		WithSchema(flatSchema())

	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.GetID() == nil || got.GetID().Value() != res.ID.Value() {
		t.Fatalf("FindByID id = %v, want %s", got.GetID(), res.ID)
	}
	if got.Name != "Bruno" || got.Email != "bruno@x" {
		t.Fatalf("FindByID fields: name=%q email=%q", got.Name, got.Email)
	}
	if got.Phone == nil || *got.Phone != phone {
		t.Fatalf("FindByID phone = %v, want %q", got.Phone, phone)
	}

	// A non-existent id yields the canonical RecordNotFound (not a silent zero).
	if _, err := loader.FindOne(ctx, criteria.ByID(domain.NewID(uuid.NewString()))); err == nil {
		t.Fatal("expected NotFound for a missing id")
	}
}

// TestOracleEngine_FindAll_OffsetWindow proves offset pagination executes on a
// live Oracle: an ordered FindAll with Limit + Offset returns the correct page
// via the `OFFSET m ROWS FETCH NEXT n ROWS ONLY` row-limiting tail (Oracle
// 12c+). The window is identical across engines; only the rendered clause
// differs.
func TestOracleEngine_FindAll_OffsetWindow(t *testing.T) {
	eng, _ := setup(t)
	ctx := ctxFor()

	for _, name := range []string{"Ann", "Bea", "Cyd", "Dan", "Eve"} {
		e := &flatPerson{Name: name, Email: name + "@x"}
		ins, err := domain.GetInsertable(e, nil, "GetInsertable")
		if err != nil {
			t.Fatalf("GetInsertable(%s): %v", name, err)
		}
		if _, err := eng.Insert(ctx, ins, flatSchema(), core.WriteHook{}); err != nil {
			t.Fatalf("Insert(%s): %v", name, err)
		}
	}

	loader := read.NewAggregateLoader[*flatPerson](eng, func() *flatPerson { return &flatPerson{} }).
		WithSchema(flatSchema())

	// Page 2 of an ascending window, 2 per page — skip Ann, Bea; expect Cyd, Dan.
	page, err := loader.FindAll(ctx, criteria.Where(nil).OrderBy("Name").Limit(2).Offset(2))
	if err != nil {
		t.Fatalf("FindAll offset window: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("offset window: expected 2 rows, got %d", len(page))
	}
	if page[0].Name != "Cyd" || page[1].Name != "Dan" {
		t.Fatalf("offset window order wrong: %q, %q (want Cyd, Dan)", page[0].Name, page[1].Name)
	}

	// The last page is shorter than the limit — skip 4, expect just Eve.
	tail, err := loader.FindAll(ctx, criteria.Where(nil).OrderBy("Name").Limit(2).Offset(4))
	if err != nil {
		t.Fatalf("FindAll tail window: %v", err)
	}
	if len(tail) != 1 || tail[0].Name != "Eve" {
		t.Fatalf("tail window wrong: got %d rows (want 1 row Eve)", len(tail))
	}

	// Contract: an offset with no ORDER BY is rejected before it can return a
	// non-deterministic page.
	if _, err := loader.FindAll(ctx, criteria.Where(nil).Limit(2).Offset(2)); err == nil {
		t.Fatal("expected an error: Offset without an OrderBy")
	}
}

// --- aggregate fixtures (root + child) ----------------------------------

type acct struct {
	domain.AggregateRoot
	Name string
}

func (a *acct) Modes() []domain.EntityMode {
	return []domain.EntityMode{
		domain.ModeInsert, domain.ModeUpdate, domain.ModeDelete,
		domain.ModeArchive, domain.ModeUnarchive,
	}
}
func (*acct) BuildRules(string, domain.Service, *domain.Rules) {}
func (a *acct) GetAggregateRoot() *domain.AggregateRoot        { return &a.AggregateRoot }
func (a *acct) AggregateChildren() []domain.AggregateValueObject {
	return []domain.AggregateValueObject{tag{}}
}

type tag struct {
	ID    string
	Label string
}

func (t tag) GetID() domain.ID                               { return domain.NewID(t.ID) }
func (tag) BuildRules(string, domain.Service, *domain.Rules) {}

func acctSchema() *core.TableSchema {
	return core.NewTableSchema[*acct]("accts").
		PK("id").Field("Name", "name").
		SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(core.NewTableSchema[tag]("acct_tags").
			PK("id").FK("acct_id").Field("Label", "label").
			SoftDelete("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at"))
}

func setupAgg(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	testDB := newTestOracleSchema(t)
	eng, err := New(ctx, core.EngineConfig{DSN: testDB})
	if err != nil {
		t.Fatalf("New on the test schema: %v", err)
	}
	raw, _ := sql.Open("oracle", testDB)
	for _, stmt := range []string{
		`CREATE TABLE accts (
			id RAW(16) NOT NULL PRIMARY KEY, name VARCHAR2(255 CHAR) NOT NULL,
			deleted_at TIMESTAMP(6) NULL, created_at TIMESTAMP(6) NOT NULL, updated_at TIMESTAMP(6) NOT NULL )`,
		`CREATE TABLE acct_tags (
			id RAW(16) NOT NULL PRIMARY KEY,
			acct_id RAW(16) NOT NULL,
			label VARCHAR2(255 CHAR) NOT NULL,
			deleted_at TIMESTAMP(6) NULL, created_at TIMESTAMP(6) NOT NULL, updated_at TIMESTAMP(6) NOT NULL,
			CONSTRAINT fk_acct FOREIGN KEY (acct_id) REFERENCES accts(id) ON DELETE CASCADE )`,
		`CREATE TABLE outbox (
			id RAW(16) NOT NULL PRIMARY KEY, aggregate_type VARCHAR2(100) NOT NULL,
			event_type VARCHAR2(50) NOT NULL, aggregate_id VARCHAR2(36) NOT NULL,
			payload JSON NOT NULL, traceparent VARCHAR2(64) NULL, created_at TIMESTAMP(6) NOT NULL )`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	t.Cleanup(func() {
		// The throw-away schema is dropped by newTestOracleSchema's cleanup
		// (LIFO: these closes run first); no per-table teardown needed.
		_ = raw.Close()
		eng.Close()
	})
	return eng.(*Engine), raw
}

// TestOracleEngine_AggregateRoundTrip writes an aggregate (root + 2 children)
// via the engine and reads it back via the loader — proving the aggregate
// write path (child FK injection as RAW(16), one TX) and the aggregate read
// path (batched child load) on Oracle — then archives it (cascade) and
// deletes it (FK ON DELETE CASCADE).
func TestOracleEngine_AggregateRoundTrip(t *testing.T) {
	eng, raw := setupAgg(t)
	ctx := ctxFor()

	a := &acct{Name: "Acme"}
	domain.AddAggregateChild(a, tag{Label: "vip"})
	domain.AddAggregateChild(a, tag{Label: "beta"})
	ins, err := domain.GetInsertable(a, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, acctSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert aggregate: %v", err)
	}

	// 2 child rows persisted with the RAW(16) FK to the root.
	var childCount int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM acct_tags`).Scan(&childCount); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if childCount != 2 {
		t.Fatalf("expected 2 child rows, got %d", childCount)
	}

	// Read back via the loader — root + batched children.
	loader := read.NewAggregateLoader[*acct](eng, func() *acct { return &acct{} }).WithSchema(acctSchema())
	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne aggregate: %v", err)
	}
	if got.Name != "Acme" {
		t.Fatalf("root name = %q", got.Name)
	}
	kids := domain.GetCurrentItemsOf[tag](&got.AggregateRoot)
	if len(kids) != 2 {
		t.Fatalf("expected 2 loaded children, got %d", len(kids))
	}

	// Archive cascades to the children (deleted_at set on every active child).
	arch, err := domain.GetArchivable(got, nil, "GetArchivable")
	if err != nil {
		t.Fatalf("GetArchivable: %v", err)
	}
	if err := eng.Archive(ctx, arch, acctSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Archive aggregate: %v", err)
	}
	var activeChildren int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM acct_tags WHERE deleted_at IS NULL`).Scan(&activeChildren); err != nil {
		t.Fatalf("count active children: %v", err)
	}
	if activeChildren != 0 {
		t.Fatalf("archive cascade left %d active children", activeChildren)
	}

	// Hard delete relies on FK ON DELETE CASCADE — both tables emptied.
	del, err := domain.GetDeletable(got, nil, "GetDeletable")
	if err != nil {
		t.Fatalf("GetDeletable: %v", err)
	}
	if err := eng.Delete(ctx, del, acctSchema(), core.WriteHook{}); err != nil {
		t.Fatalf("Delete aggregate: %v", err)
	}
	var roots, tags int
	_ = raw.QueryRow(`SELECT COUNT(*) FROM accts`).Scan(&roots)
	_ = raw.QueryRow(`SELECT COUNT(*) FROM acct_tags`).Scan(&tags)
	if roots != 0 || tags != 0 {
		t.Fatalf("delete cascade left roots=%d tags=%d", roots, tags)
	}
}

type dupEmailNotification struct{ domain.DomainNotificationBase }

// TestOracleEngine_UniqueViolation proves the dialect-aware mapErr: an
// ORA-00001 on the uniq_email constraint is classified by
// oracleDialect.IsUniqueViolation (uppercase catalog name lowercased back to
// the declared form) and mapped to the bound typed notification (a
// *core.InfrastructureError), the same 409-shaped outcome the other engines
// produce — instead of leaking the raw driver error as a 500.
func TestOracleEngine_UniqueViolation(t *testing.T) {
	eng, _ := setup(t)
	ctx := ctxFor()

	repo := &write.BaseRepository[*flatPerson]{
		Engine:      eng,
		ContextName: "Person",
		Constraints: map[string]write.ConstraintBinding{
			"uniq_email": {Notification: dupEmailNotification{}, Field: "email"},
		},
		Schema: flatSchema(),
	}
	mk := func(name string) domain.Insertable {
		ins, err := domain.GetInsertable(&flatPerson{Name: name, Email: "dup@x"}, nil, "GetInsertable")
		if err != nil {
			t.Fatalf("GetInsertable: %v", err)
		}
		return ins
	}

	if _, err := repo.Scope(ctx).Insert(mk("First")); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := repo.Scope(ctx).Insert(mk("Second"))
	if err == nil {
		t.Fatal("expected a unique-violation error on the duplicate email")
	}
	var infraErr *core.InfrastructureError
	if !errors.As(err, &infraErr) {
		t.Fatalf("expected a typed *core.InfrastructureError, got %T (%v)", err, err)
	}
}
