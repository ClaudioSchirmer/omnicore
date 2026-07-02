//go:build mysql

package mysql

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// mysqlTxHandle is the MySQL implementation of persistence.TxHandle handed to
// in-TX lifecycle hooks. It embeds persistence.SealedTxHandle to satisfy the
// sealed marker (the same mechanism the Postgres pgxTxHandle uses) and carries
// the live *sql.Tx privately, recovered by an in-TX side-effect adapter via
// UnwrapMySQLTx.
type mysqlTxHandle struct {
	persistence.SealedTxHandle
	tx *sql.Tx
}

func newMySQLTxHandle(tx *sql.Tx) persistence.TxHandle { return &mysqlTxHandle{tx: tx} }

// UnwrapMySQLTx recovers the *sql.Tx from a persistence.TxHandle. It is the
// MySQL counterpart of mongo.UnwrapPgxTx: an in-TX side-effect adapter that
// deliberately targets MySQL calls it to run its own SQL inside the framework's
// TX. Using it pins that adapter to MySQL. Panics on a foreign handle.
func UnwrapMySQLTx(tx persistence.TxHandle) *sql.Tx {
	h, ok := tx.(*mysqlTxHandle)
	if !ok {
		panic(fmt.Sprintf("mysql.UnwrapMySQLTx: foreign persistence.TxHandle %T; only the MySQL engine's handle is supported", tx))
	}
	return h.tx
}

// NeutralTx exposes the live *sql.Tx through the backend-neutral core.Tx surface
// — the MySQL side of the canonical core.UnwrapTx bridge. The exported method
// satisfies infra's neutralTxCarrier across the package boundary.
func (h *mysqlTxHandle) NeutralTx() core.Tx { return mysqlTx{tx: h.tx} }

// mysqlTx adapts a live *sql.Tx to core.WriteTx. *sql.Rows / *sql.Row satisfy
// core.Rows / core.Row directly; ExecCount reads sql.Result.RowsAffected;
// Commit/Rollback drive the lifecycle (the *sql.Tx methods take no ctx);
// Handle seals the tx for lifecycle hooks; Dialect is the MySQL flavor.
type mysqlTx struct{ tx *sql.Tx }

// Compile-time proof mysqlTx satisfies the neutral write surface.
var _ core.WriteTx = mysqlTx{}

func (t mysqlTx) Exec(ctx context.Context, sqlText string, args ...any) error {
	_, err := t.tx.ExecContext(ctx, sqlText, args...)
	return err
}

func (t mysqlTx) ExecCount(ctx context.Context, sqlText string, args ...any) (int64, error) {
	res, err := t.tx.ExecContext(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (t mysqlTx) Query(ctx context.Context, sqlText string, args ...any) (core.Rows, error) {
	return t.tx.QueryContext(ctx, sqlText, args...)
}

func (t mysqlTx) QueryRow(ctx context.Context, sqlText string, args ...any) core.Row {
	return t.tx.QueryRowContext(ctx, sqlText, args...)
}

func (t mysqlTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t mysqlTx) Rollback(context.Context) error { return t.tx.Rollback() }

// Handle seals the live tx as a persistence.TxHandle — the same handle the
// lifecycle hooks receive (and recover via core.UnwrapTx / UnwrapMySQLTx).
func (t mysqlTx) Handle() persistence.TxHandle { return newMySQLTxHandle(t.tx) }

func (t mysqlTx) Dialect() core.Dialect { return mysqlDialect{} }

// The lifecycle-hook dispatch (positions A/D) + the observability hook-error
// line live once on the embedded write.BaseEngine (FireAfterBegin/FireBeforeCommit),
// reached through the neutral core.WriteTx (mysqlTx) — so the MySQL engine no
// longer carries its own copy.
