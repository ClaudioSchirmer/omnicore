//go:build sqlite

package sqlite

import (
	"context"
	"sync"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// AcquireRebuildLock implements core.RelationalEngine for SQLite with a
// PROCESS-LOCAL mutex — which is, by construction, cluster-wide: SQLite is
// inherently single-node (one process owns the file), so there is exactly one
// process and a process mutex is the whole cluster (tasks/sql_mvp.md §A.4). The
// distributed advisory locks the other engines use (pg_advisory_lock, GET_LOCK,
// sp_getapplock, DBMS_LOCK) guard against a SECOND node running a concurrent
// rebuild; on SQLite there is no second node. This only ever matters if someone
// runs SQLite WITH Mongo rebuilds — which is not recommended (no CDC, relational
// views only) — and even then it is correct for the single-node reality.
//
// The lock keyspace is per view name; the pinned "connection" is simply the
// shared pool (MaxOpenConns=1 makes it a single session anyway), surfaced through
// the neutral Querier so BeginRebuild/EndRebuild run their status writes on it.
func (e *Engine) AcquireRebuildLock(_ context.Context, viewName string) (core.RebuildLock, error) {
	mu, _ := rebuildMutexes.LoadOrStore(viewName, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	acquired := m.TryLock()
	lock := &sqliteRebuildLock{engine: e, mu: m, acquired: acquired}
	if !acquired {
		lock.holder = "local process (a rebuild for this view is already in flight)"
	}
	return lock, nil
}

// rebuildMutexes holds one *sync.Mutex per view name for the process's lifetime;
// LoadOrStore guarantees a single mutex per view so N callers contend on the
// same lock.
var rebuildMutexes sync.Map

// sqliteRebuildLock is the SQLite core.RebuildLock: a held (or contended)
// process mutex plus the shared-pool Querier.
type sqliteRebuildLock struct {
	engine   *Engine
	mu       *sync.Mutex
	acquired bool
	holder   string
	released bool
}

func (l *sqliteRebuildLock) Acquired() bool { return l.acquired }
func (l *sqliteRebuildLock) Holder() string { return l.holder }

// Querier exposes the shared pool through the neutral surface (the single
// SQLite session, given MaxOpenConns=1).
func (l *sqliteRebuildLock) Querier() core.Querier { return sqliteQuerier{exec: l.engine.db} }

// Release unlocks the process mutex when this caller held it. Idempotent; a
// contended lock (never acquired) releases nothing.
func (l *sqliteRebuildLock) Release(context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	if l.acquired {
		l.mu.Unlock()
	}
	return nil
}
