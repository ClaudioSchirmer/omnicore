//go:build sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// AcquireRebuildLock implements core.RelationalEngine for SQL Server: it pins a
// pool connection (*sql.Conn = one session), takes the named application lock
// via sp_getapplock, and wraps both in a core.RebuildLock. The lock is taken
// with @LockOwner = 'Session' — MANDATORY: a 'Transaction'-owned applock dies
// at the next COMMIT, and the rebuild lock must outlive the status-write
// transactions that run under it. Session-owned applocks auto-release when the
// session ends — the SQL Server twin of pg_advisory_lock / GET_LOCK. Holding
// the pinned conn for the lock's lifetime keeps sp_releaseapplock (and the
// auto-release on disconnect) on the right session; the handle's Querier runs
// the status writes on it too.
//
// The non-blocking try uses @LockTimeout = 0: sp_getapplock returns >= 0 on
// acquisition (0 granted, 1 granted after releasing waiters), -1 on timeout
// (another session holds it), and other negatives (-2 canceled, -3 deadlock
// victim, -999 error) on failure. The return value travels back through a
// SELECT in the same batch.
func (e *Engine) AcquireRebuildLock(ctx context.Context, viewName string) (core.RebuildLock, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire sqlserver connection for rebuild lock on %q: %w", viewName, err)
	}
	name := rebuildLockName(viewName)
	const acquireSQL = `DECLARE @res INT;
EXEC @res = sp_getapplock @Resource = @p1, @LockMode = 'Exclusive', @LockOwner = 'Session', @LockTimeout = 0;
SELECT @res;`
	var res sql.NullInt64
	if err := conn.QueryRowContext(ctx, acquireSQL, name).Scan(&res); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("sp_getapplock for view %q: %w", viewName, err)
	}
	if !res.Valid {
		_ = conn.Close()
		return nil, fmt.Errorf("sp_getapplock for view %q returned NULL", viewName)
	}
	if res.Int64 < -1 {
		_ = conn.Close()
		return nil, fmt.Errorf("sp_getapplock for view %q failed with status %d", viewName, res.Int64)
	}
	lock := &sqlserverRebuildLock{conn: conn, name: name, viewName: viewName, acquired: res.Int64 >= 0}
	if !lock.acquired {
		lock.holder = readLockHolder(ctx, conn, name)
	}
	return lock, nil
}

// sqlserverRebuildLock is the SQL Server core.RebuildLock: a pinned *sql.Conn
// holding the session-owned application lock plus the pinned-session Querier.
type sqlserverRebuildLock struct {
	conn     *sql.Conn
	name     string
	viewName string
	acquired bool
	holder   string
	released bool
}

func (l *sqlserverRebuildLock) Acquired() bool { return l.acquired }
func (l *sqlserverRebuildLock) Holder() string { return l.holder }

// Querier exposes the pinned connection through the neutral surface so
// BeginRebuild/EndRebuild run on the very session that owns the lock.
func (l *sqlserverRebuildLock) Querier() core.Querier { return sqlserverQuerier{exec: l.conn} }

// Release runs sp_releaseapplock (when this caller held it) and returns the
// connection to the pool. Closing the *sql.Conn ends the session and
// auto-releases the lock as the backstop; the explicit release surfaces an
// error to the caller's slog. Idempotent.
func (l *sqlserverRebuildLock) Release(ctx context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	var unlockErr error
	if l.acquired {
		const releaseSQL = `DECLARE @res INT;
EXEC @res = sp_releaseapplock @Resource = @p1, @LockOwner = 'Session';
SELECT @res;`
		var res sql.NullInt64
		if err := l.conn.QueryRowContext(ctx, releaseSQL, l.name).Scan(&res); err != nil {
			unlockErr = fmt.Errorf("sp_releaseapplock for view %q: %w", l.viewName, err)
		} else if !res.Valid || res.Int64 < 0 {
			unlockErr = fmt.Errorf("sp_releaseapplock for view %q did not release (status %v) — lock was not held on this session", l.viewName, res.Int64)
		}
	}
	_ = l.conn.Close()
	return unlockErr
}

// rebuildLockName derives the application-lock resource name for a view: an
// "omcv_" marker (isolating the framework's keyspace from any consumer
// sp_getapplock use of a bare name) followed by the hex FNV-64a of the view
// name — identical derivation to the MySQL engine. The result is a fixed
// ≤21 chars, far under SQL Server's 255-char resource limit, and hash-stable
// regardless of the database collation's case rules.
func rebuildLockName(viewName string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(viewName))
	return "omcv_" + strconv.FormatUint(h.Sum64(), 16)
}

// readLockHolder returns a best-effort description of the session currently
// holding the named application lock, via sys.dm_tran_locks (+ the session's
// host from sys.dm_exec_sessions). Any error degrades to "" — the holder line
// is a diagnostic, never load-bearing.
func readLockHolder(ctx context.Context, conn *sql.Conn, name string) string {
	const holderSQL = `SELECT TOP 1 l.request_session_id, ISNULL(s.host_name, '')
FROM sys.dm_tran_locks AS l
LEFT JOIN sys.dm_exec_sessions AS s ON s.session_id = l.request_session_id
WHERE l.resource_type = 'APPLICATION' AND l.request_status = 'GRANT' AND l.resource_description LIKE @p1`
	var (
		sessionID int64
		host      string
	)
	if err := conn.QueryRowContext(ctx, holderSQL, "%"+name+"%").Scan(&sessionID, &host); err != nil {
		return ""
	}
	if host != "" {
		return fmt.Sprintf("session=%d host=%s", sessionID, host)
	}
	return fmt.Sprintf("session=%d", sessionID)
}
