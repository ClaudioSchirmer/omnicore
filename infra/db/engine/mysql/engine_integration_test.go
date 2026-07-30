//go:build integration && mysql

package mysql

import (
	"context"
	"database/sql"
	"errors"
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

// Integration tests for the MySQL engine's synchronous write path against a
// real MySQL container (devops/docker-compose.yml `mysql` service, host :3307).
//
//	go test -tags=integration,mysql ./infra/db/engine/mysql/ -count=1
//
// Each test creates a THROW-AWAY database (like the pg harness) so repeat runs
// never stomp on the example service's `users_db` — that requires an
// admin-capable connection (the bench's root); override it via
// OMNICORE_TEST_MYSQL_ADMIN_DSN. Verifies each write verb persists the row
// (UUID v7 round-tripping through BINARY(16)) and lands the matching outbox row
// in the same TX.

const defaultMySQLAdminDSN = "root:root@tcp(127.0.0.1:3307)/?parseTime=true&multiStatements=true"

func mysqlAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_MYSQL_ADMIN_DSN"); v != "" {
		return v
	}
	return defaultMySQLAdminDSN
}

// testDSN is the DSN of the current test's throw-away database, set by
// newTestMySQLDB. The tests in this package do not use t.Parallel, so the
// package variable is race-free; dsn() hands it to tests that open a SECOND
// connection to the same database (e.g. the tracing engine).
var testDSN string

func dsn() string { return testDSN }

// newTestMySQLDB creates a throw-away database on the bench MySQL, points
// testDSN at it and registers its DROP as cleanup (LIFO: callers' own cleanups
// — closing engines/connections — run first). Skips the test when MySQL is
// unreachable.
func newTestMySQLDB(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := sql.Open("mysql", mysqlAdminDSN())
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — start devops/docker-compose.yml mysql service", err)
	}
	if err := admin.PingContext(ctx); err != nil {
		_ = admin.Close()
		t.Skipf("MySQL not reachable (%v) — start devops/docker-compose.yml mysql service", err)
	}

	dbName := "omnicore_mysql_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+dbName); err != nil {
		_ = admin.Close()
		t.Fatalf("CREATE DATABASE %s: %v", dbName, err)
	}
	testDSN = swapMySQLDB(mysqlAdminDSN(), dbName)

	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(c, "DROP DATABASE IF EXISTS "+dbName)
		_ = admin.Close()
		testDSN = ""
	})
	return testDSN
}

// swapMySQLDB inserts the database name into a `user:pass@tcp(host:port)/?p=v`
// DSN (between the last '/' and the '?').
func swapMySQLDB(adminDSN, name string) string {
	idx := strings.LastIndex(adminDSN, "/")
	q := strings.Index(adminDSN, "?")
	if idx == -1 || q == -1 || q < idx {
		return adminDSN
	}
	return adminDSN[:idx+1] + name + adminDSN[q:]
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
		ID("id").
		Field("Name", "name").
		Field("Email", "email").
		Field("Phone", "phone").
		DeletedAt("deleted_at").
		CreatedAt("created_at").
		UpdatedAt("updated_at")
}

// refEntity carries a secondary identity reference (TenantID) typed
// domain.ID — the field TYPE is the declaration: it pairs with a BINARY(16)
// column that is neither the ID nor an aggregate ParentID, binding as 16 bytes on
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
		ID("id").
		Field("Name", "name").
		Field("TenantID", "tenant_id")
}

