package integration

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- in-process fakes implementing the existing pgExec / pgx seams ---------

// fakeExec implements pgExec (the minimal interface failures.go declares).
// It records the SQL + args each call received and replays scripted results,
// so the failure/processed helpers are exercised without a live Postgres.
type fakeExec struct {
	execErr  error
	queryErr error

	rows *fakeRows // returned by Query
	row  *fakeRow  // returned by QueryRow

	lastSQL  string
	lastArgs []any
	calls    int
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.lastArgs = args
	return pgconn.CommandTag{}, f.execErr
}

func (f *fakeExec) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
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

func (f *fakeExec) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	f.lastSQL = sql
	f.lastArgs = args
	if f.row == nil {
		f.row = &fakeRow{scanErr: pgx.ErrNoRows}
	}
	return f.row
}

// fakeRows is a minimal pgx.Rows over a slice of positional row values.
type fakeRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error // returned by Err() after iteration
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.errAfter }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

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
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("fakeRows.Scan: dest len %d != row len %d", len(dest), len(row))
	}
	for i, d := range dest {
		if err := assign(d, row[i]); err != nil {
			return fmt.Errorf("col %d: %w", i, err)
		}
	}
	return nil
}

// fakeRow is a minimal pgx.Row for QueryRow-based helpers.
type fakeRow struct {
	values  []any
	scanErr error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		if err := assign(d, r.values[i]); err != nil {
			return err
		}
	}
	return nil
}

