package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/audit"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/ClaudioSchirmer/omnicore/infra/events"
)

// --- in-process fakes implementing the neutral infra read seam -------------

// fakeExec implements core.Querier. It records the SQL + args each call
// received and replays scripted results, so the failure/processed helpers are
// exercised without a live database.
type fakeExec struct {
	execErr  error
	queryErr error

	rows *fakeRows // returned by Query
	row  *fakeRow  // returned by QueryRow

	lastSQL  string
	lastArgs []any
	calls    int
}

func (f *fakeExec) Exec(_ context.Context, sql string, args ...any) error {
	f.calls++
	f.lastSQL = sql
	f.lastArgs = args
	return f.execErr
}

func (f *fakeExec) Query(_ context.Context, sql string, args ...any) (core.Rows, error) {
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

func (f *fakeExec) QueryRow(_ context.Context, sql string, args ...any) core.Row {
	f.lastSQL = sql
	f.lastArgs = args
	if f.row == nil {
		f.row = &fakeRow{}
	}
	return f.row
}

func (f *fakeExec) QueryMaps(_ context.Context, sql string, args ...any) ([]map[string]any, error) {
	f.lastSQL = sql
	f.lastArgs = args
	return nil, f.queryErr
}

// fakeDialect is a trivial core.Dialect — the fakeExec ignores the SQL text,
// so the rendering only needs to not panic.
type fakeDialect struct{}

func (fakeDialect) Placeholder(int) string              { return "?" }
func (fakeDialect) QuoteIdent(s string) string          { return s }
func (fakeDialect) EncodeArg(v any) any                 { return v }
func (fakeDialect) DecodeID(raw string) (string, error) { return raw, nil }
func (fakeDialect) ILikeClause(col, ph string) string   { return col + " LIKE " + ph }
func (fakeDialect) NowExpr() string                     { return "NOW()" }
func (fakeDialect) ApplyLimit(sql string, n int) string {
	return fmt.Sprintf("%s LIMIT %d", sql, n)
}
func (fakeDialect) IsUniqueViolation(error) (string, bool)     { return "", false }
func (fakeDialect) IsForeignKeyViolation(error) (string, bool) { return "", false }
func (fakeDialect) BuildUpsert(table string, _, _ []string, _ []core.UpsertSet) string {
	return "UPSERT " + table
}

// fakeEngine adapts a fake Querier + Dialect into an core.RelationalEngine so
// handleMessage / RetryPendingFailures (which take the engine and derive
// Querier()/Dialect()) run without a live backend. The write verbs are never
// reached on the consumer path.
type fakeEngine struct {
	q core.Querier
	d core.Dialect
}

func (e fakeEngine) Insert(persistence.RequestContext, domain.Insertable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (e fakeEngine) Update(persistence.RequestContext, domain.Updatable, *core.TableSchema, core.WriteHook) (domain.WriteResult, error) {
	return domain.WriteResult{}, nil
}
func (e fakeEngine) Archive(persistence.RequestContext, domain.Archivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e fakeEngine) Unarchive(persistence.RequestContext, domain.Unarchivable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e fakeEngine) Delete(persistence.RequestContext, domain.Deletable, *core.TableSchema, core.WriteHook) error {
	return nil
}
func (e fakeEngine) Querier() core.Querier { return e.q }
func (e fakeEngine) Dialect() core.Dialect { return e.d }
func (e fakeEngine) WithAudit(*audit.Config, *slog.Logger, []string) core.RelationalEngine {
	return e
}
func (e fakeEngine) WithEventPublisher(events.Publisher) core.RelationalEngine { return e }
func (e fakeEngine) AcquireRebuildLock(context.Context, string) (core.RebuildLock, error) {
	return fakeRebuildLock{q: e.q}, nil
}
func (e fakeEngine) Close() {}

// fakeRebuildLock is the no-op RebuildLock for the receiver-path tests (these
// never exercise the rebuild control plane).
type fakeRebuildLock struct{ q core.Querier }

func (fakeRebuildLock) Acquired() bool                { return true }
func (fakeRebuildLock) Holder() string                { return "" }
func (l fakeRebuildLock) Querier() core.Querier       { return l.q }
func (fakeRebuildLock) Release(context.Context) error { return nil }

// engineFor wraps a fakeExec as a RelationalEngine for the receiver-path tests.
func engineFor(exec *fakeExec) core.RelationalEngine {
	return fakeEngine{q: exec, d: fakeDialect{}}
}

// fakeRows is a minimal core.Rows over a slice of positional row values.
type fakeRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error // returned by Err() after iteration
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
		if err := RecordIntegrationFailure(context.Background(), exec, fakeDialect{}, rec); err == nil {
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
	if err := RecordIntegrationFailure(context.Background(), exec, fakeDialect{}, rec); err != nil {
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
	err := RecordIntegrationFailure(context.Background(), exec, fakeDialect{}, rec)
	if err == nil || !errors.Is(err, exec.execErr) {
		t.Fatalf("expected wrapped exec error, got %v", err)
	}
}

// --- ResolveIntegrationFailures --------------------------------------------

func TestResolveIntegrationFailures(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		exec := &fakeExec{}
		if err := ResolveIntegrationFailures(context.Background(), exec, fakeDialect{}, "", "s", "e", uuid.New()); err == nil {
			t.Error("expected error for empty consumer group")
		}
		if err := ResolveIntegrationFailures(context.Background(), exec, fakeDialect{}, "g", "s", "e", uuid.Nil); err == nil {
			t.Error("expected error for nil event id")
		}
		if exec.calls != 0 {
			t.Errorf("validation must short-circuit, got %d calls", exec.calls)
		}
	})
	t.Run("success", func(t *testing.T) {
		exec := &fakeExec{}
		if err := ResolveIntegrationFailures(context.Background(), exec, fakeDialect{}, "g", "s", "e", uuid.New()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("exec-error", func(t *testing.T) {
		exec := &fakeExec{execErr: errors.New("nope")}
		if err := ResolveIntegrationFailures(context.Background(), exec, fakeDialect{}, "g", "s", "e", uuid.New()); err == nil {
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
		if _, err := ListPendingIntegrationFailuresByGroup(context.Background(), exec, fakeDialect{}, ""); err == nil {
			t.Fatal("expected error for empty consumer group")
		}
	})
	t.Run("passes-group-arg-and-scans", func(t *testing.T) {
		exec := &fakeExec{rows: &fakeRows{data: [][]any{sampleFailureRow(7)}}}
		out, err := ListPendingIntegrationFailuresByGroup(context.Background(), exec, fakeDialect{}, "orders-int")
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
		if _, err := IsAlreadyProcessed(context.Background(), exec, fakeDialect{}, uuid.Nil, "g"); err == nil {
			t.Error("expected error for nil event id")
		}
		if _, err := IsAlreadyProcessed(context.Background(), exec, fakeDialect{}, uuid.New(), ""); err == nil {
			t.Error("expected error for empty consumer group")
		}
	})
	t.Run("found", func(t *testing.T) {
		// A row present → Next() true → already processed (engine-neutral, no
		// no-rows sentinel).
		exec := &fakeExec{rows: &fakeRows{data: [][]any{{1}}}}
		got, err := IsAlreadyProcessed(context.Background(), exec, fakeDialect{}, uuid.New(), "g")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !got {
			t.Fatal("expected true when a dedup row exists")
		}
	})
	t.Run("not-found", func(t *testing.T) {
		// No rows → Next() false → (false, nil).
		exec := &fakeExec{rows: &fakeRows{}}
		got, err := IsAlreadyProcessed(context.Background(), exec, fakeDialect{}, uuid.New(), "g")
		if err != nil {
			t.Fatalf("no rows must map to (false, nil), got err %v", err)
		}
		if got {
			t.Fatal("expected false when no dedup row exists")
		}
	})
	t.Run("query-error", func(t *testing.T) {
		exec := &fakeExec{queryErr: errors.New("conn reset")}
		if _, err := IsAlreadyProcessed(context.Background(), exec, fakeDialect{}, uuid.New(), "g"); err == nil {
			t.Fatal("expected real query error to surface")
		}
	})
}

// --- MarkProcessed ----------------------------------------------------------

func TestMarkProcessed(t *testing.T) {
	t.Run("validation", func(t *testing.T) {
		exec := &fakeExec{}
		if err := MarkProcessed(context.Background(), exec, fakeDialect{}, IntegrationProcessedRecord{ConsumerGroup: "g"}); err == nil {
			t.Error("expected error for nil event id")
		}
		if err := MarkProcessed(context.Background(), exec, fakeDialect{}, IntegrationProcessedRecord{EventID: uuid.New()}); err == nil {
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
		if err := MarkProcessed(context.Background(), exec, fakeDialect{}, rec); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exec.calls != 1 || len(exec.lastArgs) != 6 {
			t.Fatalf("expected 1 exec with 6 args, got calls=%d args=%d", exec.calls, len(exec.lastArgs))
		}
	})
	t.Run("exec-error", func(t *testing.T) {
		exec := &fakeExec{execErr: errors.New("insert failed")}
		rec := IntegrationProcessedRecord{EventID: uuid.New(), ConsumerGroup: "g"}
		if err := MarkProcessed(context.Background(), exec, fakeDialect{}, rec); err == nil {
			t.Fatal("expected exec error")
		}
	})
}
