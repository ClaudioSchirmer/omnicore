package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// pgxTxHandle is the Postgres implementation of persistence.TxHandle. It carries
// the live pgx.Tx privately so only this package recovers it (via UnwrapPgxTx or
// the neutral core.UnwrapTx bridge). The persister builds one per lifecycle-hook
// firing and discards it when the TX ends.
//
// persistence.SealedTxHandle is embedded so pgxTxHandle inherits the unexported
// sealing method persistence.TxHandle requires — the concrete adapter lives here
// (where it can import pgx) while the seal is enforced by application/persistence.
type pgxTxHandle struct {
	persistence.SealedTxHandle
	tx pgx.Tx
}

// newPgxTxHandle is the only constructor for the PG TxHandle adapter.
func newPgxTxHandle(tx pgx.Tx) persistence.TxHandle {
	return &pgxTxHandle{tx: tx}
}

// UnwrapPgxTx recovers the underlying pgx.Tx from a persistence.TxHandle — the
// PG-only escape hatch for an in-TX adapter that deliberately targets Postgres.
// The canonical, backend-neutral bridge is core.UnwrapTx. Panics on a foreign
// handle (failing loudly beats a nil pgx.Tx that NPEs in a SQL call).
func UnwrapPgxTx(tx persistence.TxHandle) pgx.Tx {
	h, ok := tx.(*pgxTxHandle)
	if !ok {
		panic(fmt.Sprintf("infra.UnwrapPgxTx: foreign persistence.TxHandle implementation %T; only the framework's own pgxTxHandle is supported", tx))
	}
	return h.tx
}

// NeutralTx exposes the live pgx.Tx through the backend-neutral core.Tx surface —
// the Postgres side of the core.UnwrapTx bridge. Exported so core.UnwrapTx (which
// type-asserts the neutralTxCarrier interface) can reach it on a sealed handle.
func (h *pgxTxHandle) NeutralTx() core.Tx { return pgTx{tx: h.tx} }

// pgTx adapts a live pgx.Tx to core.WriteTx: Exec drops the command tag, ExecCount
// reads it for the rows-affected count, Query wraps pgx.Rows as core.Rows (pgRows
// swallows the no-error Close), Commit/Rollback drive the lifecycle, Handle seals
// the tx for lifecycle hooks, and core.Dialect is the Postgres flavor.
type pgTx struct{ tx pgx.Tx }

// Compile-time proof pgTx satisfies the neutral write surface.
var _ core.WriteTx = pgTx{}

func (t pgTx) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := t.tx.Exec(ctx, sql, args...)
	return err
}

func (t pgTx) ExecCount(ctx context.Context, sql string, args ...any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t pgTx) Query(ctx context.Context, sql string, args ...any) (core.Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgRows{rows}, nil
}

func (t pgTx) QueryRow(ctx context.Context, sql string, args ...any) core.Row {
	return t.tx.QueryRow(ctx, sql, args...)
}

func (t pgTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t pgTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// Handle seals the live tx as a persistence.TxHandle — the same handle the
// lifecycle hooks receive (and recover via core.UnwrapTx / UnwrapPgxTx).
func (t pgTx) Handle() persistence.TxHandle { return newPgxTxHandle(t.tx) }

func (t pgTx) Dialect() core.Dialect { return pgDialect{} }
