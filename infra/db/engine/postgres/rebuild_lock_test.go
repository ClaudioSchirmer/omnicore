package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestViewLockKey_Deterministic(t *testing.T) {
	a := viewLockKey("users")
	b := viewLockKey("users")
	if a != b {
		t.Fatalf("viewLockKey not deterministic: %d vs %d", a, b)
	}
}

func TestViewLockKey_PerViewKeyDiffers(t *testing.T) {
	a := viewLockKey("users")
	b := viewLockKey("orders")
	if a == b {
		t.Error("viewLockKey collides across distinct view names")
	}
}

func TestViewLockKey_PrefixIsolatesFromConsumerKeys(t *testing.T) {
	// A consumer that hashes a bare "users" string for its own advisory
	// lock must NOT collide with the framework's key for the same view
	// name. The prefix is what enforces this.
	frameworkKey := viewLockKey("users")
	// Recompute without prefix — this is what a naive consumer might do.
	bareKey := bareFNV64a("users")
	if frameworkKey == bareKey {
		t.Error("framework key matches a bare-FNV consumer hash — prefix isolation broken")
	}
}

func bareFNV64a(s string) int64 {
	// Replicates fnv.New64a().Write([]byte(s)) → Sum64() WITHOUT the
	// framework prefix. Used in the prefix-isolation test only.
	h := fnvNew64a()
	h.write([]byte(s))
	return int64(h.sum64())
}

// Minimal stand-in for hash/fnv so the test does not depend on the same
// import that the production code uses. Same FNV-1a constants.

type fnv64a struct{ h uint64 }

func fnvNew64a() *fnv64a {
	return &fnv64a{h: 0xcbf29ce484222325}
}

func (f *fnv64a) write(p []byte) {
	for _, b := range p {
		f.h ^= uint64(b)
		f.h *= 0x100000001b3
	}
}

func (f *fnv64a) sum64() uint64 { return f.h }

func TestSplitAdvisoryKey_RoundTrip(t *testing.T) {
	// Splitting the int64 key into the (classid, objid) pair used in
	// pg_locks must be deterministic — reassembling them yields the
	// original key. Both halves are uint32 (matching the oid columns in
	// pg_locks), so the reassembly is bit-shift + OR with no sign-extension
	// games.
	cases := []int64{0, 1, -1, 1 << 31, 1<<31 - 1, viewLockKey("users")}
	for _, k := range cases {
		classid, objid := splitAdvisoryKey(k)
		reassembled := int64((uint64(classid) << 32) | uint64(objid))
		if reassembled != k {
			t.Errorf("splitAdvisoryKey(%d) → (%d, %d) round-trip = %d", k, classid, objid, reassembled)
		}
	}
}

func TestSQLConstants_PointAtAdvisoryFunctions(t *testing.T) {
	if !strings.Contains(sqlTryAdvisoryLock, "pg_try_advisory_lock") {
		t.Error("sqlTryAdvisoryLock does not call pg_try_advisory_lock")
	}
	if !strings.Contains(sqlAdvisoryUnlock, "pg_advisory_unlock") {
		t.Error("sqlAdvisoryUnlock does not call pg_advisory_unlock")
	}
	if !strings.Contains(sqlReadViewLockHolder, "pg_locks") || !strings.Contains(sqlReadViewLockHolder, "pg_stat_activity") {
		t.Error("sqlReadViewLockHolder must join pg_locks and pg_stat_activity")
	}
	if !strings.Contains(sqlReadViewLockHolder, "'advisory'") {
		t.Error("sqlReadViewLockHolder must scope to advisory locks (locktype = 'advisory')")
	}
}

// ─── advisory-lock helpers (driven through a scriptable pgExec) ───────────────

// covRow is a minimal pgx.Row over a positional value slice.
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

// covExec scripts the pg-local pgExec seam: QueryRow returns the configured
// covRow; Exec/Query are stubs (the lock helpers only use QueryRow).
type covExec struct {
	row              *covRow
	lastQueryRowSQL  string
	lastQueryRowArgs []any
}

func (e *covExec) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (e *covExec) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (e *covExec) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	e.lastQueryRowSQL = sql
	e.lastQueryRowArgs = args
	if e.row == nil {
		e.row = &covRow{scanErr: pgx.ErrNoRows}
	}
	return e.row
}

func TestTryAcquireViewLock_Granted(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{true}}}
	ok, err := tryAcquireViewLock(context.Background(), exec, "users")
	if err != nil || !ok {
		t.Fatalf("expected granted lock, got ok=%v err=%v", ok, err)
	}
	if exec.lastQueryRowSQL != sqlTryAdvisoryLock {
		t.Fatalf("unexpected SQL: %s", exec.lastQueryRowSQL)
	}
	if exec.lastQueryRowArgs[0] != viewLockKey("users") {
		t.Fatalf("lock key arg drifted: %v", exec.lastQueryRowArgs[0])
	}
}

func TestTryAcquireViewLock_NotGranted(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{false}}}
	ok, err := tryAcquireViewLock(context.Background(), exec, "users")
	if err != nil || ok {
		t.Fatalf("expected not-granted, got ok=%v err=%v", ok, err)
	}
}

func TestTryAcquireViewLock_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if _, err := tryAcquireViewLock(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "pg_try_advisory_lock") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestReleaseViewLock_Released(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{true}}}
	if err := releaseViewLock(context.Background(), exec, "users"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReleaseViewLock_NotHeld(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{false}}}
	err := releaseViewLock(context.Background(), exec, "users")
	if err == nil || !strings.Contains(err.Error(), "was not held") {
		t.Fatalf("expected not-held error, got %v", err)
	}
}

func TestReleaseViewLock_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if err := releaseViewLock(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "pg_advisory_unlock") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}

func TestReadViewLockHolder_ReturnsHolder(t *testing.T) {
	exec := &covExec{row: &covRow{values: []any{int32(4242), "users-svc", "10.0.0.1"}}}
	h, err := readViewLockHolder(context.Background(), exec, "users")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h == nil || h.PID != 4242 || h.ApplicationName != "users-svc" || h.ClientAddr != "10.0.0.1" {
		t.Fatalf("holder drifted: %+v", h)
	}
	// classid + objid are derived from the lock key.
	classid, objid := splitAdvisoryKey(viewLockKey("users"))
	if exec.lastQueryRowArgs[0] != classid || exec.lastQueryRowArgs[1] != objid {
		t.Fatalf("classid/objid args drifted: %v", exec.lastQueryRowArgs)
	}
}

func TestReadViewLockHolder_NoRowsReturnsNilNil(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: pgx.ErrNoRows}}
	h, err := readViewLockHolder(context.Background(), exec, "users")
	if err != nil || h != nil {
		t.Fatalf("expected (nil, nil) on no rows, got h=%v err=%v", h, err)
	}
}

func TestReadViewLockHolder_ScanErrorWrapped(t *testing.T) {
	exec := &covExec{row: &covRow{scanErr: errors.New("conn reset")}}
	if _, err := readViewLockHolder(context.Background(), exec, "users"); err == nil ||
		!strings.Contains(err.Error(), "read lock holder") {
		t.Fatalf("expected wrapped error, got %v", err)
	}
}
