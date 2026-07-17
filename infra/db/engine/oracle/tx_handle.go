//go:build oracle

package oracle

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// oracleTxHandle is the Oracle implementation of persistence.TxHandle handed
// to in-TX lifecycle hooks. It embeds persistence.SealedTxHandle to satisfy
// the sealed marker (the same mechanism the other engines use) and carries the
// live *sql.Tx privately, recovered by an in-TX side-effect adapter via
// UnwrapOracleTx.
type oracleTxHandle struct {
	persistence.SealedTxHandle
	tx *sql.Tx
}

func newOracleTxHandle(tx *sql.Tx) persistence.TxHandle { return &oracleTxHandle{tx: tx} }

// UnwrapOracleTx recovers the *sql.Tx from a persistence.TxHandle. It is the
// Oracle counterpart of UnwrapMySQLTx / UnwrapSQLServerTx / UnwrapPgxTx: an
// in-TX side-effect adapter that deliberately targets Oracle calls it to run
// its own SQL inside the framework's TX. Using it pins that adapter to Oracle.
// Panics on a foreign handle.
func UnwrapOracleTx(tx persistence.TxHandle) *sql.Tx {
	h, ok := tx.(*oracleTxHandle)
	if !ok {
		panic(fmt.Sprintf("oracle.UnwrapOracleTx: foreign persistence.TxHandle %T; only the Oracle engine's handle is supported", tx))
	}
	return h.tx
}

// NeutralTx exposes the live *sql.Tx through the backend-neutral core.Tx
// surface — the Oracle side of the canonical core.UnwrapTx bridge. The
// exported method satisfies infra's neutralTxCarrier across the package
// boundary.
func (h *oracleTxHandle) NeutralTx() core.Tx { return oracleTx{tx: h.tx} }

// oracleTx adapts a live *sql.Tx to core.WriteTx. *sql.Rows / *sql.Row satisfy
// core.Rows / core.Row directly; ExecCount reads sql.Result.RowsAffected;
// Commit/Rollback drive the lifecycle (the *sql.Tx methods take no ctx);
// Handle seals the tx for lifecycle hooks; Dialect is the Oracle flavor.
type oracleTx struct{ tx *sql.Tx }

// Compile-time proof oracleTx satisfies the neutral write surface.
var _ core.WriteTx = oracleTx{}

func (t oracleTx) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, sqlText, args...)
	return err
}

func (t oracleTx) ExecCount(ctx context.Context, sqlText string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t oracleTx) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	return t.tx.QueryContext(ctx, sqlText, args...)
}

func (t oracleTx) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return t.tx.QueryRowContext(ctx, sqlText, args...)
}

func (t oracleTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t oracleTx) Rollback(context.Context) error { return t.tx.Rollback() }

// Handle seals the live tx as a persistence.TxHandle — the same handle the
// lifecycle hooks receive (and recover via core.UnwrapTx / UnwrapOracleTx).
func (t oracleTx) Handle() persistence.TxHandle { return newOracleTxHandle(t.tx) }

func (t oracleTx) Dialect() core.Dialect { return oracleDialect{} }

// The lifecycle-hook dispatch (positions A/D) + the observability hook-error
// line live once on the embedded write.BaseEngine
// (FireAfterBegin/FireBeforeCommit), reached through the neutral core.WriteTx
// (oracleTx) — this engine carries no copy of its own.
