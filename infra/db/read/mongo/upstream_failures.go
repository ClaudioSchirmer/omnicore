package mongo

import (
	"context"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db"
)

// UpstreamFailureStage is the closed set of values the stage column on
// omnicore_upstream_failures carries. Mirrors the three failure points of
// UpstreamSubscriber.ripple.
type UpstreamFailureStage string

const (
	// UpstreamFailureStageDiscover indicates MongoDB.FindIDsByField failed
	// — the subscriber could not enumerate the local view docs whose join
	// field references the changed upstream id. local_id is "" on this
	// stage because the discovery itself failed.
	UpstreamFailureStageDiscover UpstreamFailureStage = "discover"

	// UpstreamFailureStageCompose indicates Composer.Compose failed for a
	// specific local id. Other local ids of the same upstream entity may
	// have succeeded; the failure is per-doc.
	UpstreamFailureStageCompose UpstreamFailureStage = "compose"

	// UpstreamFailureStageUpsert indicates MongoDB.Upsert of the recomposed
	// doc failed. Same per-doc shape as compose.
	UpstreamFailureStageUpsert UpstreamFailureStage = "upsert"
)

// UpstreamFailureRecord mirrors one row of omnicore_upstream_failures.
// Used as input to RecordUpstreamFailure (the auto fields — id, attempt,
// first_seen_at, last_attempt_at, resolved_at — are managed by the SQL)
// and as the shape returned by ListPendingUpstreamFailures.
type UpstreamFailureRecord struct {
	ID                int64
	SubscriptionTopic string
	ViewName          string
	UpstreamID        string
	LocalID           string // empty on UpstreamFailureStageDiscover
	Stage             UpstreamFailureStage
	Error             string
	Attempt           int
	FirstSeenAt       time.Time
	LastAttemptAt     time.Time
}

// The bookkeeping SQL is dialect-neutral apart from two engine-specific bits,
// both supplied by the db.Dialect: the upsert clause (RecordUpstreamFailure builds
// it via db.Dialect.BuildUpsert — the only structurally divergent statement) and
// the positional placeholder on the filtered SELECT/UPDATE. The identifiers are
// framework-controlled and safe bare on both engines, so the SELECT/UPDATE keep
// a plain shape and only render the placeholder per dialect.
const (
	upstreamFailureTable = "omnicore_upstream_failures"

	selectUpstreamFailures = `SELECT id, subscription_topic, view_name, upstream_id, local_id, stage, error,
       attempt, first_seen_at, last_attempt_at
FROM omnicore_upstream_failures
WHERE resolved_at IS NULL`

	orderUpstreamFailures = `
ORDER BY last_attempt_at ASC`
)

var upstreamFailureInsertCols = []string{"subscription_topic", "view_name", "upstream_id", "local_id", "stage", "error"}

var upstreamFailureConflictCols = []string{"subscription_topic", "view_name", "upstream_id", "local_id", "stage"}

// RecordUpstreamFailure upserts one failure row: insert on first sighting
// of the (subscription_topic, view_name, upstream_id, local_id, stage)
// natural key, increment attempt + refresh last_attempt_at + overwrite
// error on repeat. A previously-resolved row is reopened (resolved_at →
// NULL) — the same coordinate failing again after a manual fix would
// otherwise leave a stale "resolved" marker.
//
// Best-effort: the framework's failure-isolation contract (Kafka offset
// must advance) is preserved by treating any PG error as informational.
// Callers log + carry on; they never block the consumer.
func RecordUpstreamFailure(ctx context.Context, q db.Querier, d db.Dialect, rec UpstreamFailureRecord) error {
	if rec.SubscriptionTopic == "" {
		return fmt.Errorf("record upstream failure: subscription_topic required")
	}
	if rec.ViewName == "" {
		return fmt.Errorf("record upstream failure: view_name required")
	}
	if rec.UpstreamID == "" {
		return fmt.Errorf("record upstream failure: upstream_id required")
	}
	if rec.Stage != UpstreamFailureStageDiscover &&
		rec.Stage != UpstreamFailureStageCompose &&
		rec.Stage != UpstreamFailureStageUpsert {
		return fmt.Errorf("record upstream failure: stage %q invalid (want discover|compose|upsert)", rec.Stage)
	}
	// Upsert: insert on first sighting of the natural key, else increment
	// attempt + refresh error/last_attempt + reopen resolved_at.
	sql := d.BuildUpsert(upstreamFailureTable, upstreamFailureInsertCols, upstreamFailureConflictCols, []db.UpsertSet{
		{Col: "error", Mode: db.UpsertSetNew},
		{Col: "attempt", Mode: db.UpsertSetExpr, Expr: "attempt + 1"},
		{Col: "last_attempt_at", Mode: db.UpsertSetExpr, Expr: "NOW()"},
		{Col: "resolved_at", Mode: db.UpsertSetExpr, Expr: "NULL"},
	})
	if err := q.Exec(ctx, sql,
		rec.SubscriptionTopic,
		rec.ViewName,
		rec.UpstreamID,
		rec.LocalID,
		string(rec.Stage),
		rec.Error,
	); err != nil {
		return fmt.Errorf("record upstream failure: %w", err)
	}
	return nil
}

