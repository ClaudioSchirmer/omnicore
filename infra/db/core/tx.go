package core

import (
	"context"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// Tx is the backend-neutral in-TX surface handed to canonical in-TX side-effect
// adapters via UnwrapTx. Exec/Query/QueryRow run inside the framework's open
// transaction; Dialect renders the engine-specific statement bits (placeholders,
// identifier quoting) so the adapter's SQL is backend-neutral. Each engine adapts
// its live tx (pgx.Tx on Postgres, *sql.Tx on MySQL) to this surface.
type Tx interface {
	Exec(ctx context.Context, sql string, args ...any) error
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) Row
	Dialect() Dialect
}

// WriteTx is the framework-owned in-TX surface for the write path: the neutral
// Tx (Exec/Query/QueryRow + Dialect) plus the transaction lifecycle and the
// rows-affected count. It is what lets the backend-neutral persistence
// orchestration (outbox, audit, hooks — and, in a later phase, the data writes
// themselves) live once in infra/db and run on ANY engine. Each engine adapts
// its live driver tx: pgx.Tx on Postgres, *sql.Tx on MySQL.
//
//   - ExecCount returns the rows-affected count (pgconn.CommandTag.RowsAffected
//     / sql.Result.RowsAffected), so a 0-row UPDATE maps to NotFound uniformly
//     without a dialect-specific RETURNING + ErrNoRows dance.
//   - Handle returns the sealed persistence.TxHandle the lifecycle hooks fire
//     against, so the shared dispatcher names no driver type — each engine's
//     adapter builds its own handle (newPgxTxHandle / newMySQLTxHandle).
type WriteTx interface {
	Tx
	ExecCount(ctx context.Context, sql string, args ...any) (int64, error)
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
	Handle() persistence.TxHandle
}

// WriteBeginner is the one new engine capability the neutral write
// orchestration depends on: open a framework-owned write TX. It stays off the
// public RelationalEngine port (the consumer-facing shape is unchanged); the
// shared orchestration resolves it from the concrete engine at construction.
type WriteBeginner interface {
	Begin(ctx context.Context) (WriteTx, error)
}

// neutralTxCarrier is the bridge each engine's TxHandle implements so UnwrapTx
// can recover the neutral Tx. The method is EXPORTED on purpose: an engine's
// handle lives in its own package, and only an exported method is satisfiable
// across the package boundary. The handle is still returned as the sealed
// persistence.TxHandle, so the method is reachable only through UnwrapTx, never by
// application code.
type neutralTxCarrier interface{ NeutralTx() Tx }

// UnwrapTx is the canonical in-TX bridge: it recovers the backend-neutral Tx from
// a sealed persistence.TxHandle so a framework or consumer in-TX adapter runs its
// side effect on ANY engine (render SQL via tx.Dialect(), execute via tx.Exec).
// UnwrapPgxTx (in infra/db/pg) survives as a PG-only escape hatch for an adapter
// that deliberately hard-codes Postgres.
//
// The panic guards a foreign TxHandle that does not carry a neutral Tx — failing
// loudly at the unwrap site beats a nil exec that NPEs deep in a SQL call.
func UnwrapTx(tx persistence.TxHandle) Tx {
	c, ok := tx.(neutralTxCarrier)
	if !ok {
		panic(fmt.Sprintf("db.UnwrapTx: persistence.TxHandle %T does not carry a neutral Tx", tx))
	}
	return c.NeutralTx()
}
