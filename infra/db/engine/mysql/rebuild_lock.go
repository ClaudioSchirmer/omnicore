//go:build mysql

package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// AcquireRebuildLock implements core.RelationalEngine for MySQL: it pins a pool
// connection (*sql.Conn = one session), takes the named user-level lock via
// GET_LOCK(name, 0), and wraps both in a core.RebuildLock. GET_LOCK is
// session-scoped and auto-releases when the session ends — the database/sql twin
// of Postgres' connection-bound pg_advisory_lock. Holding the pinned conn for the
// lock's lifetime is what keeps RELEASE_LOCK (and the auto-release on disconnect)
// on the right session; the handle's Querier runs the status writes on it too.
//
// The non-blocking try uses timeout 0 (return immediately): GET_LOCK returns 1 on
// acquisition, 0 when another session holds it, NULL on an error during
// acquisition. MySQL 8 (the supported floor) allows multiple named locks per
// session, so one rebuilder taking several view locks across its run is fine.
func (e *Engine) AcquireRebuildLock(ctx context.Context, viewName string) (core.RebuildLock, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire mysql connection for rebuild lock on %q: %w", viewName, err)
	}
	name := rebuildLockName(viewName)
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("GET_LOCK for view %q: %w", viewName, err)
	}
	if !got.Valid {
		_ = conn.Close()
		return nil, fmt.Errorf("GET_LOCK for view %q returned NULL (error during lock acquisition)", viewName)
	}
	lock := &mysqlRebuildLock{conn: conn, name: name, viewName: viewName, acquired: got.Int64 == 1}
	if !lock.acquired {
		lock.holder = readLockHolder(ctx, conn, name)
	}
	return lock, nil
}

// mysqlRebuildLock is the MySQL core.RebuildLock: a pinned *sql.Conn holding the
// named user-level lock plus the pinned-session Querier.
type mysqlRebuildLock struct {
	conn     *sql.Conn
	name     string
	viewName string
	acquired bool
	holder   string
	released bool
}

func (l *mysqlRebuildLock) Acquired() bool { return l.acquired }
func (l *mysqlRebuildLock) Holder() string { return l.holder }

// Querier exposes the pinned connection through the neutral surface so
// BeginRebuild/EndRebuild run on the very session that owns the lock.
func (l *mysqlRebuildLock) Querier() core.Querier { return mysqlQuerier{exec: l.conn} }

// Release runs RELEASE_LOCK (when this caller held it) and returns the connection
// to the pool. Closing the *sql.Conn auto-releases the lock as the backstop; the
// explicit RELEASE_LOCK surfaces an error to the caller's slog. Idempotent.
func (l *mysqlRebuildLock) Release(ctx context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	var unlockErr error
	if l.acquired {
		var rel sql.NullInt64
		if err := l.conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", l.name).Scan(&rel); err != nil {
			unlockErr = fmt.Errorf("RELEASE_LOCK for view %q: %w", l.viewName, err)
		} else if !rel.Valid || rel.Int64 != 1 {
			unlockErr = fmt.Errorf("RELEASE_LOCK for view %q did not release — lock was not held on this session", l.viewName)
		}
	}
	_ = l.conn.Close()
	return unlockErr
}

// rebuildLockName derives the MySQL user-level lock name for a view: an "omcv_"
// marker (isolating the framework's keyspace from any consumer GET_LOCK use of a
// bare name) followed by the hex FNV-64a of the view name. The result is a
// fixed ≤21 chars, well under MySQL's 64-char lock-name limit, and case-stable
// (lock names are case-insensitive, so a hashed name avoids name collisions two
// differently-cased view names could otherwise hit).
func rebuildLockName(viewName string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(viewName))
	return "omcv_" + strconv.FormatUint(h.Sum64(), 16)
}

// readLockHolder returns a best-effort description of the session currently
// holding the named lock: IS_USED_LOCK yields the holder's connection id, then
// performance_schema.threads (when enabled) yields its host. Any error degrades
// to "" — the holder line is a diagnostic, never load-bearing.
func readLockHolder(ctx context.Context, conn *sql.Conn, name string) string {
	var connID sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT IS_USED_LOCK(?)", name).Scan(&connID); err != nil || !connID.Valid {
		return ""
	}
	var host sql.NullString
	_ = conn.QueryRowContext(ctx,
		"SELECT PROCESSLIST_HOST FROM performance_schema.threads WHERE PROCESSLIST_ID = ?",
		connID.Int64).Scan(&host)
	if host.Valid && host.String != "" {
		return fmt.Sprintf("connection=%d host=%s", connID.Int64, host.String)
	}
	return fmt.Sprintf("connection=%d", connID.Int64)
}
