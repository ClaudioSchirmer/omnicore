package infra

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// --- in-process fakes implementing the pgExec / pgx.Row / pgx.Rows seams ----
//
// Distinct names (cov*) so they don't clash with fakePgExec (upstream_failures_test.go)
// or the integration package's fakeExec. covExec scripts Exec/Query/QueryRow
// results so the registry, lock, and list helpers run without a live Postgres.

type covExec struct {
	execTag      pgconn.CommandTag
	execErr      error
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any

	rows          *covRows
	queryErr      error
	lastQuerySQL  string
	lastQueryArgs []any

	row              *covRow
	lastQueryRowSQL  string
	lastQueryRowArgs []any
}

func (e *covExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	e.execCalls++
	e.lastExecSQL = sql
	e.lastExecArgs = args
	return e.execTag, e.execErr
}

func (e *covExec) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	e.lastQuerySQL = sql
	e.lastQueryArgs = args
	if e.queryErr != nil {
		return nil, e.queryErr
	}
	if e.rows == nil {
		e.rows = &covRows{}
	}
	return e.rows, nil
}

func (e *covExec) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	e.lastQueryRowSQL = sql
	e.lastQueryRowArgs = args
	if e.row == nil {
		e.row = &covRow{scanErr: pgx.ErrNoRows}
	}
	return e.row
}

// covRow is a minimal pgx.Row / keyedRow over a positional value slice.
type covRow struct {
	values  []any
	scanErr error
}

func (r *covRow) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	for i, d := range dest {
		if i >= len(r.values) {
			break
		}
		if err := covAssign(d, r.values[i]); err != nil {
			return err
		}
	}
	return nil
}

// covRows is a minimal pgx.Rows over a slice of positional row values.
type covRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error
}

func (r *covRows) Close()                                       {}
func (r *covRows) Err() error                                   { return r.errAfter }
func (r *covRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *covRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *covRows) Values() ([]any, error)                       { return nil, nil }
func (r *covRows) RawValues() [][]byte                          { return nil }
func (r *covRows) Conn() *pgx.Conn                              { return nil }

func (r *covRows) Next() bool {
	if r.idx >= len(r.data) {
		return false
	}
	r.idx++
	return true
}

func (r *covRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx-1]
	if len(dest) != len(row) {
		return fmt.Errorf("covRows.Scan: dest len %d != row len %d", len(dest), len(row))
	}
	for i, d := range dest {
		if err := covAssign(d, row[i]); err != nil {
			return fmt.Errorf("col %d: %w", i, err)
		}
	}
	return nil
}

