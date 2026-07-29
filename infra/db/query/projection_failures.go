package query

import (
	"context"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// The read-side's UNIFIED failure ledger — omnicore_projection_failures.
//
// Every piece of deferred read-side work lands here, discriminated by Kind:
//
//   - KindEvent: a whole projection EVENT whose in-process retry budget was
//     exhausted. Holding it would stall every healthy aggregate behind it on
//     the same partition; confirming it would be silent, permanent divergence.
//     The row carries the outbox payload, so the retry driver replays it
//     without the broker. "Advance" stops meaning "forget".
//
//   - KindRipple: one EMBED-SEGMENT refresh that failed — the pair (source,
//     dependent view) whose materialized copy could not be brought up to date.
//     The source is an upstream subscription topic, or "view:<name>" for a
//     local view (query.JoinView). NO payload is stored: a ripple replay must
//     recompose from the source's CURRENT document — replaying a stale copy is
//     exactly what the revision guards exist to prevent.
//
// One retry driver serves both kinds (the SyncEngine's parked-retry loop, the
// mongo.parkedRetry knob).
//
// The ledger mirrors LIVE STATE, not a growing log: one row per natural key
// (consumer group, kind, topic, aggregate type, aggregate id), and a newer
// failure for the same key OVERWRITES the older one — latest payload / stage /
// error win. That is sound because the projection is state-based rather than
// event-sourced; per-aggregate ordering (hash dispatch pins one aggregate to
// one worker) is what makes "newer" well defined.

const projectionFailureTable = "omnicore_projection_failures"

// ProjectionFailureKind discriminates what a ledger row defers — and therefore
// which replay the retry driver runs.
type ProjectionFailureKind string

const (
	// ProjectionFailureKindEvent is a parked projection event, replayed from
	// its stored payload.
	ProjectionFailureKindEvent ProjectionFailureKind = "event"
	// ProjectionFailureKindRipple is a failed embed-segment refresh, replayed
	// by recomposing the (source, dependent view) pair from current state.
	ProjectionFailureKindRipple ProjectionFailureKind = "ripple"
)

// ProjectionFailureStage narrows WHERE a ripple failed. Empty on event rows.
type ProjectionFailureStage string

const (
	// ProjectionFailureStageDiscover indicates the reverse scan that finds the
	// embedding documents failed — no local doc is known yet.
	ProjectionFailureStageDiscover ProjectionFailureStage = "discover"
	// ProjectionFailureStageCompose indicates composing the refreshed segment
	// failed for a discovered local doc.
	ProjectionFailureStageCompose ProjectionFailureStage = "compose"
	// ProjectionFailureStageUpsert indicates writing the refreshed segment
	// into the embedding document failed.
	ProjectionFailureStageUpsert ProjectionFailureStage = "upsert"
	// ProjectionFailureStageSignal indicates the post-write re-read of the
	// SOURCE document (which feeds the ripple) failed — the ripple never even
	// started. Without this row that loss was invisible.
	ProjectionFailureStageSignal ProjectionFailureStage = "signal"
)

// ProjectionFailureRecord mirrors one row of omnicore_projection_failures.
// Field semantics depend on Kind — see the table comments; the short form:
// event rows describe a message (Topic/AggregateType/EventType/AggregateID/
// Payload), ripple rows describe a pair (Topic = source or "view:<name>",
// AggregateType = dependent view, AggregateID = source doc id, Stage/LocalID).
type ProjectionFailureRecord struct {
	// ID is the surrogate row id — the canonical uuid string (a UUID v7 on every
	// dialect; the scan restores BINARY(16)/RAW(16) to canonical text).
	ID            string
	Kind          ProjectionFailureKind
	ConsumerGroup string
	Topic         string
	AggregateType string
	EventType     string
	AggregateID   string
	Stage         ProjectionFailureStage
	LocalID       string
	Traceparent   string
	Payload       []byte
	Error         string
	Attempt       int
	FirstSeenAt   time.Time
	LastAttemptAt time.Time
}

const selectProjectionFailures = `SELECT id, kind, consumer_group, topic, aggregate_type, event_type,
       aggregate_id, stage, local_id, traceparent, payload, error, attempt, first_seen_at, last_attempt_at
FROM omnicore_projection_failures
WHERE resolved_at IS NULL`

const orderProjectionFailures = `
ORDER BY last_attempt_at ASC`

var projectionFailureInsertCols = []string{
	"id", "kind", "consumer_group", "topic", "aggregate_type", "event_type",
	"aggregate_id", "stage", "local_id", "traceparent", "payload", "error",
}

var projectionFailureConflictCols = []string{"consumer_group", "kind", "topic", "aggregate_type", "aggregate_id"}

// RecordProjectionFailure upserts one ledger row: insert on the first sighting
// of the natural key, refresh on repeat — the newer payload/stage/error win,
// attempt increments, and a previously-resolved row reopens.
//
// Best-effort by contract: the caller has already decided to advance, so a
// ledger write failure must not block it. The caller logs and carries on — and
// the reconciliation sweep remains the backstop that does not depend on this
// table at all, which is what keeps the detection independent of the mechanism
// it protects.
func RecordProjectionFailure(ctx context.Context, q core.Querier, d core.Dialect, rec ProjectionFailureRecord) error {
	if rec.Kind != ProjectionFailureKindEvent && rec.Kind != ProjectionFailureKindRipple {
		return fmt.Errorf("record projection failure: kind must be event or ripple, got %q", rec.Kind)
	}
	if rec.ConsumerGroup == "" {
		return fmt.Errorf("record projection failure: consumer_group required")
	}
	if rec.AggregateType == "" {
		return fmt.Errorf("record projection failure: aggregate_type required")
	}
	if rec.AggregateID == "" {
		return fmt.Errorf("record projection failure: aggregate_id required")
	}
	if rec.Kind == ProjectionFailureKindEvent && len(rec.Payload) == 0 {
		return fmt.Errorf("record projection failure: an event row without a payload cannot be replayed")
	}
	if rec.Kind == ProjectionFailureKindRipple && len(rec.Payload) != 0 {
		return fmt.Errorf("record projection failure: a ripple row must not carry a payload (replay reads current state)")
	}
	rowID, err := newControlPlaneID()
	if err != nil {
		return fmt.Errorf("record projection failure: %w", err)
	}
	sql := d.BuildUpsert(projectionFailureTable, projectionFailureInsertCols, projectionFailureConflictCols, []core.UpsertSet{
		// The payload is refreshed, not kept: a newer parked event carries newer
		// state, and replaying the stale one would project backwards. Stage and
		// local_id likewise describe the LATEST failure.
		{Col: "event_type", Mode: core.UpsertSetNew},
		{Col: "stage", Mode: core.UpsertSetNew},
		{Col: "local_id", Mode: core.UpsertSetNew},
		{Col: "traceparent", Mode: core.UpsertSetNew},
		{Col: "payload", Mode: core.UpsertSetNew},
		{Col: "error", Mode: core.UpsertSetNew},
		{Col: "attempt", Mode: core.UpsertSetBump},
		{Col: "last_attempt_at", Mode: core.UpsertSetExpr, Expr: d.NowExpr()},
		{Col: "resolved_at", Mode: core.UpsertSetExpr, Expr: "NULL"},
	})
	if err := q.Exec(ctx, sql,
		d.EncodeArg(rowID),
		string(rec.Kind),
		rec.ConsumerGroup,
		rec.Topic,
		rec.AggregateType,
		nullableText(rec.EventType),
		rec.AggregateID,
		nullableText(string(rec.Stage)),
		nullableText(rec.LocalID),
		nullableText(rec.Traceparent),
		nullablePayload(rec.Payload),
		rec.Error,
	); err != nil {
		return fmt.Errorf("record projection failure: %w", err)
	}
	return nil
}

// nullableText binds "" as NULL — the nullable-text convention the unified
// columns share (and the only representable form on Oracle, which stores ''
// as NULL).
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullablePayload binds an empty payload as NULL (ripple rows store none).
func nullablePayload(p []byte) any {
	if len(p) == 0 {
		return nil
	}
	return string(p)
}

// ResolveProjectionFailure marks the pending row for one natural key as
// resolved. Called after a replay of that key completes cleanly. Idempotent:
// nothing pending means zero rows affected and no error.
func ResolveProjectionFailure(ctx context.Context, q core.Querier, d core.Dialect, consumerGroup string, kind ProjectionFailureKind, topic, aggregateType, aggregateID string) error {
	if consumerGroup == "" || aggregateType == "" || aggregateID == "" {
		return fmt.Errorf("resolve projection failure: consumer_group, aggregate_type and aggregate_id are all required")
	}
	sql := "UPDATE " + projectionFailureTable + " SET resolved_at = " + d.NowExpr() + " WHERE" +
		" consumer_group = " + d.Placeholder(1) +
		" AND kind = " + d.Placeholder(2) +
		" AND topic = " + d.Placeholder(3) +
		" AND aggregate_type = " + d.Placeholder(4) +
		" AND aggregate_id = " + d.Placeholder(5) +
		" AND resolved_at IS NULL"
	if err := q.Exec(ctx, sql, consumerGroup, string(kind), topic, aggregateType, aggregateID); err != nil {
		return fmt.Errorf("resolve projection failure: %w", err)
	}
	return nil
}

// ListPendingProjectionFailures returns every unresolved row for one consumer
// group — both kinds — oldest attempt first: the order the retry driver
// replays in, and the order an operator wants when asking "what is stuck".
func ListPendingProjectionFailures(ctx context.Context, q core.Querier, d core.Dialect, consumerGroup string) ([]ProjectionFailureRecord, error) {
	if consumerGroup == "" {
		return nil, fmt.Errorf("list pending projection failures: consumer_group required")
	}
	sql := selectProjectionFailures + " AND consumer_group = " + d.Placeholder(1) + orderProjectionFailures
	rows, err := q.Query(ctx, sql, consumerGroup)
	if err != nil {
		return nil, fmt.Errorf("list pending projection failures: %w", err)
	}
	defer rows.Close()
	var out []ProjectionFailureRecord
	for rows.Next() {
		var r ProjectionFailureRecord
		var kind string
		var eventType, stage, localID, traceparent, payload *string
		if err := rows.Scan(
			&r.ID,
			&kind,
			&r.ConsumerGroup,
			&r.Topic,
			&r.AggregateType,
			&eventType,
			&r.AggregateID,
			&stage,
			&localID,
			&traceparent,
			&payload,
			&r.Error,
			&r.Attempt,
			&r.FirstSeenAt,
			&r.LastAttemptAt,
		); err != nil {
			return nil, fmt.Errorf("list pending projection failures: scan: %w", err)
		}
		r.Kind = ProjectionFailureKind(kind)
		if eventType != nil {
			r.EventType = *eventType
		}
		if stage != nil {
			r.Stage = ProjectionFailureStage(*stage)
		}
		if localID != nil {
			r.LocalID = *localID
		}
		if traceparent != nil {
			r.Traceparent = *traceparent
		}
		if payload != nil {
			r.Payload = []byte(*payload)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending projection failures: rows: %w", err)
	}
	return out, nil
}
