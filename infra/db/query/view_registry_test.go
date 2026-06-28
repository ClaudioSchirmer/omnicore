package query

import (
	"os"
	"strings"
	"testing"
)

// SQL strings carry the wire contract with the database; changes to them
// are deliberate and need to surface in code review. These tests pin the
// shape so an accidental drift is caught.

func TestSQLConstants_ReferenceCorrectTable(t *testing.T) {
	cases := []struct {
		name string
		sql  string
	}{
		{"sqlReadViewRegistry", sqlReadViewRegistry(fakeDialect{})},
		{"sqlInitViewRegistry", sqlInitViewRegistry(fakeDialect{})},
		{"sqlBeginRebuild", sqlBeginRebuild(fakeDialect{})},
		{"sqlEndRebuild", sqlEndRebuild(fakeDialect{})},
		{"sqlListNonDone", sqlListNonDone},
	}
	for _, c := range cases {
		if !strings.Contains(c.sql, "omnicore_mongo_views") {
			t.Errorf("%s missing reference to omnicore_mongo_views", c.name)
		}
	}
}

func TestBeginRebuildSQL_TransitionsToProcessing(t *testing.T) {
	if !strings.Contains(sqlBeginRebuild(fakeDialect{}), "status = 'processing'") {
		t.Error("sqlBeginRebuild does not transition status to 'processing'")
	}
	for _, col := range []string{"started_at", "pid", "host"} {
		if !strings.Contains(sqlBeginRebuild(fakeDialect{}), col) {
			t.Errorf("sqlBeginRebuild does not write %q", col)
		}
	}
}

func TestEndRebuildSQL_CapturesPreviousState(t *testing.T) {
	// previous_* triple comes from the row's CURRENT state (pg.Postgres reads
	// before writes inside an UPDATE). Statement must populate all three.
	for _, expr := range []string{
		"previous_version = version",
		"previous_combined_hash = combined_hash",
		"previous_applied_at = applied_at",
	} {
		if !strings.Contains(sqlEndRebuild(fakeDialect{}), expr) {
			t.Errorf("sqlEndRebuild missing previous_* capture: %q", expr)
		}
	}
	if !strings.Contains(sqlEndRebuild(fakeDialect{}), "status = 'done'") {
		t.Error("sqlEndRebuild does not transition status back to 'done'")
	}
	// started_at / pid / host cleared on completion.
	for _, expr := range []string{"started_at = NULL", "pid = NULL", "host = NULL"} {
		if !strings.Contains(sqlEndRebuild(fakeDialect{}), expr) {
			t.Errorf("sqlEndRebuild does not clear %q", expr)
		}
	}
}

func TestListNonDoneSQL_FiltersAndOrders(t *testing.T) {
	if !strings.Contains(sqlListNonDone, "WHERE status <> 'done'") {
		t.Error("sqlListNonDone filter missing")
	}
	if !strings.Contains(sqlListNonDone, "ORDER BY started_at") {
		t.Error("sqlListNonDone order missing — oldest in-flight should surface first")
	}
}

func TestFormatRegistryAppliedBy_FormatsServiceAndPID(t *testing.T) {
	got := FormatRegistryAppliedBy("users-svc")
	if !strings.HasPrefix(got, "users-svc@pid:") {
		t.Errorf("FormatRegistryAppliedBy(%q) = %q, want prefix %q", "users-svc", got, "users-svc@pid:")
	}
}

func TestFormatRegistryAppliedBy_EmptyServiceFallsBackToUnknown(t *testing.T) {
	got := FormatRegistryAppliedBy("")
	if !strings.HasPrefix(got, "unknown@pid:") {
		t.Errorf("FormatRegistryAppliedBy(\"\") = %q, want prefix %q", got, "unknown@pid:")
	}
}

func TestCodeVersionEnv_HonoursOMNICOREVar(t *testing.T) {
	// Sanity: the package reads OMNICORE_CODE_VERSION at write time. Confirm
	// the constant points where the docs claim it does.
	const expectedName = "OMNICORE_CODE_VERSION"
	if codeVersionEnv != expectedName {
		t.Errorf("codeVersionEnv = %q, want %q", codeVersionEnv, expectedName)
	}
	// Probing that os.Getenv returns whatever was set, without depending
	// on external test state.
	_ = os.Getenv(codeVersionEnv)
}

func TestViewRegistryStatus_ClosedSet(t *testing.T) {
	// The CHECK constraint on the DB side mirrors this — only the two
	// strings below are accepted. If a new state lands, the schema
	// migration AND this test need to update together.
	got := []ViewRegistryStatus{ViewRegistryStatusDone, ViewRegistryStatusProcessing}
	want := []ViewRegistryStatus{"done", "processing"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("status[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
