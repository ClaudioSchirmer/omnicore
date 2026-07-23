//go:build integration && postgres

// Shared helpers for the pg-engine integration tests. Run with:
//
//	go test -tags=integration ./infra/db/pg/...
//
// Defaults target the example service's local docker-compose Postgres
// (postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable);
// override via OMNICORE_TEST_PG_DSN. Each test creates a throw-away database so
// parallel and repeat runs don't stomp on each other.
package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

const defaultPGAdminDSN = "postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable"

// testCtx returns the *AppContext used across the integration tests; it carries
// a random request id and satisfies persistence.RequestContext.
func testCtx() *configuration.AppContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// noHook is the zero-value core.WriteHook the integration tests pass when they
// exercise the persister without lifecycle hook firing.
var noHook = core.WriteHook{}

func pgAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultPGAdminDSN
}

// newTestPG creates a throw-away Postgres database with the framework outbox +
// omnicore_mongo_views + audit_events tables already applied. Returns a
// *Postgres and a cleanup func.
func newTestPG(t *testing.T) (*Postgres, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("omnicore_pg_test_%s", strings.ReplaceAll(uuid.NewString(), "-", ""))

	adminPool, err := pgxpool.New(ctx, pgAdminDSN())
	if err != nil {
		t.Skipf("skipping: cannot reach Postgres at %s (%v)", pgAdminDSN(), err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		t.Fatalf("CREATE DATABASE %q: %v", dbName, err)
	}

	dsn := swapDB(pgAdminDSN(), dbName)
	engine, err := NewPostgres(ctx, dsn)
	if err != nil {
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
		t.Fatalf("NewPostgres: %v", err)
	}

	if err := createFrameworkTables(ctx, engine.Pool()); err != nil {
		engine.Close()
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
		t.Fatalf("framework tables: %v", err)
	}

	cleanup := func() {
		engine.Close()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		admin, err := pgxpool.New(c, pgAdminDSN())
		if err != nil {
			return
		}
		defer admin.Close()
		_, _ = admin.Exec(c, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
		_, _ = admin.Exec(c, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
	}
	return engine, cleanup
}

func swapDB(dsn, name string) string {
	idx := strings.LastIndex(dsn, "/")
	q := strings.Index(dsn, "?")
	if idx == -1 || q == -1 || q < idx {
		return dsn
	}
	return dsn[:idx+1] + name + dsn[q:]
}

// createFrameworkTables installs the outbox + omnicore_mongo_views + audit_events
// tables without dragging the migration package in (avoids an import cycle).
func createFrameworkTables(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE outbox (
			id UUID PRIMARY KEY,
			aggregate_type TEXT NOT NULL,
			aggregate_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			payload JSONB NOT NULL,
			traceparent VARCHAR(64),
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE omnicore_mongo_views (
			id UUID PRIMARY KEY,
			view_name TEXT NOT NULL UNIQUE,
			version INTEGER NOT NULL CHECK (version > 0),
			rebuild_hash VARCHAR(64),
			artifact_hash VARCHAR(64),
			combined_hash VARCHAR(64),
			previous_version INTEGER,
			previous_combined_hash VARCHAR(64),
			previous_applied_at TIMESTAMP,
			status TEXT NOT NULL DEFAULT 'done' CHECK (status IN ('done','processing')),
			started_at TIMESTAMP,
			pid TEXT,
			host TEXT,
			applied_at TIMESTAMP NOT NULL,
			applied_by TEXT NOT NULL,
			code_version TEXT
		)`,
		`CREATE TABLE audit_events (
			id            UUID         NOT NULL,
			aggregate_id  CHAR(36)     NOT NULL,
			entity_type   VARCHAR(255) NOT NULL,
			verb          VARCHAR(32)  NOT NULL,
			action_name   VARCHAR(64)  NOT NULL,
			kind          VARCHAR(16)  NOT NULL,
			actor         VARCHAR(255),
			actor_issuer  VARCHAR(255),
			tenant_id     VARCHAR(255),
			trace_id      VARCHAR(32),
			thread_id     CHAR(36)     NOT NULL,
			occurred_at   TIMESTAMP    NOT NULL,
			created_at    TIMESTAMP    NOT NULL DEFAULT NOW(),
			payload       JSONB        NOT NULL,
			PRIMARY KEY (id)
		)`,
		`CREATE INDEX audit_events_entity_timeline_idx
			ON audit_events (entity_type, aggregate_id, occurred_at DESC)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// createTable executes a CREATE TABLE statement on the test PG.
func createTable(t *testing.T, engine *Postgres, ddl string) {
	t.Helper()
	if _, err := engine.Pool().Exec(context.Background(), ddl); err != nil {
		t.Fatalf("createTable: %v\nDDL:\n%s", err, ddl)
	}
}

// outboxCount returns the number of outbox rows in the test PG.
func outboxCount(t *testing.T, engine *Postgres) int {
	t.Helper()
	var n int
	if err := engine.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox`).Scan(&n); err != nil {
		t.Fatalf("outboxCount: %v", err)
	}
	return n
}

// outboxRow carries aggregate_type, event_type, aggregate_id, payload per row.
type outboxRow struct {
	AggregateType, EventType, AggregateID string
	Payload                               []byte
}

func outboxRows(t *testing.T, engine *Postgres) []outboxRow {
	t.Helper()
	rows, err := engine.Pool().Query(context.Background(),
		`SELECT aggregate_type, event_type, aggregate_id, payload FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatalf("outboxRows: %v", err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.AggregateType, &r.EventType, &r.AggregateID, &r.Payload); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// rowCount returns the row count of an arbitrary table.
func rowCount(t *testing.T, engine *Postgres, table string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)
	if err := engine.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("rowCount %s: %v", table, err)
	}
	return n
}

// activeCount counts rows where deleted_at IS NULL.
func activeCount(t *testing.T, engine *Postgres, table string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE deleted_at IS NULL`, table)
	if err := engine.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("activeCount %s: %v", table, err)
	}
	return n
}