// covAssign sets *dst = src with convertibility tolerance, mirroring how the
// pgx driver lands a column value into a typed destination pointer. A nil src
// leaves the destination at its zero value (the column was NULL).
func covAssign(dst any, src any) error {
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Pointer || dv.IsNil() {
		return fmt.Errorf("dest must be a non-nil pointer, got %T", dst)
	}
	target := dv.Elem()
	sv := reflect.ValueOf(src)
	if !sv.IsValid() {
		return nil
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

// ============================================================================
// struct_scan.go — scanRowIntoStruct / scanLeadingKey
// ============================================================================

type scanTargetStruct struct {
	Name string
	Age  int
}

func TestScanRowIntoStruct_FillsFields(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &covRow{values: []any{"bob", 42}}
	err := scanRowIntoStruct(row, dst, []string{"name", "age"}, map[string]int{"name": 0, "age": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dst.Name != "bob" || dst.Age != 42 {
		t.Fatalf("fields drifted: %+v", dst)
	}
}

func TestScanRowIntoStruct_RejectsNonPointer(t *testing.T) {
	if err := scanRowIntoStruct(&covRow{}, scanTargetStruct{}, nil, nil); err == nil {
		t.Fatal("expected error for non-pointer dst")
	}
	var nilPtr *scanTargetStruct
	if err := scanRowIntoStruct(&covRow{}, nilPtr, nil, nil); err == nil {
		t.Fatal("expected error for nil pointer dst")
	}
}

func TestScanRowIntoStruct_RejectsNonStruct(t *testing.T) {
	x := 7
	if err := scanRowIntoStruct(&covRow{}, &x, nil, nil); err == nil {
		t.Fatal("expected error for pointer-to-non-struct dst")
	}
}

func TestScanRowIntoStruct_UnknownColumn(t *testing.T) {
	dst := &scanTargetStruct{}
	err := scanRowIntoStruct(&covRow{values: []any{"x"}}, dst, []string{"missing"}, map[string]int{"name": 0})
	if err == nil || !strings.Contains(err.Error(), "no corresponding field") {
		t.Fatalf("expected unknown-column error, got %v", err)
	}
}

func TestScanRowIntoStruct_ScanErrorPropagates(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &covRow{scanErr: errors.New("boom")}
	if err := scanRowIntoStruct(row, dst, []string{"name"}, map[string]int{"name": 0}); err == nil {
		t.Fatal("expected scan error to propagate")
	}
}

func TestScanLeadingKey_ReturnsKeyAndFills(t *testing.T) {
	dst := &scanTargetStruct{}
	row := &covRow{values: []any{"id-1", "bob", 42}}
	key, err := scanLeadingKey(row, dst, []string{"name", "age"}, map[string]int{"name": 0, "age": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if key != "id-1" {
		t.Fatalf("key = %q, want id-1", key)
	}
	if dst.Name != "bob" || dst.Age != 42 {
		t.Fatalf("fields drifted: %+v", dst)
	}
}

func TestScanLeadingKey_RejectsNonPointer(t *testing.T) {
	if _, err := scanLeadingKey(&covRow{}, scanTargetStruct{}, nil, nil); err == nil {
		t.Fatal("expected error for non-pointer dst")
	}
	var nilPtr *scanTargetStruct
	if _, err := scanLeadingKey(&covRow{}, nilPtr, nil, nil); err == nil {
		t.Fatal("expected error for nil pointer dst")
	}
}

func TestScanLeadingKey_RejectsNonStruct(t *testing.T) {
	x := 0
	if _, err := scanLeadingKey(&covRow{}, &x, nil, nil); err == nil {
		t.Fatal("expected error for pointer-to-non-struct dst")
	}
}

func TestScanLeadingKey_UnknownColumn(t *testing.T) {
	dst := &scanTargetStruct{}
	_, err := scanLeadingKey(&covRow{values: []any{"id"}}, dst, []string{"missing"}, map[string]int{"name": 0})
	if err == nil || !strings.Contains(err.Error(), "no corresponding field") {
		t.Fatalf("expected unknown-column error, got %v", err)
	}
}

// ============================================================================
// pg_view_registry.go — ReadViewRegistry / InitViewRegistry / BeginRebuild /
// EndRebuild / ListNonDone
// ============================================================================

// registryRowValues returns a positional value slice aligned to the 15-column
// SELECT in sqlReadViewRegistry. Pointer columns are passed nil (NULL).
func registryRowValues() []any {
	return []any{
		"users",           // view_name
		3,                 // version
		"rh",              // rebuild_hash
		"ah",              // artifact_hash
		"ch",              // combined_hash
		nil,               // previous_version *int
		nil,               // previous_combined_hash *string
		nil,               // previous_applied_at *time.Time
		"done",            // status
		nil,               // started_at *time.Time
		nil,               // pid *string
		nil,               // host *string
		time.Now(),        // applied_at
		"users-svc@pid:1", // applied_by
		nil,               // code_version *string
	}
}

func TestReadViewRegistry_ScansRow(t *testing.T) {
	exec := &covExec{row: &covRow{values: registryRowValues()}}
	out, err := ReadViewRegistry(context.Background(), exec, "users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("expected a row, got nil")
	}
	if out.ViewName != "users" || out.Version != 3 || out.Status != ViewRegistryStatusDone {
		t.Fatalf("scanned fields drifted: %+v", out)
	}
	if len(exec.lastQueryRowArgs) != 1 || exec.lastQueryRowArgs[0] != "users" {
		t.Fatalf("view name must be the only bound arg, got %v", exec.lastQueryRowArgs)
	}
}

func TestReadViewRegistry_NoRowsReturnsNilNil(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: pgx.ErrNoRows}}
	out, err := ReadViewRegistry(context.Background(), exec, "users")
	if err != nil {
		t.Fatalf("ErrNoRows must map to (nil, nil), got err %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil row, got %+v", out)
	}
}

func TestReadViewRegistry_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if _, err := ReadViewRegistry(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "read view registry") {
		t.Fatalf("expected wrapped read error, got %v", err)
	}
}

func TestInitViewRegistry_PassesArgs(t *testing.T) {
	exec := &covExec{}
	in := InitViewRegistryInput{
		ViewName: "users", Version: 1, RebuildHash: "rh", ArtifactHash: "ah",
		CombinedHash: "ch", ServiceName: "users-svc", Now: time.Now(),
	}
	if err := InitViewRegistry(context.Background(), exec, in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.execCalls != 1 {
		t.Fatalf("expected 1 exec call, got %d", exec.execCalls)
	}
	if exec.lastExecArgs[0] != "users" || exec.lastExecArgs[1] != 1 {
		t.Fatalf("args drifted: %v", exec.lastExecArgs)
	}
	// applied_by is the FormatRegistryAppliedBy render.
	if got, _ := exec.lastExecArgs[6].(string); !strings.HasPrefix(got, "users-svc@pid:") {
		t.Fatalf("applied_by arg drifted: %v", exec.lastExecArgs[6])
	}
}

func TestInitViewRegistry_ExecErrorWrapped(t *testing.T) {
	exec := &covExec{execErr: errors.New("conflict")}
	err := InitViewRegistry(context.Background(), exec, InitViewRegistryInput{ViewName: "users"})
	if err == nil || !strings.Contains(err.Error(), "init view registry") {
		t.Fatalf("expected wrapped init error, got %v", err)
	}
}

func TestBeginRebuild_Succeeds(t *testing.T) {
	exec := &covExec{execTag: pgconn.NewCommandTag("UPDATE 1")}
	if err := BeginRebuild(context.Background(), exec, "users", time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exec.lastExecArgs[0] != "users" {
		t.Fatalf("view name arg drifted: %v", exec.lastExecArgs)
	}
}

func TestBeginRebuild_MissingRowErrors(t *testing.T) {
	exec := &covExec{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := BeginRebuild(context.Background(), exec, "users", time.Now())
	if err == nil || !strings.Contains(err.Error(), "registry row missing") {
		t.Fatalf("expected missing-row error, got %v", err)
	}
}

func TestBeginRebuild_ExecErrorWrapped(t *testing.T) {
	exec := &covExec{execErr: errors.New("db down")}
	err := BeginRebuild(context.Background(), exec, "users", time.Now())
	if err == nil || !strings.Contains(err.Error(), "begin rebuild") {
		t.Fatalf("expected wrapped begin error, got %v", err)
	}
}

func TestEndRebuild_Succeeds(t *testing.T) {
	exec := &covExec{execTag: pgconn.NewCommandTag("UPDATE 1")}
	in := EndRebuildInput{
		ViewName: "users", Version: 2, RebuildHash: "rh", ArtifactHash: "ah",
		CombinedHash: "ch", ServiceName: "users-svc", Now: time.Now(),
	}
	if err := EndRebuild(context.Background(), exec, in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEndRebuild_MissingRowErrors(t *testing.T) {
	exec := &covExec{execTag: pgconn.NewCommandTag("UPDATE 0")}
	err := EndRebuild(context.Background(), exec, EndRebuildInput{ViewName: "users"})
	if err == nil || !strings.Contains(err.Error(), "registry row missing") {
		t.Fatalf("expected missing-row error, got %v", err)
	}
}

func TestEndRebuild_ExecErrorWrapped(t *testing.T) {
	exec := &covExec{execErr: errors.New("db down")}
	err := EndRebuild(context.Background(), exec, EndRebuildInput{ViewName: "users"})
	if err == nil || !strings.Contains(err.Error(), "end rebuild") {
		t.Fatalf("expected wrapped end error, got %v", err)
	}
}

func TestListNonDone_ScansRows(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{registryRowValues(), registryRowValues()}}}
	out, err := ListNonDone(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].ViewName != "users" || out[0].Version != 3 {
		t.Fatalf("scanned fields drifted: %+v", out[0])
	}
}

func TestListNonDone_QueryError(t *testing.T) {
	exec := &covExec{queryErr: errors.New("query boom")}
	if _, err := ListNonDone(context.Background(), exec); err == nil ||
		!strings.Contains(err.Error(), "list non-done views") {
		t.Fatalf("expected wrapped query error, got %v", err)
	}
}

func TestListNonDone_ScanError(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{registryRowValues()}, scanErr: errors.New("bad scan")}}
	if _, err := ListNonDone(context.Background(), exec); err == nil ||
		!strings.Contains(err.Error(), "scan non-done view row") {
		t.Fatalf("expected wrapped scan error, got %v", err)
	}
}

func TestListNonDone_RowsErr(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{registryRowValues()}, errAfter: errors.New("late err")}}
	if _, err := ListNonDone(context.Background(), exec); err == nil ||
		!strings.Contains(err.Error(), "iterate non-done views") {
		t.Fatalf("expected wrapped rows.Err, got %v", err)
	}
}

// ============================================================================
// pg_view_lock.go — TryAcquireViewLock / ReleaseViewLock / ReadViewLockHolder
// ============================================================================

func TestTryAcquireViewLock_Granted(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{true}}}
	ok, err := TryAcquireViewLock(context.Background(), exec, "users")
	if err != nil || !ok {
		t.Fatalf("expected granted lock, got ok=%v err=%v", ok, err)
	}
	if exec.lastQueryRowSQL != sqlTryAdvisoryLock {
		t.Fatalf("unexpected SQL: %s", exec.lastQueryRowSQL)
	}
	if exec.lastQueryRowArgs[0] != ViewLockKey("users") {
		t.Fatalf("lock key arg drifted: %v", exec.lastQueryRowArgs[0])
	}
}

