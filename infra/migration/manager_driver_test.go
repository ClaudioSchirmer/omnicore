//go:build postgres

package migration

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
)

// fakeDriver is an in-memory golang-migrate database.Driver. It tracks the
// version pointer + dirty flag and records every migration body passed to
// Run, so tests can assert exactly which SQL golang-migrate would have
// executed without any real database.
type fakeDriver struct {
	mu      sync.Mutex
	version int // -1 == database.NilVersion (no migration applied)
	dirty   bool

	versionErr    error  // returned by Version
	runErr        error  // returned by Run
	setVersionErr error  // returned by SetVersion
	onVersion     func() // side effect fired at the start of Version (test seam)

	runs   []string // bodies applied via Run, in order
	closed bool
}

func newFakeDriver() *fakeDriver { return &fakeDriver{version: -1} }

func (f *fakeDriver) Open(string) (database.Driver, error) { return f, nil }

func (f *fakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeDriver) Lock() error   { return nil }
func (f *fakeDriver) Unlock() error { return nil }

func (f *fakeDriver) Run(migration io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runErr != nil {
		return f.runErr
	}
	body, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	f.runs = append(f.runs, string(body))
	return nil
}

func (f *fakeDriver) SetVersion(version int, dirty bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setVersionErr != nil {
		return f.setVersionErr
	}
	f.version = version
	f.dirty = dirty
	return nil
}

func (f *fakeDriver) Version() (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.onVersion != nil {
		f.onVersion()
	}
	if f.versionErr != nil {
		return 0, false, f.versionErr
	}
	return f.version, f.dirty, nil
}

func (f *fakeDriver) Drop() error { return nil }

func (f *fakeDriver) state() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.dirty
}

func (f *fakeDriver) runBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.runs))
	copy(out, f.runs)
	return out
}

// fakeManager builds a Manager whose openDriver hands out one persistent fake
// per tracking table, so a test can assert the framework and service planes
// independently and re-run Up against retained state.
func fakeManager(dir string, drivers map[string]*fakeDriver) *Manager {
	return &Manager{
		dialect: "postgres",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			drv, ok := drivers[trackingTable]
			if !ok {
				return nil, "", errors.New("unexpected tracking table: " + trackingTable)
			}
			return drv, "fake", nil
		},
	}
}

// serviceManager wires a single fake as the service-plane driver.
func serviceManager(dir string, drv *fakeDriver) *Manager {
	return fakeManager(dir, map[string]*fakeDriver{serviceTrackingTbl: drv})
}

// writeFiles materializes migration files (name → body) in a fresh temp dir.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// --- Status -----------------------------------------------------------------

func TestStatus_NoMigrationsApplied(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	m := serviceManager(dir, newFakeDriver())

	v, dirty, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if v != 0 || dirty {
		t.Errorf("nil version must map to (0, false), got (%d, %v)", v, dirty)
	}
}

func TestStatus_ReportsVersionAndDirty(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.version = 7
	drv.dirty = true
	m := serviceManager(dir, drv)

	v, dirty, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if v != 7 || !dirty {
		t.Errorf("expected (7, true), got (%d, %v)", v, dirty)
	}
}

func TestStatus_DriverVersionError(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.versionErr = errors.New("boom")
	m := serviceManager(dir, drv)

	_, _, err := m.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration[service]: version") {
		t.Fatalf("expected wrapped version error, got: %v", err)
	}
}

func TestStatus_MissingDirFailsToOpenSource(t *testing.T) {
	m := serviceManager(filepath.Join(t.TempDir(), "nope"), newFakeDriver())

	_, _, err := m.Status(context.Background())
	if err == nil {
		t.Fatal("expected an error when the service migration dir does not exist")
	}
	if !strings.Contains(err.Error(), "service file source") {
		t.Errorf("error must come from serviceSource, got: %v", err)
	}
}

func TestStatus_OpenDriverError(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	m := &Manager{
		dialect: "postgres",
		dir:     dir,
		openDriver: func(string) (database.Driver, string, error) {
			return nil, "", errors.New("no database")
		},
	}

	_, _, err := m.Status(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("expected the openDriver error to surface, got: %v", err)
	}
}

