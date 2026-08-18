package audit

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// ─── neutral read fakes (mirror the engine's db.Rows seam, no driver dep) ─────

// fakeQueryer implements the audit package's Queryer interface, replaying
// scripted rows so FindByID / FindByAggregate exercise their full scan path
// without a live database — the read twin of the persister tests' fake Execer.
type fakeQueryer struct {
	queryErr error
	rows     *fakeRows

	lastSQL  string
	lastArgs []any
}

func (f *fakeQueryer) Query(_ context.Context, sql string, args ...any) (Rows, error) {
	f.lastSQL = sql
	f.lastArgs = args
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.rows == nil {
		f.rows = &fakeRows{}
	}
	return f.rows, nil
}

type fakeRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error
}

func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Err() error   { return r.errAfter }

func (r *fakeRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	return scanReflect(dest, r.data[r.idx-1])
}

// testReader builds a reader over the fake with a Postgres-style placeholder
// renderer and row cap — both shapes are irrelevant to the fake (it records
// the SQL verbatim), so any deterministic renderer works.
func testReader(q Queryer) appaudit.Reader {
	return NewReader(q,
		func(n int) string { return fmt.Sprintf("$%d", n) },
		func(v any) any { return v },
		func(sql string, n int) string { return sql + fmt.Sprintf(" LIMIT %d", n) })
}

// scanReflect lands each scripted column value into the matching scan target
// pointer, mirroring how a driver populates dest pointers — including the
// **string targets the nullable actor/issuer/tenant columns scan into.
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
// nullable actor/issuer/tenant/trace columns carrying *string pointers (the
// scan targets these into **string).
func happyRow(actor, issuer, tenant *string, payload []byte) []any {
	return happyRowWithTrace(actor, issuer, tenant, nil, payload)
}

// happyRowWithTrace is happyRow with the trace_id column spelled out, for the
// tests that assert the pivot column actually reaches the AuditEvent.
func happyRowWithTrace(actor, issuer, tenant, trace *string, payload []byte) []any {
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
		trace,            // trace_id (*string)
		time.Now().UTC(), // occurred_at
		payload,          // payload
	}
}

// ─── FindByID ────────────────────────────────────────────────────────────────

func TestFindByID_Hit(t *testing.T) {
	actor := "user-42"
	q := &fakeQueryer{rows: &fakeRows{data: [][]any{
		happyRow(&actor, nil, nil, []byte(`{"snapshot":{"name":"alice"}}`)),
	}}}
	ev, err := testReader(q).FindByID(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if ev.Actor != "user-42" || ev.EntityType != "User" {
		t.Errorf("scanned event drifted: %+v", ev)
	}
	if ev.Snapshot == nil || ev.Snapshot["name"] != "alice" {
		t.Errorf("payload not decoded: %v", ev.Snapshot)
	}
	if len(q.lastArgs) != 1 {
		t.Errorf("FindByID must bind exactly the id arg, got %v", q.lastArgs)
	}
	// The id binds through the dialect's value codec as a domain.ID (identity
	// encode in this test) — native uuid form on every engine.
	if _, ok := q.lastArgs[0].(domain.ID); !ok {
		t.Errorf("FindByID must bind the id through the value codec (domain.ID), got %T", q.lastArgs[0])
	}
}

func TestFindByID_MissMapsToSentinel(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{}} // no rows
	_, err := testReader(q).FindByID(context.Background(), uuid.New())
	if !errors.Is(err, appaudit.ErrAuditNotFound) {
		t.Errorf("empty result must map to appaudit.ErrAuditNotFound, got %v", err)
	}
}

func TestFindByID_QueryErrorWrapped(t *testing.T) {
	q := &fakeQueryer{queryErr: errors.New("conn reset")}
	_, err := testReader(q).FindByID(context.Background(), uuid.New())
	if err == nil || errors.Is(err, appaudit.ErrAuditNotFound) {
		t.Errorf("transport failure must surface as a wrapped error, got %v", err)
	}
}

func TestFindByID_ScanErrorWrapped(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{
		data:    [][]any{happyRow(nil, nil, nil, []byte(`{}`))},
		scanErr: errors.New("bad scan"),
	}}
	_, err := testReader(q).FindByID(context.Background(), uuid.New())
	if err == nil || errors.Is(err, appaudit.ErrAuditNotFound) {
		t.Errorf("scan failure must surface as a wrapped error, got %v", err)
	}
}

