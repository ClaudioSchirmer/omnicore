package infra

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"

	"github.com/jackc/pgx/v5"
)

// viewLockKeyPrefix isolates the framework's advisory-lock keyspace from
// any consumer-application advisory locks. Services may already use
// pg_advisory_lock for their own purposes; the prefix ensures keys derived
// here cannot collide.
const viewLockKeyPrefix = "omnicore.mongo.view.rebuild:"

// ViewLockKey returns the deterministic int64 key used to acquire the
// PostgreSQL advisory lock for a given Mongo view. The mapping is FNV-64a
// over a prefixed string — deterministic across runs and processes, stable
// over arbitrarily many service restarts.
//
// Hash collisions in a 64-bit space are negligible at the expected scale
// (dozens of views per service across the whole stack).
func ViewLockKey(viewName string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(viewLockKeyPrefix))
	_, _ = h.Write([]byte(viewName))
	// int64 wrap of the uint64 sum — pg_advisory_lock takes BIGINT (signed
	// int64). Bit pattern is preserved either way; PG doesn't care about
	// the sign.
	return int64(h.Sum64())
}

// ViewLockHolder describes the process that currently owns a rebuild advisory
// lock. Populated via pg_locks ⋈ pg_stat_activity. Returned by
// ReadViewLockHolder; best-effort — fields can be empty if the snapshot has
// already shifted by the time the query runs.
type ViewLockHolder struct {
	PID             int32
	ApplicationName string
	ClientAddr      string
}

// SQL constants — exported for diagnostics + unit tests.
const (
	sqlTryAdvisoryLock = `SELECT pg_try_advisory_lock($1)`
	sqlAdvisoryUnlock  = `SELECT pg_advisory_unlock($1)`

	// pg_locks rows for advisory locks carry classid/objid as the upper
	// and lower halves of the bigint key. Splitting the key matches the
	// PG documented behaviour.
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

// TryAcquireViewLock attempts to acquire the advisory lock for the given
// view on the connection held by `exec`. Returns true when the lock was
// granted to this caller, false when another session already holds it.
//
// IMPORTANT: pg_advisory_lock is bound to the CONNECTION that acquired it.
// The caller must hold the connection (typically via *pgxpool.Conn pinned
// with pool.Acquire) for the entire rebuild duration. If a fresh pool
// connection is used per call, the unlock will fail to find the lock on
// the new connection and the lock will remain held until the original
// connection is returned to the pool and idle-timeouts. For Phase 2's
// rebuild integration, the orchestrator will use a pinned connection.
func TryAcquireViewLock(ctx context.Context, exec pgExec, viewName string) (bool, error) {
	key := ViewLockKey(viewName)
	var acquired bool
	err := exec.QueryRow(ctx, sqlTryAdvisoryLock, key).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("pg_try_advisory_lock for view %q: %w", viewName, err)
	}
	return acquired, nil
}

// ReleaseViewLock releases the advisory lock for the given view. Must be
// called on the SAME connection that acquired it. Returns nil on
// successful release; logs a warning at the call-site (via the rebuild
// orchestrator) on failure — the auto-release on connection close is the
// safety net for any release that doesn't land cleanly.
func ReleaseViewLock(ctx context.Context, exec pgExec, viewName string) error {
	key := ViewLockKey(viewName)
	var released bool
	err := exec.QueryRow(ctx, sqlAdvisoryUnlock, key).Scan(&released)
	if err != nil {
		return fmt.Errorf("pg_advisory_unlock for view %q: %w", viewName, err)
	}
	if !released {
		return fmt.Errorf("pg_advisory_unlock for view %q returned false — lock was not held on this connection", viewName)
	}
	return nil
}

// ReadViewLockHolder returns the holder of the advisory lock for the given
// view (PID, application_name, client_addr) by joining pg_locks against
// pg_stat_activity. Best-effort — if the lock is released between the
// failed TryAcquire and the read, returns (nil, nil). If the read itself
// fails, the error is wrapped.
//
// The diagnostic uses this to populate the "another instance is rebuilding"
// boot abort message with concrete details. Falling back to a generic
// message on missing data is acceptable.
func ReadViewLockHolder(ctx context.Context, exec pgExec, viewName string) (*ViewLockHolder, error) {
	key := ViewLockKey(viewName)
	classid, objid := splitAdvisoryKey(key)
	var h ViewLockHolder
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
// pg_locks exposes for advisory locks. PG documents the layout as the
// upper 32 bits → classid, lower 32 bits → objid. Both columns are of type
// oid in pg_locks, which pgx encodes as uint32 — returning int32 would
// silently produce negative values when the upper bit of the uint32 maps
// onto the sign bit of int32, and pgx then refuses the binding.
func splitAdvisoryKey(key int64) (classid uint32, objid uint32) {
	classid = uint32(uint64(key) >> 32)
	objid = uint32(uint64(key) & 0xFFFFFFFF)
	return
}
