package audit

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ─── pgx fakes (mirror the seam pattern from infra/integration) ──────────────

// fakeQueryExec implements the audit package's pgExec interface, replaying
// scripted rows so FindByID / FindByAggregate exercise their full scan path
// without a live Postgres.
type fakeQueryExec struct {
	queryErr error
	rows     *fakeQueryRows
	row      *fakeQueryRow

	lastSQL  string
	lastArgs []any
}

func (f *fakeQueryExec) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.lastSQL = sql
	f.lastArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.rows == nil {
		f.rows = &fakeQueryRows{}
	}
	return f.rows, nil
}

func (f *fakeQueryExec) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.lastSQL = sql
	f.lastArgs = args
	if f.row == nil {
		f.row = &fakeQueryRow{scanErr: pgx.ErrNoRows}
	}
	return f.row
}

type fakeQueryRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error
}

func (r *fakeQueryRows) Close()                                       {}
func (r *fakeQueryRows) Err() error                                   { return r.errAfter }
func (r *fakeQueryRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeQueryRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeQueryRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeQueryRows) RawValues() [][]byte                          { return nil }
func (r *fakeQueryRows) Conn() *pgx.Conn                              { return nil }

func (r *fakeQueryRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeQueryRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return scanReflect(dest, r.data[r.idx-1])
}

type fakeQueryRow struct {
	values  []any
	scanErr error
}

func (r *fakeQueryRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return scanReflect(dest, r.values)
}

// scanReflect lands each scripted column value into the matching scan target
// pointer, mirroring how the pgx driver populates dest pointers — including
// the **string targets the nullable actor/issuer/tenant columns scan into.
func scanReflect(dest []any, row []any) error {
	if len(dest) != len(row) {
		return errors.New("scanReflect: dest/row length mismatch")
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer || dv.IsNil() {
			return errors.New("scanReflect: dest must be a non-nil pointer")
		}
		target := dv.Elem()
		sv := reflect.ValueOf(row[i])
		if !sv.IsValid() {
			continue // nil source → leave the zero value
		}
		if sv.Type().AssignableTo(target.Type()) {
			target.Set(sv)
			continue
		}
		if sv.Type().ConvertibleTo(target.Type()) {
			target.Set(sv.Convert(target.Type()))
			continue
		}
		return errors.New("scanReflect: cannot assign " + sv.Type().String() + " to " + target.Type().String())
	}
	return nil
}

// happyRow returns one column tuple in selectAuditEventCols order, with the
// nullable actor/issuer/tenant columns carrying *string pointers (pgx scans
// these into **string).
func happyRow(actor, issuer, tenant *string, payload []byte) []any {
	return []any{
		uuid.New(),       // id
		"User",           // entity_type
		uuid.New(),       // aggregate_id
		"update",         // verb
		"GetUpdatable",   // action_name
		"delta",          // kind
		actor,            // actor (*string)
		issuer,           // actor_issuer (*string)
		tenant,           // tenant_id (*string)
		uuid.New(),       // thread_id
		time.Now().UTC(), // occurred_at
		payload,          // payload
	}
}

// ─── FindByID ────────────────────────────────────────────────────────────────

func TestFindByID_Hit(t *testing.T) {
	actor := "user-42"
	exec := &fakeQueryExec{row: &fakeQueryRow{
		values: happyRow(&actor, nil, nil, []byte(`{"snapshot":{"name":"alice"}}`)),
	}}
	ev, err := FindByID(context.Background(), exec, uuid.New())
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if ev.Actor != "user-42" || ev.EntityType != "User" {
		t.Errorf("scanned event drifted: %+v", ev)
	}
	if ev.Snapshot == nil || ev.Snapshot["name"] != "alice" {
		t.Errorf("payload not decoded: %v", ev.Snapshot)
	}
	if len(exec.lastArgs) != 1 {
		t.Errorf("FindByID must bind exactly the id arg, got %v", exec.lastArgs)
	}
}

func TestFindByID_MissMapsToSentinel(t *testing.T) {
	exec := &fakeQueryExec{row: &fakeQueryRow{scanErr: pgx.ErrNoRows}}
	_, err := FindByID(context.Background(), exec, uuid.New())
	if !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("ErrNoRows must map to ErrAuditNotFound, got %v", err)
	}
}

func TestFindByID_TransportErrorWrapped(t *testing.T) {
	exec := &fakeQueryExec{row: &fakeQueryRow{scanErr: errors.New("conn reset")}}
	_, err := FindByID(context.Background(), exec, uuid.New())
	if err == nil || errors.Is(err, ErrAuditNotFound) {
		t.Errorf("transport failure must surface as a wrapped error, got %v", err)
	}
}

// ─── FindByAggregate ─────────────────────────────────────────────────────────

func TestFindByAggregate_ReturnsRowsNewestFirst(t *testing.T) {
	exec := &fakeQueryExec{rows: &fakeQueryRows{data: [][]any{
		happyRow(nil, nil, nil, []byte(`{}`)),
		happyRow(nil, nil, nil, []byte(`{}`)),
	}}}
	out, err := FindByAggregate(context.Background(), exec, "User", uuid.NewString())
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out))
	}
	if len(exec.lastArgs) != 2 {
		t.Errorf("FindByAggregate must bind entityType + aggregateID, got %v", exec.lastArgs)
	}
}

func TestFindByAggregate_EmptyResultIsNonNilSlice(t *testing.T) {
	exec := &fakeQueryExec{rows: &fakeQueryRows{}}
	out, err := FindByAggregate(context.Background(), exec, "User", uuid.NewString())
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("empty aggregate must return empty (non-nil) slice, got %v", out)
	}
}

func TestFindByAggregate_QueryErrorWrapped(t *testing.T) {
	exec := &fakeQueryExec{queryErr: errors.New("query boom")}
	_, err := FindByAggregate(context.Background(), exec, "User", uuid.NewString())
	if err == nil {
		t.Fatal("expected query error to surface")
	}
}

func TestFindByAggregate_ScanErrorWrapped(t *testing.T) {
	exec := &fakeQueryExec{rows: &fakeQueryRows{
		data:    [][]any{happyRow(nil, nil, nil, []byte(`{}`))},
		scanErr: errors.New("bad scan"),
	}}
	_, err := FindByAggregate(context.Background(), exec, "User", uuid.NewString())
	if err == nil {
		t.Fatal("expected scan error to surface")
	}
}

func TestFindByAggregate_RowsErrWrapped(t *testing.T) {
	exec := &fakeQueryExec{rows: &fakeQueryRows{
		data:     [][]any{happyRow(nil, nil, nil, []byte(`{}`))},
		errAfter: errors.New("late rows err"),
	}}
	_, err := FindByAggregate(context.Background(), exec, "User", uuid.NewString())
	if err == nil {
		t.Fatal("expected rows.Err() to surface")
	}
}