// A no-rows cursor that ALSO reports a late Err() is a transport failure, not a
// clean miss — it must not collapse into the not-found sentinel.
func TestFindByID_EmptyWithRowsErrIsTransport(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{errAfter: errors.New("late rows err")}}
	_, err := testReader(q).FindByID(context.Background(), uuid.New())
	if err == nil || errors.Is(err, appaudit.ErrAuditNotFound) {
		t.Errorf("rows.Err() on an empty cursor must surface, not map to not-found, got %v", err)
	}
}

// ─── FindByAggregate ─────────────────────────────────────────────────────────

func TestFindByAggregate_ReturnsRowsNewestFirst(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{data: [][]any{
		happyRow(nil, nil, nil, []byte(`{}`)),
		happyRow(nil, nil, nil, []byte(`{}`)),
	}}}
	out, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 rows, got %d", len(out))
	}
	if len(q.lastArgs) != 2 {
		t.Errorf("FindByAggregate must bind entityType + aggregateID, got %v", q.lastArgs)
	}
}

// trace_id is the pivot from an audit row to the trace of the request that
// produced it. The persister writes it; a read that dropped the column would
// hand every consumer an empty TraceID and quietly break that jump.
func TestFindByAggregate_CarriesTraceID(t *testing.T) {
	trace := "4bf92f3577b34da6a3ce929d0e0e4736"
	q := &fakeQueryer{rows: &fakeRows{data: [][]any{
		happyRowWithTrace(nil, nil, nil, &trace, []byte(`{}`)),
	}}}
	out, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if len(out) != 1 || out[0].TraceID != trace {
		t.Errorf("TraceID lost on the read path: %+v", out)
	}
}

// The cap must reach the STATEMENT, not the materialized slice: a long
// timeline must never cross the wire only to be trimmed in Go.
func TestFindByAggregate_RendersTheCapIntoTheStatement(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{}}
	if _, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 7); err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if !strings.Contains(q.lastSQL, "LIMIT 7") {
		t.Errorf("the dialect's cap is missing from the statement: %q", q.lastSQL)
	}
	// Ordering has to precede the cap, or the engine caps an unordered set and
	// "newest first" stops being newest.
	if strings.Index(q.lastSQL, "ORDER BY") > strings.Index(q.lastSQL, "LIMIT 7") {
		t.Errorf("the cap must come after ORDER BY, got %q", q.lastSQL)
	}
}

// The dialect renderer is the only thing that knows WHERE the cap goes, so a
// reader built without one still answers rather than emitting invalid SQL —
// the hand-wired NewReader path.
func TestFindByAggregate_NilApplyLimitLeavesStatementUncapped(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{}}
	r := NewReader(q, func(n int) string { return fmt.Sprintf("$%d", n) }, func(v any) any { return v }, nil)
	if _, err := r.FindByAggregate(context.Background(), "User", uuid.NewString(), 7); err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if strings.Contains(q.lastSQL, "LIMIT") {
		t.Errorf("no renderer supplied, so no cap can be rendered; got %q", q.lastSQL)
	}
}

func TestFindByAggregate_EmptyResultIsNonNilSlice(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{}}
	out, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err != nil {
		t.Fatalf("FindByAggregate: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("empty aggregate must return empty (non-nil) slice, got %v", out)
	}
}

func TestFindByAggregate_QueryErrorWrapped(t *testing.T) {
	q := &fakeQueryer{queryErr: errors.New("query boom")}
	_, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err == nil {
		t.Fatal("expected query error to surface")
	}
}

func TestFindByAggregate_ScanErrorWrapped(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{
		data:    [][]any{happyRow(nil, nil, nil, []byte(`{}`))},
		scanErr: errors.New("bad scan"),
	}}
	_, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err == nil {
		t.Fatal("expected scan error to surface")
	}
}

func TestFindByAggregate_RowsErrWrapped(t *testing.T) {
	q := &fakeQueryer{rows: &fakeRows{
		data:     [][]any{happyRow(nil, nil, nil, []byte(`{}`))},
		errAfter: errors.New("late rows err"),
	}}
	_, err := testReader(q).FindByAggregate(context.Background(), "User", uuid.NewString(), 20)
	if err == nil {
		t.Fatal("expected rows.Err() to surface")
	}
}