func TestTryAcquireViewLock_NotGranted(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{false}}}
	ok, err := TryAcquireViewLock(context.Background(), exec, "users")
	if err != nil || ok {
		t.Fatalf("expected not-granted, got ok=%v err=%v", ok, err)
	}
}

func TestTryAcquireViewLock_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if _, err := TryAcquireViewLock(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "pg_try_advisory_lock") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestReleaseViewLock_Released(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{true}}}
	if err := ReleaseViewLock(context.Background(), exec, "users"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReleaseViewLock_NotHeld(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{false}}}
	err := ReleaseViewLock(context.Background(), exec, "users")
	if err == nil || !strings.Contains(err.Error(), "was not held") {
		t.Fatalf("expected not-held error, got %v", err)
	}
}

func TestReleaseViewLock_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if err := ReleaseViewLock(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "pg_advisory_unlock") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestReadViewLockHolder_ReturnsHolder(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{int32(4242), "users-svc", "10.0.0.1"}}}
	h, err := ReadViewLockHolder(context.Background(), exec, "users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.PID != 4242 || h.ApplicationName != "users-svc" || h.ClientAddr != "10.0.0.1" {
		t.Fatalf("holder drifted: %+v", h)
	}
	// classid + objid are derived from the lock key.
	classid, objid := splitAdvisoryKey(ViewLockKey("users"))
	if exec.lastQueryRowArgs[0] != classid || exec.lastQueryRowArgs[1] != objid {
		t.Fatalf("classid/objid args drifted: %v", exec.lastQueryRowArgs)
	}
}

