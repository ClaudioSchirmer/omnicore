//go:build sqlserver

package sqlserver

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// sqlserverTxHandle is the SQL Server implementation of persistence.TxHandle
// handed to in-TX lifecycle hooks. It embeds persistence.SealedTxHandle to
// satisfy the sealed marker (the same mechanism the other engines use) and
// carries the live *sql.Tx privately, recovered by an in-TX side-effect adapter
// via UnwrapSQLServerTx.
type sqlserverTxHandle struct {
	persistence.SealedTxHandle
	tx *sql.Tx
}

func newSQLServerTxHandle(tx *sql.Tx) persistence.TxHandle { return &sqlserverTxHandle{tx: tx} }

// UnwrapSQLServerTx recovers the *sql.Tx from a persistence.TxHandle. It is the
// SQL Server counterpart of UnwrapMySQLTx / UnwrapPgxTx: an in-TX side-effect
// adapter that deliberately targets SQL Server calls it to run its own SQL
// inside the framework's TX. Using it pins that adapter to SQL Server. Panics
// on a foreign handle.
func UnwrapSQLServerTx(tx persistence.TxHandle) *sql.Tx {
	h, ok := tx.(*sqlserverTxHandle)
	if !ok {
		panic(fmt.Sprintf("sqlserver.UnwrapSQLServerTx: foreign persistence.TxHandle %T; only the SQL Server engine's handle is supported", tx))
	}
	return h.tx
}

// NeutralTx exposes the live *sql.Tx through the backend-neutral core.Tx
// surface — the SQL Server side of the canonical core.UnwrapTx bridge. The
// exported method satisfies infra's neutralTxCarrier across the package
// boundary.
func (h *sqlserverTxHandle) NeutralTx() core.Tx { return sqlserverTx{tx: h.tx} }

// sqlserverTx adapts a live *sql.Tx to core.WriteTx. *sql.Rows / *sql.Row
// satisfy core.Rows / core.Row directly; ExecCount reads
// sql.Result.RowsAffected; Commit/Rollback drive the lifecycle (the *sql.Tx
// methods take no ctx); Handle seals the tx for lifecycle hooks; Dialect is the
// SQL Server flavor.
type sqlserverTx struct{ tx *sql.Tx }

// Compile-time proof sqlserverTx satisfies the neutral write surface.
var _ core.WriteTx = sqlserverTx{}

func (t sqlserverTx) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, sqlText, args...)
	return err
}

func (t sqlserverTx) ExecCount(ctx context.Context, sqlText string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t sqlserverTx) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	return t.tx.QueryContext(ctx, sqlText, args...)
}

func (t sqlserverTx) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return t.tx.QueryRowContext(ctx, sqlText, args...)
}

func (t sqlserverTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t sqlserverTx) Rollback(context.Context) error { return t.tx.Rollback() }

// Handle seals the live tx as a persistence.TxHandle — the same handle the
// lifecycle hooks receive (and recover via core.UnwrapTx / UnwrapSQLServerTx).
func (t sqlserverTx) Handle() persistence.TxHandle { return newSQLServerTxHandle(t.tx) }

func (t sqlserverTx) Dialect() core.Dialect { return sqlserverDialect{} }

// The lifecycle-hook dispatch (positions A/D) + the observability hook-error
// line live once on the embedded write.BaseEngine
// (FireAfterBegin/FireBeforeCommit), reached through the neutral core.WriteTx
// (sqlserverTx) — this engine carries no copy of its own.
