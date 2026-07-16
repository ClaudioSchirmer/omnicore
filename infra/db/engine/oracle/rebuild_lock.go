//go:build oracle

package oracle

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// AcquireRebuildLock implements core.RelationalEngine for Oracle: it pins a
// pool connection (*sql.Conn = one session), takes the named application lock
// via DBMS_LOCK (ALLOCATE_UNIQUE derives a stable handle for the name; REQUEST
// takes it exclusive), and wraps both in a core.RebuildLock. The lock is
// requested with release_on_commit => FALSE — MANDATORY: a commit-released
// lock dies at the next COMMIT, and the rebuild lock must outlive the
// status-write transactions that run under it. Session-scoped DBMS_LOCK locks
// auto-release when the session ends — the Oracle twin of pg_advisory_lock /
// GET_LOCK / sp_getapplock. Holding the pinned conn for the lock's lifetime
// keeps RELEASE (and the auto-release on disconnect) on the right session; the
// handle's Querier runs the status writes on it too.
//
// DBMS_LOCK.REQUEST with timeout 0 is the non-blocking try: 0 granted,
// 1 timeout (another session holds it), 4 already owned by this session (also
// counts as held), 2 deadlock / 3 parameter error / 5 illegal handle are
// failures. The result travels back through a PL/SQL OUT bind (sql.Out).
//
// Operational requirement (documented in the manual): the application user
// needs GRANT EXECUTE ON SYS.DBMS_LOCK — not a default grant; the QA bench
// provisions it in its init script.
func (e *Engine) AcquireRebuildLock(ctx context.Context, viewName string) (core.RebuildLock, error) {
	conn, err := e.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire oracle connection for rebuild lock on %q: %w", viewName, err)
	}
	name := rebuildLockName(viewName)
	const acquireSQL = `DECLARE
  v_handle VARCHAR2(128);
BEGIN
  DBMS_LOCK.ALLOCATE_UNIQUE(:1, v_handle);
  :2 := DBMS_LOCK.REQUEST(v_handle, DBMS_LOCK.X_MODE, 0, FALSE);
END;`
	var res int64
	if _, err := conn.ExecContext(ctx, acquireSQL, name, sql.Out{Dest: &res}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("DBMS_LOCK.REQUEST for view %q (missing GRANT EXECUTE ON SYS.DBMS_LOCK?): %w", viewName, err)
	}
	if res != 0 && res != 1 && res != 4 {
		_ = conn.Close()
		return nil, fmt.Errorf("DBMS_LOCK.REQUEST for view %q failed with status %d", viewName, res)
	}
	lock := &oracleRebuildLock{conn: conn, name: name, viewName: viewName, acquired: res == 0 || res == 4}
	if !lock.acquired {
		lock.holder = readLockHolder(ctx, conn, name)
	}
	return lock, nil
}

// oracleRebuildLock is the Oracle core.RebuildLock: a pinned *sql.Conn holding
// the session-scoped DBMS_LOCK lock plus the pinned-session Querier.
type oracleRebuildLock struct {
	conn     *sql.Conn
	name     string
	viewName string
	acquired bool
	holder   string
	released bool
}

func (l *oracleRebuildLock) Acquired() bool { return l.acquired }
func (l *oracleRebuildLock) Holder() string { return l.holder }

// Querier exposes the pinned connection through the neutral surface so
// BeginRebuild/EndRebuild run on the very session that owns the lock.
func (l *oracleRebuildLock) Querier() core.Querier { return oracleQuerier{exec: l.conn} }

// Release runs DBMS_LOCK.RELEASE (when this caller held it) and returns the
// connection to the pool. Closing the *sql.Conn ends the session and
// auto-releases the lock as the backstop; the explicit release surfaces an
// error to the caller's slog. Idempotent. RELEASE returns 0 on success,
// 4 when the lock is not owned by this session.
func (l *oracleRebuildLock) Release(ctx context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	var unlockErr error
	if l.acquired {
		const releaseSQL = `DECLARE
  v_handle VARCHAR2(128);
BEGIN
  DBMS_LOCK.ALLOCATE_UNIQUE(:1, v_handle);
  :2 := DBMS_LOCK.RELEASE(v_handle);
END;`
		var res int64
		if _, err := l.conn.ExecContext(ctx, releaseSQL, l.name, sql.Out{Dest: &res}); err != nil {
			unlockErr = fmt.Errorf("DBMS_LOCK.RELEASE for view %q: %w", l.viewName, err)
		} else if res != 0 {
			unlockErr = fmt.Errorf("DBMS_LOCK.RELEASE for view %q did not release (status %d) — lock was not held on this session", l.viewName, res)
		}
	}
	_ = l.conn.Close()
	return unlockErr
}

// rebuildLockName derives the DBMS_LOCK lock name for a view: an "omcv_"
// marker (isolating the framework's keyspace from any consumer DBMS_LOCK use
// of a bare name — ALLOCATE_UNIQUE names are database-global) followed by the
// hex FNV-64a of the view name — identical derivation to the other engines.
// The result is a fixed ≤21 chars, far under DBMS_LOCK's 128-char name limit,
// and hash-stable regardless of the database's NLS case rules.
func rebuildLockName(viewName string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(viewName))
	return "omcv_" + strconv.FormatUint(h.Sum64(), 16)
}

// readLockHolder returns a best-effort description of the session currently
// holding the named DBMS_LOCK lock, via sys.dbms_lock_allocated (name →
// lockid) joined to v$lock ('UL' user locks) and v$session. Reading those
// views needs privileges the application user typically lacks — any error
// degrades to "" (the holder line is a diagnostic, never load-bearing).
func readLockHolder(ctx context.Context, conn *sql.Conn, name string) string {
	const holderSQL = `SELECT s.sid, NVL(s.machine, '')
FROM sys.dbms_lock_allocated a
JOIN v$lock l ON l.id1 = a.lockid AND l.type = 'UL' AND l.lmode > 0
JOIN v$session s ON s.sid = l.sid
WHERE a.name = :1 FETCH FIRST 1 ROWS ONLY`
	var (
		sessionID int64
		machine   string
	)
	if err := conn.QueryRowContext(ctx, holderSQL, name).Scan(&sessionID, &machine); err != nil {
		return ""
	}
	if machine != "" {
		return fmt.Sprintf("session=%d machine=%s", sessionID, machine)
	}
	return fmt.Sprintf("session=%d", sessionID)
}