// A secondary BINARY(16) identity column (not the ID, not an aggregate ParentID)
// typed domain.ID must round-trip: written as 16 bytes (the typed EncodeArg
// case) and auto-scanned back to the canonical value (the id scan proxy) —
// Postgres parity for a cross-aggregate reference.
func TestMySQLEngine_SecondaryUUIDColumn(t *testing.T) {
	eng, raw := setup(t)
	ctx := ctxFor()

	if _, err := raw.ExecContext(ctx, `DROP TABLE IF EXISTS refs`); err != nil {
		t.Fatalf("drop refs: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `CREATE TABLE refs (
		id BINARY(16) PRIMARY KEY,
		name VARCHAR(255) NOT NULL,
		tenant_id BINARY(16) NOT NULL
	)`); err != nil {
		t.Fatalf("create refs: %v", err)
	}
	t.Cleanup(func() { _, _ = raw.ExecContext(context.Background(), `DROP TABLE IF EXISTS refs`) })

	tenant := uuid.NewString()
	ins, err := domain.GetInsertable(&refEntity{Name: "Acme", TenantID: domain.NewID(tenant)}, nil, "GetInsertable")
	if err != nil {
		t.Fatalf("GetInsertable: %v", err)
	}
	res, err := eng.Insert(ctx, ins, refSchema(), core.WriteHook{})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Stored as BINARY(16), not text.
	var rawTenant []byte
	if err := raw.QueryRow(`SELECT tenant_id FROM refs`).Scan(&rawTenant); err != nil {
		t.Fatalf("select tenant_id: %v", err)
	}
	if len(rawTenant) != 16 {
		t.Fatalf("tenant_id stored as %d bytes, want BINARY(16)", len(rawTenant))
	}

	// Auto-scan decodes it back to the canonical UUID string.
	loader := read.NewAggregateLoader[*refEntity](eng, func() *refEntity { return &refEntity{} }).
		WithSchema(refSchema())
	got, err := loader.FindOne(ctx, criteria.ByID(res.ID))
	if err != nil {
		t.Fatalf("FindOne: %v", err)
	}
	if got.TenantID.Value() != tenant {
		t.Fatalf("secondary id column = %q, want canonical %q (BINARY(16) not decoded on scan)", got.TenantID.Value(), tenant)
	}

	// A bare-string criteria probe on the domain.ID-typed field is lifted by
	// the translator and matches the BINARY(16) column.
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
	eng, err := New(ctx, core.EngineConfig{DSN: newTestMySQLDB(t), Pool: core.PoolConfig{MaxOpenConns: 7, MaxIdleConns: 3}})
	if err != nil {
		t.Fatalf("New on the test database: %v", err)
	}
	defer eng.Close()
	if got := eng.(*Engine).db.Stats().MaxOpenConnections; got != 7 {
		t.Fatalf("MaxOpenConnections = %d, want 7 (pool config not applied to *sql.DB)", got)
	}
}

func setup(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	testDB := newTestMySQLDB(t)
	eng, err := New(ctx, core.EngineConfig{DSN: testDB})
	if err != nil {
		t.Fatalf("New on the test database: %v", err)
	}
	raw, err := sql.Open("mysql", testDB)
	if err != nil {
		t.Fatalf("open assert conn: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE flat_persons (
			id BINARY(16) PRIMARY KEY,
			name VARCHAR(255) NOT NULL,
			email VARCHAR(255) NOT NULL,
			phone VARCHAR(32) NULL,
			revision BIGINT NOT NULL DEFAULT 0,
			deleted_at DATETIME NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE KEY uniq_email (email)
		)`,
		`CREATE TABLE outbox (
			id BINARY(16) PRIMARY KEY,
			aggregate_type VARCHAR(100) NOT NULL,
			event_type VARCHAR(50) NOT NULL,
			aggregate_id VARCHAR(36) NOT NULL,
			payload JSON NOT NULL,
			traceparent VARCHAR(64) NULL,
			created_at DATETIME NOT NULL
		)`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl %q: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		// The throw-away database is dropped by newTestMySQLDB's cleanup (LIFO:
		// these closes run first); no per-table teardown needed.
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
		`SELECT COUNT(*) FROM outbox WHERE event_type = ? AND aggregate_id = ?`, eventType, aggID,
	).Scan(&n); err != nil {
		t.Fatalf("outbox count: %v", err)
	}
	return n
}

