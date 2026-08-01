//go:build sqlite

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// sqliteTxHandle is the SQLite implementation of persistence.TxHandle handed to
// in-TX lifecycle hooks. It embeds persistence.SealedTxHandle to satisfy the
// sealed marker (the same mechanism the other engines use) and carries the live
// *sql.Tx privately, recovered by an in-TX side-effect adapter via UnwrapSQLiteTx.
type sqliteTxHandle struct {
	persistence.SealedTxHandle
	tx *sql.Tx
}

func newSQLiteTxHandle(tx *sql.Tx) persistence.TxHandle { return &sqliteTxHandle{tx: tx} }

// UnwrapSQLiteTx recovers the *sql.Tx from a persistence.TxHandle. It is the
// SQLite counterpart of UnwrapOracleTx / UnwrapMySQLTx / UnwrapSQLServerTx /
// UnwrapPgxTx: an in-TX side-effect adapter that deliberately targets SQLite
// calls it to run its own SQL inside the framework's TX. Using it pins that
// adapter to SQLite. Panics on a foreign handle.
func UnwrapSQLiteTx(tx persistence.TxHandle) *sql.Tx {
	h, ok := tx.(*sqliteTxHandle)
	if !ok {
		panic(fmt.Sprintf("sqlite.UnwrapSQLiteTx: foreign persistence.TxHandle %T; only the SQLite engine's handle is supported", tx))
	}
	return h.tx
}

// NeutralTx exposes the live *sql.Tx through the backend-neutral core.Tx
// surface — the SQLite side of the canonical core.UnwrapTx bridge.
func (h *sqliteTxHandle) NeutralTx() core.Tx { return sqliteTx{tx: h.tx} }

// sqliteTx adapts a live *sql.Tx to core.WriteTx. *sql.Rows / *sql.Row satisfy
// core.Rows / core.Row directly; ExecCount reads sql.Result.RowsAffected;
// Commit/Rollback drive the lifecycle; Handle seals the tx for lifecycle hooks;
// Dialect is the SQLite flavor. NOTE: the read helpers here return the raw
// driver rows (no time-decoding wrapper), matching the write path's usage —
// the write orchestration reads back only ids/revisions/counts, never a
// timestamp column into a *time.Time (those are bound as args, see encodeArg).
type sqliteTx struct{ tx *sql.Tx }

var _ core.WriteTx = sqliteTx{}

func (t sqliteTx) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, sqlText, args...)
	return err
}

func (t sqliteTx) ExecCount(ctx context.Context, sqlText string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t sqliteTx) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	return t.tx.QueryContext(ctx, sqlText, args...)
}

func (t sqliteTx) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return t.tx.QueryRowContext(ctx, sqlText, args...)
}

func (t sqliteTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t sqliteTx) Rollback(context.Context) error { return t.tx.Rollback() }

// Handle seals the live tx as a persistence.TxHandle — the same handle the
// lifecycle hooks receive (and recover via core.UnwrapTx / UnwrapSQLiteTx).
func (t sqliteTx) Handle() persistence.TxHandle { return newSQLiteTxHandle(t.tx) }

func (t sqliteTx) Dialect() core.Dialect { return sqliteDialect{} }
