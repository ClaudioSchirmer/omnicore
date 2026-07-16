//go:build oracle

package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/golang-migrate/migrate/v4/database"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// oracleDriver is a hand-rolled golang-migrate database.Driver for Oracle —
// golang-migrate ships no in-tree Oracle driver (verified against v4), so the
// framework carries its own over go-ora. It is engine/driver-layer internal:
// constructed only by NewOracle (oracle_runner.go), never registered globally.
//
// Semantics mirror the stock mysql/sqlserver drivers:
//   - the tracking table holds AT MOST one row (version NUMBER(19), dirty
//     BOOLEAN — the 23ai native boolean), replaced inside a transaction on
//     every SetVersion;
//   - Lock serializes concurrent migration runs via a session-scoped
//     DBMS_LOCK held on a pinned connection (the same package the engine's
//     rebuild lock uses — GRANT EXECUTE ON SYS.DBMS_LOCK is already the
//     documented operational requirement), returning database.ErrLocked on
//     timeout;
//   - Run splits the migration body into statements on top-level semicolons
//     (Oracle's driver protocol takes one statement per call and rejects the
//     trailing semicolon) — plain SQL only, PL/SQL blocks are not supported
//     in migration files (documented in the manual).
type oracleDriver struct {
	db       *sql.DB
	table    string
	lockConn *sql.Conn // pinned session holding the DBMS_LOCK migration lock
}

// Compile-time proof the hand-rolled driver satisfies golang-migrate's port.
var _ database.Driver = (*oracleDriver)(nil)

// newOracleDriver wraps an open pool for one tracking table and ensures the
// table exists (CREATE TABLE IF NOT EXISTS — native on the 23ai floor). The
// table name comes from the framework's constants, validated all the same.
func newOracleDriver(db *sql.DB, trackingTable string) (*oracleDriver, error) {
	if !core.SafeIdentifier(trackingTable) {
		return nil, fmt.Errorf("invalid tracking table %q", trackingTable)
	}
	d := &oracleDriver{db: db, table: trackingTable}
	if _, err := db.ExecContext(context.Background(),
		"CREATE TABLE IF NOT EXISTS "+trackingTable+" (version NUMBER(19) NOT NULL, dirty BOOLEAN NOT NULL)"); err != nil {
		return nil, fmt.Errorf("ensure tracking table %s: %w", trackingTable, err)
	}
	return d, nil
}

// Open is the registry entry point golang-migrate calls for URL-opened
// drivers. This driver is instance-only (migrate.NewWithInstance via
// NewOracle) — the framework never registers it globally.
func (d *oracleDriver) Open(string) (database.Driver, error) {
	return nil, errors.New("migration[oracle]: URL open unsupported; construct via migration.NewOracle")
}

// Close closes the pool this driver owns (opened by NewOracle's openDriver,
// one per migrate instance). A still-pinned lock connection is released first
// as the backstop — Unlock is the normal path.
func (d *oracleDriver) Close() error {
	if d.lockConn != nil {
		_ = d.lockConn.Close()
		d.lockConn = nil
	}
	return d.db.Close()
}

// Lock takes the migration lock: DBMS_LOCK ALLOCATE_UNIQUE + REQUEST
// (exclusive, session-scoped — release_on_commit FALSE so the SetVersion
// transactions under it do not drop it) on a pinned connection. REQUEST waits
// up to 60s for a concurrent migrator, then reports database.ErrLocked.
// Result codes: 0 granted, 1 timeout, 4 already owned by this session; the
// rest are failures.
func (d *oracleDriver) Lock() error {
	if d.lockConn != nil {
		return nil
	}
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration[oracle]: lock connection: %w", err)
	}
	const acquireSQL = `DECLARE
  v_handle VARCHAR2(128);
BEGIN
  DBMS_LOCK.ALLOCATE_UNIQUE(:1, v_handle);
  :2 := DBMS_LOCK.REQUEST(v_handle, DBMS_LOCK.X_MODE, 60, FALSE);
END;`
	var res int64
	if _, err := conn.ExecContext(ctx, acquireSQL, d.lockName(), sql.Out{Dest: &res}); err != nil {
		_ = conn.Close()
		return fmt.Errorf("migration[oracle]: DBMS_LOCK.REQUEST (missing GRANT EXECUTE ON SYS.DBMS_LOCK?): %w", err)
	}
	switch res {
	case 0, 4:
		d.lockConn = conn
		return nil
	case 1:
		_ = conn.Close()
		return database.ErrLocked
	default:
		_ = conn.Close()
		return fmt.Errorf("migration[oracle]: DBMS_LOCK.REQUEST failed with status %d", res)
	}
}

// Unlock releases the migration lock and returns the pinned connection to the
// pool. Idempotent; closing the session is the auto-release backstop.
func (d *oracleDriver) Unlock() error {
	if d.lockConn == nil {
		return nil
	}
	conn := d.lockConn
	d.lockConn = nil
	const releaseSQL = `DECLARE
  v_handle VARCHAR2(128);
BEGIN
  DBMS_LOCK.ALLOCATE_UNIQUE(:1, v_handle);
  :2 := DBMS_LOCK.RELEASE(v_handle);
END;`
	var res int64
	_, err := conn.ExecContext(context.Background(), releaseSQL, d.lockName(), sql.Out{Dest: &res})
	_ = conn.Close()
	if err != nil {
		return fmt.Errorf("migration[oracle]: DBMS_LOCK.RELEASE: %w", err)
	}
	if res != 0 {
		return fmt.Errorf("migration[oracle]: DBMS_LOCK.RELEASE did not release (status %d)", res)
	}
	return nil
}