func TestReadViewLockHolder_NoRowsReturnsNilNil(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: pgx.ErrNoRows}}
	h, err := ReadViewLockHolder(context.Background(), exec, "users")
	if err != nil || h != nil {
		t.Fatalf("expected (nil, nil) on no rows, got h=%v err=%v", h, err)
	}
}

func TestReadViewLockHolder_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if _, err := ReadViewLockHolder(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "read lock holder") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestSplitAdvisoryKey_RoundTrips(t *testing.T) {
	key := ViewLockKey("orders")
	classid, objid := splitAdvisoryKey(key)
	got := int64(uint64(classid)<<32 | uint64(objid))
	if got != key {
		t.Fatalf("split/recombine drifted: key=%d got=%d", key, got)
	}
}

// ============================================================================
// upstream_failures.go — list / scan path
// ============================================================================

func upstreamFailureRow(id int64) []any {
	return []any{
		id,             // id
		"users.events", // subscription_topic
		"orders",       // view_name
		"u1",           // upstream_id
		"ord-7",        // local_id
		"compose",      // stage
		"boom",         // error
		2,              // attempt
		time.Now(),     // first_seen_at
		time.Now(),     // last_attempt_at
	}
}

func TestListPendingUpstreamFailures_ScansRows(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{upstreamFailureRow(1), upstreamFailureRow(2)}}}
	out, err := ListPendingUpstreamFailures(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].ID != 1 || out[0].Stage != UpstreamFailureStageCompose || out[0].Attempt != 2 {
		t.Fatalf("scanned fields drifted: %+v", out[0])
	}
}

func TestListPendingUpstreamFailures_QueryError(t *testing.T) {
	exec := &covExec{queryErr: errors.New("query boom")}
	if _, err := ListPendingUpstreamFailures(context.Background(), exec); err == nil {
		t.Fatal("expected query error")
	}
}

func TestListPendingUpstreamFailures_ScanError(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{upstreamFailureRow(1)}, scanErr: errors.New("bad scan")}}
	if _, err := ListPendingUpstreamFailures(context.Background(), exec); err == nil ||
		!strings.Contains(err.Error(), "scan") {
		t.Fatalf("expected wrapped scan error, got %v", err)
	}
}