func TestMySQLEngine_WritePath(t *testing.T) {
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

	// Row persisted with BINARY(16) ID round-tripping back to the same UUID.
	var rawID []byte
	var name, email string
	if err := raw.QueryRow(`SELECT id, name, email FROM flat_persons`).Scan(&rawID, &name, &email); err != nil {
		t.Fatalf("select after insert: %v", err)
	}
	gotID, err := uuid.FromBytes(rawID)
	if err != nil || gotID.String() != id.Value() {
		t.Fatalf("BINARY(16) ID did not round-trip: bytes=%x got=%v want=%s err=%v", rawID, gotID, id, err)
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

	// --- Archive (archive set) ---
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

	// --- Unarchive (archive cleared) ---
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

// TestMySQLEngine_UpdateMatchSemantics covers the RowsAffected-driven match
// detection added for PG⇄MySQL parity:
//   - an UPDATE for an id that does not exist surfaces RecordNotFound (404),
//     never a silent success (the silent lost-update the review flagged);
//   - a no-op UPDATE of an EXISTING row (identical values) still succeeds —
//     proving clientFoundRows makes RowsAffected count MATCHED, not changed,
//     rows, so an idempotent PUT is not mistaken for a missing row.
func TestMySQLEngine_UpdateMatchSemantics(t *testing.T) {
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
	// not-found) thanks to clientFoundRows.
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
		t.Fatalf("no-op UPDATE of an existing row should succeed (clientFoundRows), got: %v", err)
	}
}

// TestMySQLEngine_FindByID proves the relocated read seam: the framework's
// AggregateLoader + criteria engine run over the MySQL engine via the neutral
// Querier/Dialect — the BINARY(16) id is encoded into the WHERE arg and decoded
// back on scan, so a UUID v7 round-trips through a real MySQL read.
func TestMySQLEngine_FindByID(t *testing.T) {
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

// TestMySQLEngine_FindAll_OffsetWindow proves offset pagination executes on a
// live MySQL: an ordered FindAll with Limit + Offset returns the correct page
// via the dialect's native `LIMIT n OFFSET m`. The write-side offset window is
// identical across engines; only the rendered clause differs.
func TestMySQLEngine_FindAll_OffsetWindow(t *testing.T) {
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
		t.Fatalf("tail window wrong: got %d rows, first=%q (want 1 row Eve)", len(tail), func() string {
			if len(tail) > 0 {
				return tail[0].Name
			}
			return ""
		}())
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
	domain.Managed
	Label string
}

func (tag) BuildRules(string, domain.Service, *domain.Rules) {}

func acctSchema() *core.TableSchema {
	return core.NewTableSchema[*acct]("accts").
		ID("id").Field("Name", "name").
		DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at").
		Child(core.NewTableSchema[tag]("acct_tags").
			ID("id").ParentID("acct_id").Field("Label", "label").
			DeletedAt("deleted_at").CreatedAt("created_at").UpdatedAt("updated_at"))
}

func setupAgg(t *testing.T) (*Engine, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	testDB := newTestMySQLDB(t)
	eng, err := New(ctx, core.EngineConfig{DSN: testDB})
	if err != nil {
		t.Fatalf("New on the test database: %v", err)
	}
	raw, _ := sql.Open("mysql", testDB)
	for _, stmt := range []string{
		`CREATE TABLE accts (
			id BINARY(16) PRIMARY KEY, name VARCHAR(255) NOT NULL,
			deleted_at DATETIME NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL )`,
		`CREATE TABLE acct_tags (
			id BINARY(16) PRIMARY KEY,
			acct_id BINARY(16) NOT NULL,
			label VARCHAR(255) NOT NULL,
			deleted_at DATETIME NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			CONSTRAINT fk_acct FOREIGN KEY (acct_id) REFERENCES accts(id) ON DELETE CASCADE )`,
		`CREATE TABLE outbox (
			id BINARY(16) PRIMARY KEY, aggregate_type VARCHAR(100) NOT NULL,
			event_type VARCHAR(50) NOT NULL, aggregate_id VARCHAR(36) NOT NULL,
			payload JSON NOT NULL, traceparent VARCHAR(64) NULL, created_at DATETIME NOT NULL )`,
	} {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("ddl: %v", err)
		}
	}
	t.Cleanup(func() {
		// The throw-away database is dropped by newTestMySQLDB's cleanup (LIFO:
		// these closes run first); no per-table teardown needed.
		_ = raw.Close()
		eng.Close()
	})
	return eng.(*Engine), raw
}

// TestMySQLEngine_AggregateRoundTrip writes an aggregate (root + 2 children) via
// the engine and reads it back via the loader — proving the aggregate write path
// (child ParentID injection as BINARY(16), one TX) and the aggregate read path
// (batched child load) on MySQL — then archives it (cascade) and deletes it
// (ParentID ON DELETE CASCADE).
func TestMySQLEngine_AggregateRoundTrip(t *testing.T) {
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

	// 2 child rows persisted with the BINARY(16) ParentID to the root.
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

	// Hard delete relies on ParentID ON DELETE CASCADE — both tables emptied.
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

// TestMySQLEngine_UniqueViolation proves the dialect-aware mapErr: a MySQL 1062
// on the uniq_email index is classified by mysqlDialect.IsUniqueViolation and
// mapped to the bound typed notification (a *core.InfrastructureError), the
// same 409-shaped outcome the Postgres path produces from SQLSTATE 23505 —
// instead of leaking the raw driver error as a 500.
func TestMySQLEngine_UniqueViolation(t *testing.T) {
	eng, _ := setup(t)
	ctx := ctxFor()

	repo := (&write.BaseRepository[*flatPerson]{
		Engine:      eng,
		NewEntity:   func() *flatPerson { return &flatPerson{} },
		ContextName: "Person",
		Constraints: map[string]write.ConstraintBinding{
			"uniq_email": {Notification: dupEmailNotification{}, Field: "email"},
		},
	}).WithSchema(flatSchema().Revision("revision"))
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
