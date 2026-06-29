//go:build postgres

package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionStmt names a monthly partition of audit_events and carries the
// idempotent SQL that creates it (CREATE TABLE IF NOT EXISTS — safe to
// re-execute every boot). The bounds are inclusive of the lower month
// boundary and exclusive of the next month, matching the table's
// PARTITION BY RANGE (created_at) declaration.
type PartitionStmt struct {
	Name string
	SQL  string
}

// BuildPartitionStatements returns the SQL statements that ensure the next
// n monthly partitions of audit_events exist, starting from the month of
// `now` (i.e. n=3 from a June timestamp covers June + July + August).
//
// Names follow the deterministic shape `audit_events_YYYY_MM`. The same
// inputs always produce the same output so callers can pre-compute the
// expected set in tests.
func BuildPartitionStatements(now time.Time, n int) []PartitionStmt {
	if n <= 0 {
		return nil
	}
	now = now.UTC()
	out := make([]PartitionStmt, 0, n)
	for i := 0; i < n; i++ {
		// time.Date normalizes month > 12 into the next year, so passing
		// now.Month() + i lands correctly across year boundaries.
		start := time.Date(now.Year(), now.Month()+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		name := fmt.Sprintf("audit_events_%04d_%02d", start.Year(), int(start.Month()))
		stmt := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF audit_events FOR VALUES FROM ('%s') TO ('%s')",
			name,
			start.Format("2006-01-02"),
			end.Format("2006-01-02"),
		)
		out = append(out, PartitionStmt{Name: name, SQL: stmt})
	}
	return out
}

// EnsureFuturePartitions creates the next n monthly partitions of
// audit_events on the given pool. Idempotent — each statement uses
// CREATE TABLE IF NOT EXISTS so re-runs across boots cost a no-op
// catalog lookup per partition. Called from bootstrap.Run after
// migrations and before serving HTTP.
//
// A nil pool is a configuration error (audit cannot create partitions
// without database access) and returns a descriptive error.
func EnsureFuturePartitions(ctx context.Context, pool *pgxpool.Pool, n int) error {
	if pool == nil {
		return fmt.Errorf("audit: EnsureFuturePartitions requires non-nil pool")
	}
	stmts := BuildPartitionStatements(time.Now(), n)
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s.SQL); err != nil {
			return fmt.Errorf("audit: ensure partition %s: %w", s.Name, err)
		}
	}
	return nil
}
