//go:build postgres

package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// causeText flattens the cause messages carried by the InfrastructureError's
// notification contexts — ValidateDownExists stores the offending filenames in
// each message's Err, not in the generic top-level Error() string.
func causeText(t *testing.T, err error) string {
	t.Helper()
	nc, ok := err.(interface {
		NotificationContexts() []*domain.NotificationContext
	})
	if !ok {
		t.Fatalf("error is not a NotificationCarrier: %T", err)
	}
	var b strings.Builder
	for _, c := range nc.NotificationContexts() {
		for _, m := range c.Messages() {
			if m.Err != nil {
				b.WriteString(m.Err.Error())
				b.WriteString(" ")
			}
		}
	}
	return b.String()
}

// writeMigrationFiles materializes the given filenames (empty SQL bodies) in a
// fresh temp dir and returns a Manager pointed at it. The pool is nil because
// ValidateDownExists only reads the directory.
func managerWithFiles(t *testing.T, names ...string) *Manager {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("-- noop\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return NewPostgres("", dir)
}

func TestValidateDownExists_MissingDirIsOK(t *testing.T) {
	m := NewPostgres("", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := m.ValidateDownExists(); err != nil {
		t.Fatalf("missing dir must be a no-op, got: %v", err)
	}
}

func TestValidateDownExists_WellFormedPair(t *testing.T) {
	m := managerWithFiles(t, "0002_init.up.sql", "0002_init.down.sql")
	if err := m.ValidateDownExists(); err != nil {
		t.Fatalf("a complete pair must validate, got: %v", err)
	}
}

func TestValidateDownExists_MissingDown(t *testing.T) {
	m := managerWithFiles(t, "0002_init.up.sql")
	err := m.ValidateDownExists()
	if err == nil {
		t.Fatal("expected error for missing .down.sql")
	}
	if !strings.Contains(causeText(t, err), "0002_init.up.sql") {
		t.Errorf("error must name the orphaned up file, got: %v", causeText(t, err))
	}
}

func TestValidateDownExists_IgnoresSubdirectories(t *testing.T) {
	m := managerWithFiles(t, "0002_init.up.sql", "0002_init.down.sql")
	if err := os.Mkdir(filepath.Join(m.dir, "archive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateDownExists(); err != nil {
		t.Fatalf("subdirectories must be skipped, got: %v", err)
	}
}

func TestValidateDownExists_MalformedFilename_NoVersionPrefix(t *testing.T) {
	m := managerWithFiles(t, "init.up.sql", "init.down.sql")
	err := m.ValidateDownExists()
	if err == nil {
		t.Fatal("expected error for a filename without a parseable version prefix")
	}
	if !strings.Contains(causeText(t, err), "init.up.sql") {
		t.Errorf("error must name the malformed file, got: %v", causeText(t, err))
	}
}

func TestValidateDownExists_MalformedFilename_NonNumericPrefix(t *testing.T) {
	m := managerWithFiles(t, "v2_init.up.sql", "v2_init.down.sql")
	err := m.ValidateDownExists()
	if err == nil {
		t.Fatal("expected error for a non-numeric version prefix")
	}
	if !strings.Contains(causeText(t, err), "v2_init.up.sql") {
		t.Errorf("error must name the malformed file, got: %v", causeText(t, err))
	}
}