// --- Force ------------------------------------------------------------------

func TestForce_ResetsPointerClean(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.version = 3
	drv.dirty = true
	m := serviceManager(dir, drv)

	if err := m.Force(context.Background(), 2); err != nil {
		t.Fatalf("Force: %v", err)
	}
	v, dirty := drv.state()
	if v != 2 || dirty {
		t.Errorf("Force must set (2, clean), got (%d, %v)", v, dirty)
	}
	if len(drv.runBodies()) != 0 {
		t.Error("Force must not run any SQL")
	}
}

func TestForce_InvalidVersion(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	m := serviceManager(dir, newFakeDriver())

	err := m.Force(context.Background(), -2)
	if err == nil || !strings.Contains(err.Error(), "force -2") {
		t.Fatalf("expected wrapped invalid-version error, got: %v", err)
	}
}

func TestForce_SetVersionError(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.setVersionErr = errors.New("disk on fire")
	m := serviceManager(dir, drv)

	err := m.Force(context.Background(), 5)
	if err == nil || !strings.Contains(err.Error(), "force 5") {
		t.Fatalf("expected wrapped force error, got: %v", err)
	}
}

func TestForce_OpenServiceError(t *testing.T) {
	m := serviceManager(filepath.Join(t.TempDir(), "nope"), newFakeDriver())
	if err := m.Force(context.Background(), 1); err == nil {
		t.Fatal("expected an error when the service source cannot open")
	}
}

// --- Down -------------------------------------------------------------------

func TestDown_RejectsNonPositiveSteps(t *testing.T) {
	// openDriver is nil on purpose: the guard must reject before any driver use.
	m := &Manager{dialect: "postgres", dir: t.TempDir()}
	for _, steps := range []int{0, -3} {
		err := m.Down(context.Background(), steps)
		if err == nil || !strings.Contains(err.Error(), "steps must be > 0") {
			t.Errorf("steps=%d: expected guard error, got: %v", steps, err)
		}
	}
}

func TestDown_RevertsSteps(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_first.up.sql":    "-- up 2",
		"0002_first.down.sql":  "-- down 2",
		"0003_second.up.sql":   "-- up 3",
		"0003_second.down.sql": "-- down 3",
	})
	drv := newFakeDriver()
	drv.version = 3
	m := serviceManager(dir, drv)

	if err := m.Down(context.Background(), 1); err != nil {
		t.Fatalf("Down: %v", err)
	}
	v, dirty := drv.state()
	if v != 2 || dirty {
		t.Errorf("expected version pointer (2, clean), got (%d, %v)", v, dirty)
	}
	runs := drv.runBodies()
	if len(runs) != 1 || !strings.Contains(runs[0], "-- down 3") {
		t.Errorf("expected exactly the 0003 down body to run, got: %q", runs)
	}
}

func TestDown_ErrorFromNilVersion(t *testing.T) {
	// Stepping down with nothing applied is a real error (not ErrNoChange).
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	m := serviceManager(dir, newFakeDriver())

	err := m.Down(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "down 1 steps") {
		t.Fatalf("expected wrapped down error, got: %v", err)
	}
}

func TestDown_OpenServiceError(t *testing.T) {
	m := serviceManager(filepath.Join(t.TempDir(), "nope"), newFakeDriver())
	if err := m.Down(context.Background(), 1); err == nil {
		t.Fatal("expected an error when the service source cannot open")
	}
}

// --- Up ---------------------------------------------------------------------

