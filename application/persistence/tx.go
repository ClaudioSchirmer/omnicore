// Package persistence holds the application-layer port types for the
// persistence pipeline.
//
// The package carries three families of declarations consumed across the
// stack:
//
//   - TxHandle / CommandTag / Rows / Row — minimal in-TX surface exposed to
//     in-TX lifecycle hooks. Application code never imports pgx; infra/
//     wraps *pgx.Tx behind these interfaces.
//   - AfterBeginHook[T] / BeforeCommitHook[T] — function types declaring the
//     hook shape. Slot semantics: afterBegin fires INSIDE the TX before any
//     framework write; beforeCommit fires INSIDE the TX after all framework
//     writes (data + outbox + audit) and before COMMIT. A non-nil error
//     returned by either rolls the whole TX back; type identity preserved
//     end-to-end so domain.NotificationCarrier reaches the wire envelope
//     verbatim.
//   - AfterBeginHookProvider[T] / BeforeCommitHookProvider[T] — interfaces
//     detected by the Auto Command Handlers via type assertion. A Cmd that
//     declares AfterBegin / BeforeCommit satisfies the matching provider
//     automatically; the handler reads the methods and forwards them to the
//     Writer as WriteOption[T] closures.
//   - WriteOption[T] / WithAfterBegin / WithBeforeCommit — functional
//     options consumed by the Writer[T] port. Auto and manual handlers both
//     end up at the same Writer call; the only difference is how the
//     closures originate (Cmd method vs explicit closure).
//   - Writer[T] — typed write port that carries the variadic options. The
//     domain.Repository[T] interface still describes the read+write
//     contract pure to the domain layer; Writer[T] is the
//     application-layer surface infra.BaseRepository[T] also implements so
//     the Auto + manual handlers can pass the variadic without pulling
//     application/persistence imports into the domain.
package persistence

import (
	"context"
)

// TxHandle is the minimal in-TX surface exposed to lifecycle hooks. The
// concrete implementation lives in infra/ wrapping *pgx.Tx; application
// code never imports pgx.
//
// The surface is intentionally narrow:
//
//   - Exec / Query / QueryRow cover the canonical "write companion rows or
//     read state inside the framework's TX" use cases (extra outbox row,
//     denormalization, cross-table lookup).
//   - Begin / Commit / Rollback are absent — the TX lifecycle is the
//     framework's, not the hook's.
//   - CopyFrom / SendBatch / Prepare / LargeObjects are absent — advanced
//     features graduate to manual handlers that own their own TX.
//
// Consumers own the iterator: defer rows.Close() is the convention.
//
// Errors returned by Exec / Query / QueryRow are raw pgx errors —
// constraint-name mapping (infra.ConstraintBinding) lives at the
// Repository level and is intentionally NOT applied at this surface. A
// hook that wants "constraint X → typed notification" inspects the pgError
// and returns the typed notification explicitly via
// infra.SingleNotificationError or similar.
type TxHandle interface {
	Exec(ctx context.Context, sql string, args ...any) (CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// CommandTag is the framework-flat equivalent of pgconn.CommandTag.
// Carries only RowsAffected because that is the single field hooks
// actually consume; the rest of the pgx tag (sql verb, OID) belongs to
// the driver, not the hook contract.
type CommandTag struct {
	RowsAffected int64
}

// Rows mirrors the read-only subset of pgx.Rows the framework permits
// hooks to consume. Close() is mandatory — the wrapper is a Go iterator
// over the framework's TX and forgetting to close it leaks the underlying
// rows handle until GC.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// Row mirrors pgx.Row. Scan is the only method — when the query
// represents at most one row, the QueryRow path returns this directly so
// hooks can call .Scan on the result without juggling iteration.
type Row interface {
	Scan(dest ...any) error
}
