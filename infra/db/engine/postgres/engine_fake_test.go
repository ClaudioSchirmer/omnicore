//go:build postgres

package postgres

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file provides a hand-rolled, in-process fake of the unexported
// pgxPool seam (postgres.go) plus the pgx.Tx / pgx.Row / pgx.Rows surfaces
// the transactional core touches. It lets the write API (Insert/Update/
// Archive/Unarchive/Delete and their aggregate variants) and the read
// helpers (loader/composer SELECTs) run without a live database. The
// integration suite (//go:build integration) remains the source of truth
// for real SQL behavior; these fakes only verify the Go control flow,
// outbox/audit sequencing, and error propagation around the driver calls.

// fakePool implements the pgxPool interface. recordedExec/recordedQuery
// capture the SQL the persister emits; begin / commit / rollback errors are
// injectable to exercise the failure branches. Each Begin hands back the
// same *fakeTx so a test can inspect what the transaction saw.
type fakePool struct {
	tx           *fakeTx
	beginErr     error
	queryHandler func(sql string, args []any) (pgx.Rows, error)
	execHandler  func(sql string, args []any) (pgconn.CommandTag, error)
	queryRowFn   func(sql string, args []any) pgx.Row
}

func newFakePool() *fakePool {
	return &fakePool{tx: newFakeTx()}
}

func (p *fakePool) Begin(ctx context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	if p.tx == nil {
		p.tx = newFakeTx()
	}
	return p.tx, nil
}

func (p *fakePool) Close() {}

func (p *fakePool) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if p.execHandler != nil {
		return p.execHandler(sql, args)
	}
	return pgconn.NewCommandTag("OK 1"), nil
}

func (p *fakePool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if p.queryHandler != nil {
		return p.queryHandler(sql, args)
	}
	return &fakeRows{}, nil
}

func (p *fakePool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if p.queryRowFn != nil {
		return p.queryRowFn(sql, args)
	}
	return &fakeRow{}
}

// newFakePostgres builds a Postgres whose pool is the supplied fake. The
// field is unexported but reachable here because the test file is in
// package infra.
func newFakePostgres(pool pgxPool) *Postgres {
	p := &Postgres{pool: pool}
	p.SetBeginner(p) // wire the embedded BaseEngine's write-TX beginner, as NewPostgres does
	return p
}

// fakeTx embeds pgx.Tx so it satisfies the full interface; only the methods
// the core calls are overridden. Anything unexpected (e.g. CopyFrom) keeps
// the embedded nil and would panic, surfacing an unmodelled call site.
type fakeTx struct {
	pgx.Tx

	scanID        string                                         // value handed back by QueryRow().Scan(&id)
	queryRowErr   error                                          // forced error from QueryRow().Scan
	execErr       error                                          // forced error from Exec (every call)
	execErrSubstr string                                         // forced error from Exec only when the SQL contains this substring
	commitErr     error                                          // forced error from Commit
	rollbackErr   error                                          // forced error from Rollback
	queryFn       func(sql string, args []any) (pgx.Rows, error) // overrides Query when set
	queryRowFn    func(sql string, args []any) pgx.Row           // overrides QueryRow when set
	execCalls     []string                                       // captured Exec SQL (outbox, audit, child writes)
	execArgs      [][]any                                        // captured Exec args, parallel to execCalls
	committed     bool
	rolledBack    bool
}

func newFakeTx() *fakeTx { return &fakeTx{scanID: "00000000-0000-0000-0000-000000000001"} }

func (t *fakeTx) Begin(ctx context.Context) (pgx.Tx, error) { return t, nil }

func (t *fakeTx) Commit(ctx context.Context) error {
	t.committed = true
	return t.commitErr
}

func (t *fakeTx) Rollback(ctx context.Context) error {
	t.rolledBack = true
	return t.rollbackErr
}

func (t *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execCalls = append(t.execCalls, sql)
	t.execArgs = append(t.execArgs, args)
	if t.execErr != nil {
		return pgconn.CommandTag{}, t.execErr
	}
	if t.execErrSubstr != "" && strings.Contains(sql, t.execErrSubstr) {
		return pgconn.CommandTag{}, errFake
	}
	return pgconn.NewCommandTag("OK 1"), nil
}

func (t *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.queryFn != nil {
		return t.queryFn(sql, args)
	}
	return &fakeRows{}, nil
}

func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.queryRowFn != nil {
		return t.queryRowFn(sql, args)
	}
	return &fakeRow{id: t.scanID, err: t.queryRowErr}
}

// fakeRow scans a single id string (the RETURNING id of an INSERT/UPDATE).
type fakeRow struct {
	id  string
	err error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if p, ok := d.(*string); ok {
			*p = r.id
			return nil
		}
	}
	return nil
}

// fakeRows is an empty, programmable result set. rows drives Next/Scan; when
// scan is set it is invoked per row to populate the destinations.
type fakeRows struct {
	pgx.Rows

	rows    int
	pos     int
	scan    func(idx int, dest []any) error
	nextErr error
}

func (r *fakeRows) Next() bool {
	if r.pos >= r.rows {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scan != nil {
		return r.scan(r.pos-1, dest)
	}
	return nil
}

func (r *fakeRows) Close()     {}
func (r *fakeRows) Err() error { return r.nextErr }

var errFake = errors.New("fake pg error")
