package mongo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// Backend-neutral control-plane helpers that run through the engine's db.Querier
// seam: the Mongo-view registry (ReadViewRegistry / InitViewRegistry /
// BeginRebuild / EndRebuild / ListNonDone), the upstream-failure list helpers,
// and the drift-decision stringer. The pg-specific pieces this file used to also
// cover — the advisory lock, the struct scanners, the hook dispatch — moved to
// packages db / db/pg along with their code and are tested there now.
//
// covExec scripts a db.Querier (Exec / Query / QueryRow / QueryMaps) so the
// helpers run without a live database. Distinct names (cov*) avoid clashing with
// fakeQuerier (engine_fake_test.go) and fakePgExec (upstream_failures_test.go).

type covExec struct {
	execErr      error
	execCalls    int
	lastExecSQL  string
	lastExecArgs []any

	rows          *covRows
	queryErr      error
	lastQuerySQL  string
	lastQueryArgs []any
}

func (e *covExec) Exec(_ context.Context, sql string, args ...any) error {
	e.execCalls++
	e.lastExecSQL = sql
	e.lastExecArgs = args
	return e.execErr
}

func (e *covExec) Query(_ context.Context, sql string, args ...any) (db.Rows, error) {
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

func (e *covExec) QueryRow(context.Context, string, ...any) db.Row { return &covRow{} }
func (e *covExec) QueryMaps(context.Context, string, ...any) ([]map[string]any, error) {
	return nil, nil
}

// covRow is a minimal db.Row over a positional value slice.
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

// covRows is a minimal db.Rows over a slice of positional row values.
type covRows struct {
	data     [][]any
	idx      int
	scanErr  error
	errAfter error
}

func (r *covRows) Close() error { return nil }
func (r *covRows) Err() error   { return r.errAfter }

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

// covAssign sets *dst = src with convertibility tolerance, mirroring how a driver
// lands a column value into a typed destination pointer. A nil src leaves the
// destination at its zero value (the column was NULL).
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
// pg_view_registry.go — ReadViewRegistry / InitViewRegistry / BeginRebuild /
// EndRebuild / ListNonDone (now backend-neutral over db.Querier + db.Dialect)
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
	exec := &covExec{rows: &covRows{data: [][]any{registryRowValues()}}}
	out, err := ReadViewRegistry(context.Background(), exec, fakeDialect{}, "users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out == nil {
		t.Fatal("expected a row, got nil")
	}
	if out.ViewName != "users" || out.Version != 3 || out.Status != ViewRegistryStatusDone {
		t.Fatalf("scanned fields drifted: %+v", out)
	}
	if len(exec.lastQueryArgs) != 1 || exec.lastQueryArgs[0] != "users" {
		t.Fatalf("view name must be the only bound arg, got %v", exec.lastQueryArgs)
	}
}

func TestReadViewRegistry_NoRowsReturnsNilNil(t *testing.T) {
	// Empty result set (Next() false) is the neutral "no row" signal.
	exec := &covExec{rows: &covRows{}}
	out, err := ReadViewRegistry(context.Background(), exec, fakeDialect{}, "users")
	if err != nil {
		t.Fatalf("no rows must map to (nil, nil), got err %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil row, got %+v", out)
	}
}

func TestReadViewRegistry_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{rows: &covRows{data: [][]any{registryRowValues()}, scanErr: errors.New("conn reset")}}
	if _, err := ReadViewRegistry(context.Background(), exec, fakeDialect{}, "users"); err == nil ||
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
	if err := InitViewRegistry(context.Background(), exec, fakeDialect{}, in); err != nil {
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
	err := InitViewRegistry(context.Background(), exec, fakeDialect{}, InitViewRegistryInput{ViewName: "users"})
	if err == nil || !strings.Contains(err.Error(), "init view registry") {
		t.Fatalf("expected wrapped init error, got %v", err)
	}
}

func TestBeginRebuild_Succeeds(t *testing.T) {
	exec := &covExec{}
	if err := BeginRebuild(context.Background(), exec, fakeDialect{}, "users", time.Now()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// view_name is the WHERE-clause arg, last in appearance order (started_at,
	// pid, host, view_name) so MySQL's positional binding lands it correctly.
	if exec.lastExecArgs[len(exec.lastExecArgs)-1] != "users" {
		t.Fatalf("view name arg drifted: %v", exec.lastExecArgs)
	}
}

func TestBeginRebuild_ExecErrorWrapped(t *testing.T) {
	exec := &covExec{execErr: errors.New("db down")}
	err := BeginRebuild(context.Background(), exec, fakeDialect{}, "users", time.Now())
	if err == nil || !strings.Contains(err.Error(), "begin rebuild") {
		t.Fatalf("expected wrapped begin error, got %v", err)
	}
}

func TestEndRebuild_Succeeds(t *testing.T) {
	exec := &covExec{}
	in := EndRebuildInput{
		ViewName: "users", Version: 2, RebuildHash: "rh", ArtifactHash: "ah",
		CombinedHash: "ch", ServiceName: "users-svc", Now: time.Now(),
	}
	if err := EndRebuild(context.Background(), exec, fakeDialect{}, in); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEndRebuild_ExecErrorWrapped(t *testing.T) {
	exec := &covExec{execErr: errors.New("db down")}
	err := EndRebuild(context.Background(), exec, fakeDialect{}, EndRebuildInput{ViewName: "users"})
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
	out, err := ListPendingUpstreamFailuresByTopic(context.Background(), exec, fakeDialect{}, "users.events")
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
// mongo_drift.go — DriftDecision.String unknown branch
// ============================================================================

func TestDriftDecisionString_Unknown(t *testing.T) {
	if got := DriftDecision(99).String(); got != "unknown" {
		t.Fatalf("DriftDecision(99).String() = %q, want unknown", got)
	}
}
