package core

import "context"

// RebuildLock is the pinned-session handle the Mongo-view rebuild control plane
// (SyncEngine.ExecuteRebuild) holds for the duration of one view rebuild. It is
// the backend-neutral form of the hybrid concurrency primitive: a cluster-wide
// advisory lock acquired on a pinned connection, alongside the registry status
// column the rebuild flips done⟷processing.
//
//   - Acquired reports whether THIS caller won the mutex. When false, another
//     instance of the service is already rebuilding the view; the caller aborts
//     with a descriptive error (see Holder) and still Releases the handle.
//   - Holder is a best-effort human description of the current owner when
//     Acquired is false (e.g. "pid=42 application=svc client=10.0.0.3" on
//     Postgres, "connection=88 host=10.0.0.3" on MySQL). It may be "" when the
//     snapshot has already shifted or the engine cannot resolve the owner.
//   - Querier is bound to the pinned session, so BeginRebuild/EndRebuild run
//     their UPDATEs on the same connection that owns the lock — faithful to the
//     "lock + status writes share one connection" invariant. It is valid only
//     until Release.
//   - Release frees the lock (the explicit unlock surfaces a warning on failure;
//     dropping the session is the backstop) and returns the connection to the
//     pool. It is safe to call exactly once, including when Acquired is false
//     (then it just returns the borrowed connection, nothing to unlock).
type RebuildLock interface {
	Acquired() bool
	Holder() string
	Querier() Querier
	Release(ctx context.Context) error
}