func TestUp_AppliesFrameworkThenService(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_users.up.sql":   "CREATE TABLE users_qa (id int);",
		"0002_users.down.sql": "DROP TABLE users_qa;",
	})
	fw := newFakeDriver()
	svc := newFakeDriver()
	m := fakeManager(dir, map[string]*fakeDriver{
		frameworkTrackingTbl: fw,
		serviceTrackingTbl:   svc,
	})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	fwV, fwDirty := fw.state()
	if fwV != 2 || fwDirty {
		t.Errorf("framework plane must land at (2, clean), got (%d, %v)", fwV, fwDirty)
	}
	if runs := fw.runBodies(); len(runs) != 2 ||
		!strings.Contains(runs[0], "OmniCore framework control plane") ||
		!strings.Contains(runs[1], "blue-green view rebuild slots") {
		t.Errorf("framework must run the embedded 0001 then 0002 migrations, got %d run(s)", len(runs))
	}

	svcV, svcDirty := svc.state()
	if svcV != 2 || svcDirty {
		t.Errorf("service plane must land at (2, clean), got (%d, %v)", svcV, svcDirty)
	}
	if runs := svc.runBodies(); len(runs) != 1 || !strings.Contains(runs[0], "CREATE TABLE users_qa") {
		t.Errorf("service must run exactly the 0002 up body, got: %q", runs)
	}
}

func TestUp_NoChangeOnRerun(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_users.up.sql":   "-- up 2",
		"0002_users.down.sql": "-- down 2",
	})
	fw := newFakeDriver()
	svc := newFakeDriver()
	m := fakeManager(dir, map[string]*fakeDriver{
		frameworkTrackingTbl: fw,
		serviceTrackingTbl:   svc,
	})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("first Up: %v", err)
	}
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("second Up must absorb ErrNoChange, got: %v", err)
	}
	if got := len(fw.runBodies()); got != 2 {
		t.Errorf("framework migrations must run twice (0001+0002), ran %d times", got)
	}
	if got := len(svc.runBodies()); got != 1 {
		t.Errorf("service migration must run once, ran %d times", got)
	}
}

func TestUp_UnknownDialect(t *testing.T) {
	m := &Manager{dialect: "sqlite", dir: t.TempDir()}
	err := m.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "framework iofs (sqlite)") {
		t.Fatalf("expected framework source error for unknown dialect, got: %v", err)
	}
}

func TestUp_FrameworkOpenDriverError(t *testing.T) {
	m := &Manager{
		dialect: "postgres",
		dir:     t.TempDir(),
		openDriver: func(string) (database.Driver, string, error) {
			return nil, "", errors.New("db unreachable")
		},
	}
	err := m.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration[framework]: open") {
		t.Fatalf("expected framework open error, got: %v", err)
	}
}

func TestUp_FrameworkRunError(t *testing.T) {
	fw := newFakeDriver()
	fw.runErr = errors.New("syntax error")
	m := fakeManager(t.TempDir(), map[string]*fakeDriver{frameworkTrackingTbl: fw})

	err := m.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration[framework]: up") {
		t.Fatalf("expected framework up error, got: %v", err)
	}
	if _, dirty := fw.state(); !dirty {
		t.Error("a failed Run must leave the tracking pointer dirty")
	}
}

func TestUp_MissingServiceDirSkipsServiceStage(t *testing.T) {
	// A service with no migrations directory is an empty sequence, not an
	// error — the framework stage still applies, the service stage is
	// skipped. The drivers map deliberately has no service entry: opening
	// the service plane would fail the test with "unexpected tracking table".
	fw := newFakeDriver()
	m := fakeManager(filepath.Join(t.TempDir(), "nope"), map[string]*fakeDriver{
		frameworkTrackingTbl: fw,
	})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("missing service dir must be treated as an empty sequence, got: %v", err)
	}
	if v, _ := fw.state(); v != 2 {
		t.Errorf("framework plane must still be applied, got version %d", v)
	}
}

func TestUp_EmptyServiceDirSkipsServiceStage(t *testing.T) {
	// The freshly scaffolded state: the directory exists but carries no
	// versioned *.up.sql (only placeholders like .gitkeep). golang-migrate's
	// file source errors on an empty source, so Up must skip the service
	// stage before opening it.
	dir := writeFiles(t, map[string]string{
		".gitkeep": "",
	})
	fw := newFakeDriver()
	m := fakeManager(dir, map[string]*fakeDriver{
		frameworkTrackingTbl: fw,
	})

	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("empty service dir must be treated as an empty sequence, got: %v", err)
	}
	if v, _ := fw.state(); v != 2 {
		t.Errorf("framework plane must still be applied, got version %d", v)
	}
	if runs := fw.runBodies(); len(runs) != 2 {
		t.Errorf("framework must run exactly its embedded migrations (0001+0002), got %d run(s)", len(runs))
	}
}

