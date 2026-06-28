package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AcquireRebuildLock implements core.RelationalEngine: it pins a pool connection,
// tries the Postgres advisory lock for the view, and wraps both in a
// core.RebuildLock. The advisory lock is bound to THIS connection — holding the
// pinned conn for the lock's lifetime is what makes the unlock (and the
// auto-release on disconnect) land on the right session. The returned handle's
// Querier runs on that same pinned conn, so the rebuild's status writes share it.
func (p *Postgres) AcquireRebuildLock(ctx context.Context, viewName string) (core.RebuildLock, error) {
	conn, err := p.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire pg connection for rebuild lock on %q: %w", viewName, err)
	}
	acquired, err := tryAcquireViewLock(ctx, conn, viewName)
	if err != nil {
		conn.Release()
		return nil, err
	}
	lock := &pgRebuildLock{conn: conn, viewName: viewName, acquired: acquired}
	if !acquired {
		// Best-effort holder for the abort diagnostic. The pinned conn is held
		// until Release regardless; missing holder details degrade to "".
		if h, _ := readViewLockHolder(ctx, conn, viewName); h != nil {
			lock.holder = fmt.Sprintf("pid=%d application=%q client=%q", h.PID, h.ApplicationName, h.ClientAddr)
		}
	}
	return lock, nil
}

// pgRebuildLock is the Postgres core.RebuildLock: a pinned *pgxpool.Conn carrying
// the advisory lock plus the pinned-session Querier.
type pgRebuildLock struct {
	conn     *pgxpool.Conn
	viewName string
	acquired bool
	holder   string
	released bool
}

func (l *pgRebuildLock) Acquired() bool { return l.acquired }
func (l *pgRebuildLock) Holder() string { return l.holder }

// Querier exposes the pinned connection through the neutral read/exec surface so
// BeginRebuild/EndRebuild run on the very session that owns the advisory lock.
func (l *pgRebuildLock) Querier() core.Querier { return pgQuerier{e: l.conn} }

// Release unlocks (when this caller held the lock) and returns the connection to
// the pool. Releasing the conn auto-releases the advisory lock as the backstop;
// the explicit unlock is what surfaces an error to the caller's slog. Idempotent.
func (l *pgRebuildLock) Release(ctx context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	var unlockErr error
	if l.acquired {
		unlockErr = releaseViewLock(ctx, l.conn, l.viewName)
	}
	l.conn.Release()
	return unlockErr
}

// viewLockKeyPrefix isolates the framework's advisory-lock keyspace from any
// consumer-application advisory locks. Services may already use pg_advisory_lock
// for their own purposes; the prefix ensures keys derived here cannot collide.
const viewLockKeyPrefix = "omnicore.mongo.view.rebuild:"

// viewLockKey returns the deterministic int64 key used to acquire the PostgreSQL
// advisory lock for a given Mongo view. The mapping is FNV-64a over a prefixed
// string — deterministic across runs and processes, stable over arbitrarily many
// service restarts. Hash collisions in a 64-bit space are negligible at the
// expected scale (dozens of views per service across the whole stack).
func viewLockKey(viewName string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(viewLockKeyPrefix))
	_, _ = h.Write([]byte(viewName))
	// int64 wrap of the uint64 sum — pg_advisory_lock takes BIGINT (signed
	// int64). Bit pattern is preserved either way; PG doesn't care about the sign.
	return int64(h.Sum64())
}

// viewLockHolder describes the process that currently owns a rebuild advisory
// lock. Populated via pg_locks ⋈ pg_stat_activity. Best-effort — fields can be
// empty if the snapshot has already shifted by the time the query runs.
type viewLockHolder struct {
	PID             int32
	ApplicationName string
	ClientAddr      string
}

const (
	sqlTryAdvisoryLock = `SELECT pg_try_advisory_lock($1)`
	sqlAdvisoryUnlock  = `SELECT pg_advisory_unlock($1)`

	// pg_locks rows for advisory locks carry classid/objid as the upper and
	// lower halves of the bigint key. Splitting the key matches the PG documented
	// behaviour.
	sqlReadViewLockHolder = `
SELECT a.pid, a.application_name, COALESCE(host(a.client_addr), '')
FROM pg_locks l
JOIN pg_stat_activity a ON a.pid = l.pid
WHERE l.locktype = 'advisory'
  AND l.classid  = $1
  AND l.objid    = $2
  AND l.granted  = true
LIMIT 1`
)

// tryAcquireViewLock attempts to acquire the advisory lock for the given view on
// the pinned connection held by exec. Returns true when the lock was granted to
// this caller, false when another session already holds it. The lock is bound to
// the connection; the caller must hold it for the whole rebuild (the unlock and
// the disconnect auto-release both target this same session).
func tryAcquireViewLock(ctx context.Context, exec pgExec, viewName string) (bool, error) {
	key := viewLockKey(viewName)
	var acquired bool
	if err := exec.QueryRow(ctx, sqlTryAdvisoryLock, key).Scan(&acquired); err != nil {
		return false, fmt.Errorf("pg_try_advisory_lock for view %q: %w", viewName, err)
	}
	return acquired, nil
}

// releaseViewLock releases the advisory lock for the given view. Must be called
// on the SAME connection that acquired it. Returns nil on successful release; a
// false result means the lock was not held on this connection.
func releaseViewLock(ctx context.Context, exec pgExec, viewName string) error {
	key := viewLockKey(viewName)
	var released bool
	if err := exec.QueryRow(ctx, sqlAdvisoryUnlock, key).Scan(&released); err != nil {
		return fmt.Errorf("pg_advisory_unlock for view %q: %w", viewName, err)
	}
	if !released {
		return fmt.Errorf("pg_advisory_unlock for view %q returned false — lock was not held on this connection", viewName)
	}
	return nil
}

// readViewLockHolder returns the holder of the advisory lock for the given view
// by joining pg_locks against pg_stat_activity. Best-effort — if the lock is
// released between the failed TryAcquire and the read, returns (nil, nil).
func readViewLockHolder(ctx context.Context, exec pgExec, viewName string) (*viewLockHolder, error) {
	key := viewLockKey(viewName)
	classid, objid := splitAdvisoryKey(key)
	var h viewLockHolder
	err := exec.QueryRow(ctx, sqlReadViewLockHolder, classid, objid).Scan(&h.PID, &h.ApplicationName, &h.ClientAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read lock holder for view %q: %w", viewName, err)
	}
	return &h, nil
}

// splitAdvisoryKey turns the int64 lock key into the (classid, objid) pair
// pg_locks exposes for advisory locks (upper 32 bits → classid, lower 32 →
// objid). Both columns are oid (uint32 in pgx) — returning int32 would produce
// negative values pgx then refuses to bind.
func splitAdvisoryKey(key int64) (classid uint32, objid uint32) {
	classid = uint32(uint64(key) >> 32)
	objid = uint32(uint64(key) & 0xFFFFFFFF)
	return
}
