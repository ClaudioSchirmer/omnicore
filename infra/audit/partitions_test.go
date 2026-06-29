//go:build postgres

package audit

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEnsureFuturePartitions_NilPoolIsConfigError(t *testing.T) {
	// A nil pool is a configuration error — audit cannot create partitions
	// without database access. The guard returns before touching any DB.
	err := EnsureFuturePartitions(context.Background(), nil, 3)
	if err == nil || !strings.Contains(err.Error(), "non-nil pool") {
		t.Errorf("expected non-nil-pool error, got %v", err)
	}
}

func TestBuildPartitionStatements_ZeroOrNegativeReturnsNil(t *testing.T) {
	for _, n := range []int{0, -1, -100} {
		if got := BuildPartitionStatements(time.Now(), n); got != nil {
			t.Errorf("n=%d → %d statements, want nil", n, len(got))
		}
	}
}

func TestBuildPartitionStatements_NamesAreDeterministic(t *testing.T) {
	now := time.Date(2026, time.June, 12, 0, 0, 0, 0, time.UTC)
	got := BuildPartitionStatements(now, 3)
	want := []string{
		"audit_events_2026_06",
		"audit_events_2026_07",
		"audit_events_2026_08",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d statements, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Name != w {
			t.Errorf("[%d] Name = %q, want %q", i, got[i].Name, w)
		}
	}
}

func TestBuildPartitionStatements_BoundariesAreContiguousMonthlyRanges(t *testing.T) {
	now := time.Date(2026, time.June, 12, 0, 0, 0, 0, time.UTC)
	stmts := BuildPartitionStatements(now, 3)

	// Inspect each statement's SQL — the test confirms the boundary literals,
	// not the SQL idiom (which is exercised by the integration suite).
	checks := []struct {
		idx          int
		wantFromIncl string
		wantToExcl   string
	}{
		{0, "2026-06-01", "2026-07-01"},
		{1, "2026-07-01", "2026-08-01"},
		{2, "2026-08-01", "2026-09-01"},
	}
	for _, c := range checks {
		sql := stmts[c.idx].SQL
		if !strings.Contains(sql, "FROM ('"+c.wantFromIncl+"')") {
			t.Errorf("stmt[%d] missing FROM '%s' in %q", c.idx, c.wantFromIncl, sql)
		}
		if !strings.Contains(sql, "TO ('"+c.wantToExcl+"')") {
			t.Errorf("stmt[%d] missing TO '%s' in %q", c.idx, c.wantToExcl, sql)
		}
	}
}

func TestBuildPartitionStatements_CrossesYearBoundary(t *testing.T) {
	// November + 4 months → Nov, Dec, Jan(next year), Feb.
	now := time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC)
	stmts := BuildPartitionStatements(now, 4)
	want := []string{
		"audit_events_2026_11",
		"audit_events_2026_12",
		"audit_events_2027_01",
		"audit_events_2027_02",
	}
	for i, w := range want {
		if stmts[i].Name != w {
			t.Errorf("[%d] Name = %q, want %q", i, stmts[i].Name, w)
		}
	}
}

func TestBuildPartitionStatements_IdempotencyByName(t *testing.T) {
	// Same `now` + same n → byte-identical output. Caller can replay across
	// boots and the catalog ends up with the same partition set.
	now := time.Date(2026, time.June, 12, 0, 0, 0, 0, time.UTC)
	first := BuildPartitionStatements(now, 5)
	second := BuildPartitionStatements(now, 5)
	if len(first) != len(second) {
		t.Fatalf("len mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("[%d] mismatch:\n  first=%+v\n  second=%+v", i, first[i], second[i])
		}
	}
}

func TestBuildPartitionStatements_UsesCreateTableIfNotExists(t *testing.T) {
	// The "IF NOT EXISTS" clause is the idempotency contract — every
	// statement must carry it so EnsureFuturePartitions can be replayed
	// safely across boots.
	stmts := BuildPartitionStatements(time.Now(), 1)
	if !strings.Contains(stmts[0].SQL, "CREATE TABLE IF NOT EXISTS") {
		t.Errorf("SQL missing IF NOT EXISTS clause: %q", stmts[0].SQL)
	}
}