func TestUp_ServiceOpenDriverError(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_users.up.sql":   "-- up",
		"0002_users.down.sql": "-- down",
	})
	fw := newFakeDriver()
	m := &Manager{
		dialect: "postgres",
		dir:     dir,
		openDriver: func(trackingTable string) (database.Driver, string, error) {
			if trackingTable == frameworkTrackingTbl {
				return fw, "fake", nil
			}
			return nil, "", errors.New("service db unreachable")
		},
	}

	err := m.Up(context.Background())
	if err == nil || !strings.Contains(err.Error(), "migration[service]: open") {
		t.Fatalf("expected service open error, got: %v", err)
	}
}

// --- Pending ----------------------------------------------------------------

func TestPending_NoneApplied(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_first.up.sql":    "-- up 2",
		"0002_first.down.sql":  "-- down 2",
		"0003_second.up.sql":   "-- up 3",
		"0003_second.down.sql": "-- down 3",
	})
	// Noise Pending must ignore: a subdirectory, a non-SQL file, and an
	// up file without a parseable version prefix.
	if err := os.Mkdir(filepath.Join(dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "abc_bad.up.sql"), []byte("-- bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	drv := newFakeDriver()
	// Materialize a duplicate-version file AFTER Status opens the source
	// (golang-migrate's file source rejects duplicates at open time), so the
	// dedup path in Pending is genuinely exercised.
	drv.onVersion = func() {
		_ = os.WriteFile(filepath.Join(dir, "0003_dup.up.sql"), []byte("-- dup 3"), 0o644)
	}
	m := serviceManager(dir, drv)

	got, err := m.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	want := []uint{2, 3}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("expected %v, got %v", want, got)
	}
}

func TestPending_PartiallyApplied(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_first.up.sql":    "-- up 2",
		"0002_first.down.sql":  "-- down 2",
		"0003_second.up.sql":   "-- up 3",
		"0003_second.down.sql": "-- down 3",
	})
	drv := newFakeDriver()
	drv.version = 2
	m := serviceManager(dir, drv)

	got, err := m.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 1 || got[0] != 3 {
		t.Errorf("expected [3], got %v", got)
	}
}

func TestPending_AllApplied(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_first.up.sql":   "-- up 2",
		"0002_first.down.sql": "-- down 2",
	})
	drv := newFakeDriver()
	drv.version = 2
	m := serviceManager(dir, drv)

	got, err := m.Pending(context.Background())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no pending migrations, got %v", got)
	}
}

func TestPending_StatusErrorPropagates(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.versionErr = errors.New("boom")
	m := serviceManager(dir, drv)

	if _, err := m.Pending(context.Background()); err == nil {
		t.Fatal("expected the Status error to propagate")
	}
}

func TestPending_DirRemovedAfterStatus(t *testing.T) {
	// The documented "missing directory == no pending migrations" branch. The
	// source must exist when Status opens it, so the fake removes the dir from
	// inside Version() — after the open, before Pending's os.ReadDir.
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.onVersion = func() { _ = os.RemoveAll(dir) }
	m := serviceManager(dir, drv)

	got, err := m.Pending(context.Background())
	if err != nil {
		t.Fatalf("a missing dir must be treated as empty, got: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no pending migrations, got %v", got)
	}
}

func TestPending_UnreadableDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := writeFiles(t, map[string]string{
		"0002_init.up.sql":   "-- up",
		"0002_init.down.sql": "-- down",
	})
	drv := newFakeDriver()
	drv.onVersion = func() { _ = os.Chmod(dir, 0o000) }
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	m := serviceManager(dir, drv)

	_, err := m.Pending(context.Background())
	if err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("expected a read-dir error, got: %v", err)
	}
}

// --- ValidateDownExists (remaining branch) -----------------------------------

func TestValidateDownExists_UnreadableDirError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	m := &Manager{dialect: "postgres", dir: dir}
	err := m.ValidateDownExists()
	if err == nil || !strings.Contains(err.Error(), "read dir") {
		t.Fatalf("expected a read-dir error, got: %v", err)
	}
}