// ResolveUpstreamFailures marks every pending row for (subscription,
// view, upstream_id) as resolved (sets resolved_at = NOW()). Called from
// UpstreamSubscriber.ripple after a recompose pass for the (view,
// upstream_id) pair completed without errors — the natural signal that
// any prior failure under that coordinate is no longer reproducing.
// Idempotent: returns nil + zero rows affected when nothing is pending.
//
// Same best-effort semantics as RecordUpstreamFailure.
func ResolveUpstreamFailures(ctx context.Context, q db.Querier, d db.Dialect, subscriptionTopic, viewName, upstreamID string) error {
	if subscriptionTopic == "" || viewName == "" || upstreamID == "" {
		return fmt.Errorf("resolve upstream failures: subscription_topic, view_name, upstream_id are all required")
	}
	sql := "UPDATE " + upstreamFailureTable + " SET resolved_at = NOW() WHERE" +
		" subscription_topic = " + d.Placeholder(1) +
		" AND view_name = " + d.Placeholder(2) +
		" AND upstream_id = " + d.Placeholder(3) +
		" AND resolved_at IS NULL"
	if err := q.Exec(ctx, sql, subscriptionTopic, viewName, upstreamID); err != nil {
		return fmt.Errorf("resolve upstream failures: %w", err)
	}
	return nil
}

// ListPendingUpstreamFailures returns every row with resolved_at IS NULL
// ordered by last_attempt_at ascending (oldest pending first — the most
// likely to be a real stuck failure rather than a transient retry).
// Consumed by the omnicore-admin list-failures CLI and by ad-hoc
// operator queries.
//
// No limit / pagination today — the table is expected to stay small under
// healthy operation; if a service accumulates thousands of pending rows,
// that itself is the operational signal.
func ListPendingUpstreamFailures(ctx context.Context, q db.Querier) ([]UpstreamFailureRecord, error) {
	return scanPendingUpstreamFailures(ctx, q, selectUpstreamFailures+orderUpstreamFailures)
}

// ListPendingUpstreamFailuresByTopic returns the subset of pending rows
// whose subscription_topic equals the supplied value. Used by
// UpstreamSubscriber.RetryPendingFailures so each subscriber only acts
// on its own slice — multi-subscription services do not interfere.
func ListPendingUpstreamFailuresByTopic(ctx context.Context, q db.Querier, d db.Dialect, subscriptionTopic string) ([]UpstreamFailureRecord, error) {
	if subscriptionTopic == "" {
		return nil, fmt.Errorf("list pending upstream failures by topic: subscription_topic required")
	}
	sql := selectUpstreamFailures + " AND subscription_topic = " + d.Placeholder(1) + orderUpstreamFailures
	return scanPendingUpstreamFailures(ctx, q, sql, subscriptionTopic)
}

// scanPendingUpstreamFailures is the shared row decoder used by both
// public List* variants. Centralizes the column order so a future schema
// extension only updates the SELECT + the Scan in one place.
func scanPendingUpstreamFailures(ctx context.Context, q db.Querier, query string, args ...any) ([]UpstreamFailureRecord, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending upstream failures: %w", err)
	}
	defer rows.Close()
	var out []UpstreamFailureRecord
	for rows.Next() {
		var r UpstreamFailureRecord
		var stage string
		if err := rows.Scan(
			&r.ID,
			&r.SubscriptionTopic,
			&r.ViewName,
			&r.UpstreamID,
			&r.LocalID,
			&stage,
			&r.Error,
			&r.Attempt,
			&r.FirstSeenAt,
			&r.LastAttemptAt,
		); err != nil {
			return nil, fmt.Errorf("list pending upstream failures: scan: %w", err)
		}
		r.Stage = UpstreamFailureStage(stage)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending upstream failures: rows: %w", err)
	}
	return out, nil
}
