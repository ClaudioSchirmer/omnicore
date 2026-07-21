package read

import (
	"context"
	"errors"
)

var errFakeDB = errors.New("fake db error")

// fakeQuerier is a scriptable db.Querier for loader/repository white-box tests.
// queryFn drives the SELECT path; the other verbs default to no-ops since the
// covered paths never reach them.
type fakeQuerier struct {
	queryFn func(sql string, args []any) (Rows, error)
	mapsFn  func(sql string, args []any) ([]map[string]any, error)
}

func (q fakeQuerier) Query(_ context.Context, sql string, args ...any) (Rows, error) {
	if q.queryFn != nil {
		return q.queryFn(sql, args)
	}
	return &fakeDBRows{}, nil
}
func (fakeQuerier) QueryRow(context.Context, string, ...any) Row { return fakeDBRow{} }
func (fakeQuerier) Exec(context.Context, string, ...any) error   { return nil }
func (q fakeQuerier) QueryMaps(_ context.Context, sql string, args ...any) ([]map[string]any, error) {
	if q.mapsFn != nil {
		return q.mapsFn(sql, args)
	}
	return nil, nil
}

// fakeDBRows is a programmable db.Rows. rows drives Next; scan (when set)
// populates the destinations per row; nextErr is returned by Err().
type fakeDBRows struct {
	rows    int
	pos     int
	scan    func(idx int, dest []any) error
	nextErr error
}

func (r *fakeDBRows) Next() bool {
	if r.pos >= r.rows {
		return false
	}
	r.pos++
	return true
}
func (r *fakeDBRows) Scan(dest ...any) error {
	if r.scan != nil {
		return r.scan(r.pos-1, dest)
	}
	return nil
}
func (r *fakeDBRows) Err() error   { return r.nextErr }
func (r *fakeDBRows) Close() error { return nil }

type fakeDBRow struct {
	id  string
	err error
}

func (r fakeDBRow) Scan(dest ...any) error {
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