func TestListPendingUpstreamFailures_RowsErr(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{upstreamFailureRow(1)}, errAfter: errors.New("late err")}}
	if _, err := ListPendingUpstreamFailures(context.Background(), exec); err == nil ||
		!strings.Contains(err.Error(), "rows") {
		t.Fatalf("expected wrapped rows.Err, got %v", err)
	}
}

func TestListPendingUpstreamFailuresByTopic_BindsTopicAndScans(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{upstreamFailureRow(7)}}}
	out, err := ListPendingUpstreamFailuresByTopic(context.Background(), exec, "users.events")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].ID != 7 {
		t.Fatalf("unexpected rows: %+v", out)
	}
	if len(exec.lastQueryArgs) != 1 || exec.lastQueryArgs[0] != "users.events" {
		t.Fatalf("topic must be the only bound arg, got %v", exec.lastQueryArgs)
	}
}

// ============================================================================
// hook_dispatch.go — fireAfterBegin / fireBeforeCommit / logHookError
// ============================================================================

func TestFireAfterBegin_NoHookIsNoop(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if err := p.fireAfterBegin(ctx, nil, &hookFlatEntity{}, writeHook{}, hookContext{}); err != nil {
		t.Fatalf("nil hook must be a no-op, got %v", err)
	}
}

func TestFireAfterBegin_FiresAndSucceeds(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	var got persistence.TxHandle
	hook := writeHook{AfterBegin: func(_ persistence.RequestContext, _ domain.Entity, tx persistence.TxHandle) error {
		got = tx
		return nil
	}}
	if err := p.fireAfterBegin(ctx, nil, &hookFlatEntity{}, hook, hookContext{verb: "insert", entityType: "X"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected the hook to receive a TxHandle")
	}
}

func TestFireAfterBegin_ErrorLogsAndPropagates(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	wantErr := errors.New("rejected")
	hook := writeHook{AfterBegin: func(_ persistence.RequestContext, _ domain.Entity, _ persistence.TxHandle) error {
		return wantErr
	}}
	if err := p.fireAfterBegin(ctx, nil, &hookFlatEntity{}, hook, hookContext{verb: "insert", entityType: "X"}); !errors.Is(err, wantErr) {
		t.Fatalf("expected verbatim error, got %v", err)
	}
}

func TestFireBeforeCommit_NoHookIsNoop(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	if err := p.fireBeforeCommit(ctx, nil, &hookFlatEntity{}, domain.NewRandomID(), writeHook{}, hookContext{}); err != nil {
		t.Fatalf("nil hook must be a no-op, got %v", err)
	}
}

func TestFireBeforeCommit_FiresWithID(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	wantID := domain.NewRandomID()
	var gotID domain.ID
	hook := writeHook{BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, id domain.ID, _ persistence.TxHandle) error {
		gotID = id
		return nil
	}}
	if err := p.fireBeforeCommit(ctx, nil, &hookFlatEntity{}, wantID, hook, hookContext{verb: "insert", entityType: "X"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotID != wantID {
		t.Fatalf("hook received id %v, want %v", gotID, wantID)
	}
}

func TestFireBeforeCommit_ErrorLogsAndPropagates(t *testing.T) {
	p := &Postgres{logger: discardLogger()}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	wantErr := errors.New("rejected")
	hook := writeHook{BeforeCommit: func(_ persistence.RequestContext, _ domain.Entity, _ domain.ID, _ persistence.TxHandle) error {
		return wantErr
	}}
	if err := p.fireBeforeCommit(ctx, nil, &hookFlatEntity{}, domain.NewRandomID(), hook, hookContext{verb: "insert", entityType: "X"}); !errors.Is(err, wantErr) {
		t.Fatalf("expected verbatim error, got %v", err)
	}
}

func TestLogHookError_NilLoggerFallsBack(t *testing.T) {
	// A nil logger must fall back to slog.Default without panicking.
	p := &Postgres{}
	ctx := configuration.NewAppContextWithRandomID(configuration.LangENG)
	p.logHookError(ctx, hookContext{verb: "insert", entityType: "X"}, "afterBegin", errors.New("boom"))
}

// ============================================================================
// mongo_drift.go — DriftDecision.String unknown branch
// ============================================================================

func TestDriftDecisionString_Unknown(t *testing.T) {
	if got := DriftDecision(99).String(); got != "unknown" {
		t.Fatalf("DriftDecision(99).String() = %q, want unknown", got)
	}
}
