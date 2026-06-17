//go:build integration

// Shared helpers for integration tests in the infra package. Run with:
//
//	go test -tags=integration ./infra/...
//
// Defaults target the example service's local docker-compose:
//   - Postgres: postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable
//   - Mongo:    mongodb://localhost:27018
//
// Override via OMNICORE_TEST_PG_DSN and OMNICORE_TEST_MONGO_URI when the
// bench listens elsewhere. Each test creates throw-away databases / collections
// so parallel and repeat runs don't stomp on each other or on the example's
// QA suites.
package infra

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	mongoopts "go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

const (
	defaultPGAdminDSN = "postgres://omnicore:omnicore@localhost:5433/postgres?sslmode=disable"
	defaultMongoURI   = "mongodb://localhost:27018"
)

// testCtx returns a persistence.RequestContext suitable for integration tests. The
// underlying *AppContext carries a random request id; tests calling the
// persister methods pass this in as the first arg.
func testCtx() persistence.RequestContext {
	return configuration.NewAppContextWithRandomID(configuration.LangENG)
}

// noHook is the zero-value writeHook the integration tests pass when
// they exercise the persister without lifecycle hook firing — same as
// "no opts on the BaseRepository call site".
var noHook = writeHook{}

func pgAdminDSN() string {
	if v := os.Getenv("OMNICORE_TEST_PG_DSN"); v != "" {
		return v
	}
	return defaultPGAdminDSN
}

func mongoURI() string {
	if v := os.Getenv("OMNICORE_TEST_MONGO_URI"); v != "" {
		return v
	}
	return defaultMongoURI
}

// newTestPG creates a throw-away Postgres database with the framework outbox
// + omnicore_mongo_views tables already applied (so writeOutbox doesn't fail
// on the first insert). Returns a *Postgres and a cleanup func.
func newTestPG(t *testing.T) (*Postgres, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("omnicore_infra_test_%s", strings.ReplaceAll(uuid.NewString(), "-", ""))

	adminPool, err := pgxpool.New(ctx, pgAdminDSN())
	if err != nil {
		t.Skipf("skipping: cannot reach Postgres at %s (%v)", pgAdminDSN(), err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		t.Fatalf("CREATE DATABASE %q: %v", dbName, err)
	}

	dsn := swapDB(pgAdminDSN(), dbName)
	pg, err := NewPostgres(ctx, dsn)
	if err != nil {
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
		t.Fatalf("NewPostgres: %v", err)
	}

	if err := createFrameworkTables(ctx, pg.Pool()); err != nil {
		pg.Close()
		_, _ = adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %q`, dbName))
		t.Fatalf("framework tables: %v", err)
	}

	cleanup := func() {
		pg.Close()
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
	return pg, cleanup
}

func swapDB(dsn, db string) string {
	idx := strings.LastIndex(dsn, "/")
	q := strings.Index(dsn, "?")
	if idx == -1 || q == -1 || q < idx {
		return dsn
	}
	return dsn[:idx+1] + db + dsn[q:]
}

// createFrameworkTables installs the outbox + omnicore_mongo_views tables
// without dragging the migration package in (avoids an import cycle between
// the infra integration tests and infra/migration tests).
func createFrameworkTables(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS pgcrypto`,
		`CREATE TABLE outbox (
			id BIGSERIAL PRIMARY KEY,
			aggregate_type TEXT NOT NULL,
			aggregate_id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			payload JSONB NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE omnicore_mongo_views (
			view_name TEXT PRIMARY KEY,
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

// createTable executes a CREATE TABLE statement on the test PG. Convenience
// for setting up a domain table per test.
func createTable(t *testing.T, pg *Postgres, ddl string) {
	t.Helper()
	if _, err := pg.Pool().Exec(context.Background(), ddl); err != nil {
		t.Fatalf("createTable: %v\nDDL:\n%s", err, ddl)
	}
}

// outboxCount returns the number of outbox rows in the test PG.
func outboxCount(t *testing.T, pg *Postgres) int {
	t.Helper()
	var n int
	err := pg.Pool().QueryRow(context.Background(), `SELECT COUNT(*) FROM outbox`).Scan(&n)
	if err != nil {
		t.Fatalf("outboxCount: %v", err)
	}
	return n
}

// outboxRows returns aggregate_type, event_type, aggregate_id for each row.
type outboxRow struct {
	AggregateType, EventType, AggregateID string
	Payload                               []byte
}

func outboxRows(t *testing.T, pg *Postgres) []outboxRow {
	t.Helper()
	rows, err := pg.Pool().Query(context.Background(),
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
func rowCount(t *testing.T, pg *Postgres, table string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, table)
	if err := pg.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("rowCount %s: %v", table, err)
	}
	return n
}

// activeCount counts rows where deleted_at IS NULL.
func activeCount(t *testing.T, pg *Postgres, table string) int {
	t.Helper()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE deleted_at IS NULL`, table)
	if err := pg.Pool().QueryRow(context.Background(), q).Scan(&n); err != nil {
		t.Fatalf("activeCount %s: %v", table, err)
	}
	return n
}

// --- Mongo helpers --------------------------------------------------------

func newTestMongo(t *testing.T) (*MongoDB, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("omnicore_infra_test_%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	m, err := NewMongoDB(ctx, mongoURI(), dbName)
	if err != nil {
		t.Skipf("skipping: cannot reach Mongo at %s (%v)", mongoURI(), err)
	}
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.db.Drop(c)
		_ = m.Close(c)
	}
	return m, cleanup
}

// newRawMongo returns a raw *mongo.Database for tests that want to bypass the
// MongoDB wrapper (e.g. inspect collections or insert fixture docs directly).
func newRawMongo(t *testing.T) (*mongo.Database, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dbName := fmt.Sprintf("omnicore_raw_test_%s", strings.ReplaceAll(uuid.NewString(), "-", ""))
	client, err := mongo.Connect(mongoopts.Client().ApplyURI(mongoURI()).
		SetBSONOptions(&mongoopts.BSONOptions{DefaultDocumentM: true}))
	if err != nil {
		t.Skipf("skipping: cannot reach Mongo at %s (%v)", mongoURI(), err)
	}
	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		t.Skipf("skipping: cannot ping Mongo (%v)", err)
	}
	db := client.Database(dbName)
	cleanup := func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = db.Drop(c)
		_ = client.Disconnect(c)
	}
	return db, cleanup
}

// mongoDoc returns the (single) doc keyed by _id from a collection, or nil
// when absent.
func mongoDoc(t *testing.T, m *MongoDB, collection, id string) bson.M {
	t.Helper()
	var doc bson.M
	err := m.Collection(collection).FindOne(context.Background(), bson.M{"_id": id}).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil
		}
		t.Fatalf("FindOne: %v", err)
	}
	return doc
}
