package infra

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// pgxTxHandle adapts pgx.Tx to the application-layer persistence.TxHandle
// interface. Exposed only via newPgxTxHandle so application code never
// reaches the pgx symbol through the persistence package.
//
// The wrapper is stateless beyond the embedded tx; calling Exec / Query /
// QueryRow forwards verbatim. Returned errors are raw pgx errors per the
// TxHandle contract (no infra.ConstraintBinding translation — that
// surface lives at the Repository level).
type pgxTxHandle struct {
	tx pgx.Tx
}

// newPgxTxHandle is the only constructor for the TxHandle adapter. The
// persister builds one per lifecycle-hook call and discards it when the
// TX ends (Commit OR Rollback).
func newPgxTxHandle(tx pgx.Tx) persistence.TxHandle {
	return &pgxTxHandle{tx: tx}
}

// Exec forwards to pgx.Tx.Exec. The returned CommandTag carries only
// RowsAffected — sufficient for in-TX hook accounting (e.g. "did the
// extra outbox row actually land?") without dragging the rest of the
// pgconn surface into application/.
func (h *pgxTxHandle) Exec(ctx context.Context, sql string, args ...any) (persistence.CommandTag, error) {
	ct, err := h.tx.Exec(ctx, sql, args...)
	if err != nil {
		return persistence.CommandTag{}, err
	}
	return persistence.CommandTag{RowsAffected: ct.RowsAffected()}, nil
}

// Query forwards to pgx.Tx.Query and wraps the returned rows so the
// caller iterates against the persistence.Rows surface. Consumers own
// the iterator: defer rows.Close() is the convention.
func (h *pgxTxHandle) Query(ctx context.Context, sql string, args ...any) (persistence.Rows, error) {
	rows, err := h.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

// QueryRow forwards to pgx.Tx.QueryRow. pgx delays the error until
// Scan, so the wrapper mirrors the same shape — returning a Row whose
// Scan call surfaces the error.
func (h *pgxTxHandle) QueryRow(ctx context.Context, sql string, args ...any) persistence.Row {
	return &pgxRow{row: h.tx.QueryRow(ctx, sql, args...)}
}

// pgxRows adapts pgx.Rows to persistence.Rows. The Close() method is
// non-error per the persistence contract — pgx.Rows.Close() returns
// void already, so the adaptation is trivial.
type pgxRows struct {
	rows pgx.Rows
}

func (r *pgxRows) Next() bool                 { return r.rows.Next() }
func (r *pgxRows) Scan(dest ...any) error     { return r.rows.Scan(dest...) }
func (r *pgxRows) Err() error                 { return r.rows.Err() }
func (r *pgxRows) Close()                     { r.rows.Close() }

// pgxRow adapts pgx.Row to persistence.Row. The interface carries only
// Scan because that is the only method pgx.Row itself exposes.
type pgxRow struct {
	row pgx.Row
}

func (r *pgxRow) Scan(dest ...any) error { return r.row.Scan(dest...) }