// lockName derives the DBMS_LOCK name for this tracking table — framework-
// namespaced ("omcmig_"; ALLOCATE_UNIQUE names are database-global) and
// distinct per table so the framework and service stages could interleave
// across processes without false contention.
func (d *oracleDriver) lockName() string { return "omcmig_" + d.table }

// Run applies one migration body: split into statements on top-level
// semicolons and executed in order. A failing statement surfaces as
// database.Error carrying the statement text, and leaves the tracking row
// dirty (golang-migrate's contract — SetVersion ran before Run).
func (d *oracleDriver) Run(migration io.Reader) error {
	body, err := io.ReadAll(migration)
	if err != nil {
		return fmt.Errorf("migration[oracle]: read migration: %w", err)
	}
	ctx := context.Background()
	for _, stmt := range splitOracleStatements(string(body)) {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return database.Error{OrigErr: err, Err: "migration failed", Query: []byte(stmt)}
		}
	}
	return nil
}

// SetVersion replaces the single tracking row (version + dirty) inside one
// transaction — the stock drivers' discipline. database.NilVersion with a
// clean state leaves the table empty.
func (d *oracleDriver) SetVersion(version int, dirty bool) error {
	ctx := context.Background()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration[oracle]: begin: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM "+d.table); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("migration[oracle]: clear version: %w", err)
	}
	if version >= 0 || dirty {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO "+d.table+" (version, dirty) VALUES (:1, :2)", int64(version), dirty); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration[oracle]: set version: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration[oracle]: commit version: %w", err)
	}
	return nil
}

// Version reads the tracking row; an empty table is database.NilVersion.
func (d *oracleDriver) Version() (int, bool, error) {
	var (
		v     int64
		dirty bool
	)
	err := d.db.QueryRowContext(context.Background(),
		"SELECT version, dirty FROM "+d.table+" FETCH FIRST 1 ROWS ONLY").Scan(&v, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return database.NilVersion, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("migration[oracle]: read version: %w", err)
	}
	return int(v), dirty, nil
}

// Drop removes every table in the connected schema (golang-migrate's
// destructive reset, used by tooling/tests — never by the boot path). Names
// are read from user_tables and dropped quoted (the catalog's exact case).
func (d *oracleDriver) Drop() error {
	ctx := context.Background()
	rows, err := d.db.QueryContext(ctx, "SELECT table_name FROM user_tables")
	if err != nil {
		return fmt.Errorf("migration[oracle]: list tables: %w", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("migration[oracle]: scan table name: %w", err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migration[oracle]: list tables: %w", err)
	}
	for _, name := range tables {
		if _, err := d.db.ExecContext(ctx, `DROP TABLE "`+name+`" CASCADE CONSTRAINTS PURGE`); err != nil {
			return fmt.Errorf("migration[oracle]: drop %s: %w", name, err)
		}
	}
	return nil
}

// splitOracleStatements cuts a migration body into individual statements on
// top-level semicolons — outside single-quoted strings, double-quoted
// identifiers, and -- / /* */ comments. Oracle's wire protocol takes one
// statement per execute and rejects the trailing semicolon, so the runner
// feeds statements one by one (go-mssqldb/mysql run multi-statement batches
// natively; Oracle does not). Comment-only or whitespace-only segments are
// dropped. PL/SQL blocks (BEGIN…END with internal semicolons) are NOT
// supported in migration files — plain SQL only, per the manual.
func splitOracleStatements(body string) []string {
	var (
		stmts []string
		start int
	)
	const (
		stNormal = iota
		stLineComment
		stBlockComment
		stSingleQ
		stDoubleQ
	)
	state := stNormal
	flush := func(end int) {
		stmt := strings.TrimSpace(body[start:end])
		if stmt != "" && !commentOnly(stmt) {
			stmts = append(stmts, stmt)
		}
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch state {
		case stNormal:
			switch {
			case c == ';':
				flush(i)
				start = i + 1
			case c == '\'':
				state = stSingleQ
			case c == '"':
				state = stDoubleQ
			case c == '-' && i+1 < len(body) && body[i+1] == '-':
				state = stLineComment
				i++
			case c == '/' && i+1 < len(body) && body[i+1] == '*':
				state = stBlockComment
				i++
			}
		case stLineComment:
			if c == '\n' {
				state = stNormal
			}
		case stBlockComment:
			if c == '*' && i+1 < len(body) && body[i+1] == '/' {
				state = stNormal
				i++
			}
		case stSingleQ:
			if c == '\'' {
				// '' is the escaped quote — stay in the string.
				if i+1 < len(body) && body[i+1] == '\'' {
					i++
					continue
				}
				state = stNormal
			}
		case stDoubleQ:
			if c == '"' {
				state = stNormal
			}
		}
	}
	flush(len(body))
	return stmts
}

// commentOnly reports whether a trimmed segment carries no executable SQL —
// only -- lines and /* */ blocks (e.g. the file's trailing banner, or the
// text between the last semicolon and EOF).
func commentOnly(stmt string) bool {
	for i := 0; i < len(stmt); {
		switch {
		case stmt[i] == ' ' || stmt[i] == '\t' || stmt[i] == '\n' || stmt[i] == '\r':
			i++
		case stmt[i] == '-' && i+1 < len(stmt) && stmt[i+1] == '-':
			for i < len(stmt) && stmt[i] != '\n' {
				i++
			}
		case stmt[i] == '/' && i+1 < len(stmt) && stmt[i+1] == '*':
			end := strings.Index(stmt[i+2:], "*/")
			if end < 0 {
				return true
			}
			i += end + 4
		default:
			return false
		}
	}
	return true
}