// assign sets *dst = src with convertibility tolerance, mirroring how the
// pgx driver would land a column value into a typed destination pointer.
func assign(dst any, src any) error {
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer, got %T", dst)
	}
	target := dv.Elem()
	sv := reflect.ValueOf(src)
	if !sv.IsValid() {
		return nil // nil source → leave zero value
	}
	if sv.Type().AssignableTo(target.Type()) {
		target.Set(sv)
		return nil
	}
	if sv.Type().ConvertibleTo(target.Type()) {
		target.Set(sv.Convert(target.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign %T to %s", src, target.Type())
}

func sampleFailureRow(id int64) []any {
	return []any{
		id,                // ID int64
		"orders-int",      // ConsumerGroup string
		"partners",        // SourceKey string
		"onboarded",       // EventKey string
		uuid.New(),        // EventID uuid.UUID
		[]byte(`{"a":1}`), // RawPayload []byte
		"boom",            // Error string
		3,                 // Attempt int
		time.Now(),        // FirstSeenAt time.Time
		time.Now(),        // LastAttemptAt time.Time
	}
}

// --- RecordIntegrationFailure ----------------------------------------------

func TestRecordIntegrationFailure_Validation(t *testing.T) {
	exec := &fakeExec{}
	base := IntegrationFailureRecord{
		ConsumerGroup: "g", SourceKey: "s", EventKey: "e", EventID: uuid.New(),
	}
	bad := []IntegrationFailureRecord{
		{SourceKey: "s", EventKey: "e", EventID: uuid.New()},      // missing group
		{ConsumerGroup: "g", EventKey: "e", EventID: uuid.New()},  // missing source
		{ConsumerGroup: "g", SourceKey: "s", EventID: uuid.New()}, // missing event key
		{ConsumerGroup: "g", SourceKey: "s", EventKey: "e"},       // nil event id
	}
	for i, rec := range bad {
		if err := RecordIntegrationFailure(context.Background(), exec, rec); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if exec.calls != 0 {
		t.Fatalf("validation failures must not reach exec, got %d calls", exec.calls)
	}
	_ = base
}

func TestRecordIntegrationFailure_DefaultsPayloadAndSucceeds(t *testing.T) {
	exec := &fakeExec{}
	rec := IntegrationFailureRecord{
		ConsumerGroup: "g", SourceKey: "s", EventKey: "e", EventID: uuid.New(),
		// RawPayload nil → helper must substitute "{}".
	}
	if err := RecordIntegrationFailure(context.Background(), exec, rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.calls != 1 {
		t.Fatalf("expected 1 exec call, got %d", exec.calls)
	}
	payload, ok := exec.lastArgs[4].([]byte)
	if !ok || string(payload) != "{}" {
		t.Fatalf("nil payload must default to {}, got %v", exec.lastArgs[4])
	}
}

func TestRecordIntegrationFailure_ExecError(t *testing.T) {
	exec := &fakeExec{execErr: errors.New("db down")}
	rec := IntegrationFailureRecord{
		ConsumerGroup: "g", SourceKey: "s", EventKey: "e", EventID: uuid.New(),
		RawPayload: []byte(`{"x":1}`),
	}
	err := RecordIntegrationFailure(context.Background(), exec, rec)
	if err == nil || !errors.Is(err, exec.execErr) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
}

// --- ResolveIntegrationFailures --------------------------------------------

func TestResolveIntegrationFailures(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		exec := &fakeExec{}
		if err := ResolveIntegrationFailures(context.Background(), exec, "", "s", "e", uuid.New()); err == nil {
			t.Error("expected error for empty consumer group")
		}
		if err := ResolveIntegrationFailures(context.Background(), exec, "g", "s", "e", uuid.Nil); err == nil {
			t.Error("expected error for nil event id")
		}
		if exec.calls != 0 {
			t.Errorf("validation must short-circuit, got %d calls", exec.calls)
		}
	})
	t.Run("success", func(t *testing.T) {
		exec := &fakeExec{}
		if err := ResolveIntegrationFailures(context.Background(), exec, "g", "s", "e", uuid.New()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("exec-error", func(t *testing.T) {
		exec := &fakeExec{execErr: errors.New("nope")}
		if err := ResolveIntegrationFailures(context.Background(), exec, "g", "s", "e", uuid.New()); err == nil {
			t.Fatal("expected exec error")
		}
	})
}

// --- List / scan -----------------------------------------------------------

func TestListPendingIntegrationFailures_ScansRows(t *testing.T) {
	exec := &fakeExec{rows: &fakeRows{data: [][]any{sampleFailureRow(1), sampleFailureRow(2)}}}
	out, err := ListPendingIntegrationFailures(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].ID != 1 || out[1].ID != 2 {
		t.Fatalf("row ids drifted: %+v", out)
	}
	if out[0].ConsumerGroup != "orders-int" || out[0].Error != "boom" || out[0].Attempt != 3 {
		t.Fatalf("scanned fields drifted: %+v", out[0])
	}
}

func TestListPendingIntegrationFailures_QueryError(t *testing.T) {
	exec := &fakeExec{queryErr: errors.New("query boom")}
	if _, err := ListPendingIntegrationFailures(context.Background(), exec); err == nil {
		t.Fatal("expected query error")
	}
}

func TestScanIntegrationFailures_ScanError(t *testing.T) {
	exec := &fakeExec{rows: &fakeRows{
		data:    [][]any{sampleFailureRow(1)},
		scanErr: errors.New("bad scan"),
	}}
	if _, err := ListPendingIntegrationFailures(context.Background(), exec); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestScanIntegrationFailures_RowsErr(t *testing.T) {
	exec := &fakeExec{rows: &fakeRows{
		data:     [][]any{sampleFailureRow(1)},
		errAfter: errors.New("late rows err"),
	}}
	if _, err := ListPendingIntegrationFailures(context.Background(), exec); err == nil {
		t.Fatal("expected rows.Err() to surface")
	}
}

func TestListPendingIntegrationFailuresByGroup(t *testing.T) {
	t.Run("requires-group", func(t *testing.T) {
		exec := &fakeExec{}
		if _, err := ListPendingIntegrationFailuresByGroup(context.Background(), exec, ""); err == nil {
			t.Fatal("expected error for empty consumer group")
		}
	})
	t.Run("passes-group-arg-and-scans", func(t *testing.T) {
		exec := &fakeExec{rows: &fakeRows{data: [][]any{sampleFailureRow(7)}}}
		out, err := ListPendingIntegrationFailuresByGroup(context.Background(), exec, "orders-int")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 1 || out[0].ID != 7 {
			t.Fatalf("unexpected rows: %+v", out)
		}
		if len(exec.lastArgs) != 1 || exec.lastArgs[0] != "orders-int" {
			t.Fatalf("consumer group must be bound as the only arg, got %v", exec.lastArgs)
		}
	})
}

// --- IsAlreadyProcessed -----------------------------------------------------

func TestIsAlreadyProcessed(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		exec := &fakeExec{}
		if _, err := IsAlreadyProcessed(context.Background(), exec, uuid.Nil, "g"); err == nil {
			t.Error("expected error for nil event id")
		}
		if _, err := IsAlreadyProcessed(context.Background(), exec, uuid.New(), ""); err == nil {
			t.Error("expected error for empty consumer group")
		}
	})
	t.Run("found", func(t *testing.T) {
		exec := &fakeExec{row: &fakeRow{values: []any{1}}}
		got, err := IsAlreadyProcessed(context.Background(), exec, uuid.New(), "g")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Fatal("expected true when a dedup row exists")
		}
	})
	t.Run("not-found", func(t *testing.T) {
		exec := &fakeExec{row: &fakeRow{scanErr: pgx.ErrNoRows}}
		got, err := IsAlreadyProcessed(context.Background(), exec, uuid.New(), "g")
		if err != nil {
			t.Fatalf("ErrNoRows must map to (false, nil), got err %v", err)
		}
		if got {
			t.Fatal("expected false when no dedup row exists")
		}
	})
	t.Run("scan-error", func(t *testing.T) {
		exec := &fakeExec{row: &fakeRow{scanErr: errors.New("conn reset")}}
		if _, err := IsAlreadyProcessed(context.Background(), exec, uuid.New(), "g"); err == nil {
			t.Fatal("expected real scan error to surface")
		}
	})
}

// --- MarkProcessed ----------------------------------------------------------

func TestMarkProcessed(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		exec := &fakeExec{}
		if err := MarkProcessed(context.Background(), exec, IntegrationProcessedRecord{ConsumerGroup: "g"}); err == nil {
			t.Error("expected error for nil event id")
		}
		if err := MarkProcessed(context.Background(), exec, IntegrationProcessedRecord{EventID: uuid.New()}); err == nil {
			t.Error("expected error for empty consumer group")
		}
		if exec.calls != 0 {
			t.Errorf("validation must short-circuit, got %d calls", exec.calls)
		}
	})
	t.Run("success", func(t *testing.T) {
		exec := &fakeExec{}
		rec := IntegrationProcessedRecord{
			EventID: uuid.New(), ConsumerGroup: "g", SourceKey: "s",
			EventKey: "e", Topic: "t", EventType: "T",
		}
		if err := MarkProcessed(context.Background(), exec, rec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exec.calls != 1 || len(exec.lastArgs) != 6 {
			t.Fatalf("expected 1 exec with 6 args, got calls=%d args=%d", exec.calls, len(exec.lastArgs))
		}
	})
	t.Run("exec-error", func(t *testing.T) {
		exec := &fakeExec{execErr: errors.New("insert failed")}
		rec := IntegrationProcessedRecord{EventID: uuid.New(), ConsumerGroup: "g"}
		if err := MarkProcessed(context.Background(), exec, rec); err == nil {
			t.Fatal("expected exec error")
		}
	})
}
