package integration

import (
	"context"
	"fmt"
	"time"

	"github.com/ClaudioSchirmer/omnicore/domain"
	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
	"github.com/google/uuid"
)

// IntegrationFailureRecord mirrors one row of
// omnicore_integration_failures. Mirrors the shape UpstreamFailureRecord
// already follows so operators have a single mental model across both
// failure registries. RawPayload is the JSON bytes verbatim — preserved
// so an operator-driven retry replays the exact payload the receiver saw.
type IntegrationFailureRecord struct {
	// ID is the surrogate row id — the canonical uuid string (a UUID v7 on
	// every dialect; the scan restores BINARY(16) to canonical text).
	ID            string
	ConsumerGroup string
	SourceKey     string
	EventKey      string
	EventID       uuid.UUID
	RawPayload    []byte
	Error         string
	Attempt       int
	FirstSeenAt   time.Time
	LastAttemptAt time.Time
}

// IntegrationProcessedRecord captures the columns on
// omnicore_integration_processed. Surfaced for tests asserting the
// dedup row landed; production code interacts via the helpers below.
type IntegrationProcessedRecord struct {
	EventID       uuid.UUID
	ConsumerGroup string
	SourceKey     string
	EventKey      string
	Topic         string
	EventType     string
	ProcessedAt   time.Time
}

// The control-plane SQL is dialect-neutral except for two engine-specific bits,
// both supplied by core.Dialect: the upsert clause (RecordIntegrationFailure /
// MarkProcessed build it via Dialect.BuildUpsert — the structurally divergent
// statements) and the positional placeholder on the filtered SELECT/UPDATE. The
// identifiers are framework-controlled and safe bare on both engines, so the
// SELECT/UPDATE keep a plain shape and only render the placeholder per dialect.
const (
	integrationFailureTable   = "omnicore_integration_failures"
	integrationProcessedTable = "omnicore_integration_processed"

	selectIntegrationFailures = `SELECT id, consumer_group, source_key, event_key, event_id, payload,
       error, attempt, first_seen_at, last_attempt_at
  FROM omnicore_integration_failures
 WHERE resolved_at IS NULL`

	orderIntegrationFailures = `
 ORDER BY last_attempt_at ASC`
)

var (
	integrationFailureInsertCols   = []string{"id", "consumer_group", "source_key", "event_key", "event_id", "payload", "error"}
	integrationFailureConflictCols = []string{"consumer_group", "source_key", "event_key", "event_id"}

	integrationProcessedInsertCols   = []string{"id", "event_id", "consumer_group", "source_key", "event_key", "topic", "event_type"}
	integrationProcessedConflictCols = []string{"event_id", "consumer_group"}
)

// newControlPlaneID mints the framework-standard surrogate id for a
// control-plane row: a UUID v7 generated in Go, returned as a domain.ID so the
// caller binds it through Dialect.EncodeArg into the dialect's native id form
// (uuid text on PG, BINARY(16) elsewhere) — the same id discipline as every
// domain table; no AUTO_INCREMENT/IDENTITY/DB default anywhere.
func newControlPlaneID() (domain.ID, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return domain.ID{}, fmt.Errorf("integration: uuid v7: %w", err)
	}
	return domain.NewID(u.String()), nil
}

// RecordIntegrationFailure upserts one failure row under the natural
// key (consumer_group, source_key, event_key, event_id). Repeats
// increment attempt + refresh last_attempt_at + overwrite error +
// reopen resolved_at to NULL — same shape RecordUpstreamFailure draws,
// for parity in operator tooling.
func RecordIntegrationFailure(ctx context.Context, q core.Querier, d core.Dialect, rec IntegrationFailureRecord) error {
	if rec.ConsumerGroup == "" || rec.SourceKey == "" || rec.EventKey == "" {
		return fmt.Errorf("record integration failure: consumer_group, source_key, event_key are required")
	}
	if rec.EventID == uuid.Nil {
		return fmt.Errorf("record integration failure: event_id is required")
	}
	payload := rec.RawPayload
	if payload == nil {
		payload = []byte("{}")
	}
	rowID, err := newControlPlaneID()
	if err != nil {
		return fmt.Errorf("record integration failure: %w", err)
	}
	sql := d.BuildUpsert(integrationFailureTable, integrationFailureInsertCols, integrationFailureConflictCols, []core.UpsertSet{
		{Col: "error", Mode: core.UpsertSetNew},
		{Col: "payload", Mode: core.UpsertSetNew},
		{Col: "attempt", Mode: core.UpsertSetExpr, Expr: "attempt + 1"},
		{Col: "last_attempt_at", Mode: core.UpsertSetExpr, Expr: d.NowExpr()},
		{Col: "resolved_at", Mode: core.UpsertSetExpr, Expr: "NULL"},
	})
	if err := q.Exec(ctx, sql,
		d.EncodeArg(rowID),
		rec.ConsumerGroup,
		rec.SourceKey,
		rec.EventKey,
		rec.EventID,
		// Text bind — the payload column is text-shaped JSON on every dialect;
		// SQL Server refuses the implicit varbinary→NVARCHAR conversion a raw
		// []byte would require.
		string(payload),
		rec.Error,
	); err != nil {
		return fmt.Errorf("record integration failure: %w", err)
	}
	return nil
}

// ResolveIntegrationFailures marks every pending failure for the given
// natural key as resolved. Called by Receiver.RetryPendingFailures
// after a successful re-dispatch; idempotent (zero affected rows is
// not an error).
func ResolveIntegrationFailures(ctx context.Context, q core.Querier, d core.Dialect, consumerGroup, sourceKey, eventKey string, eventID uuid.UUID) error {
	if consumerGroup == "" || sourceKey == "" || eventKey == "" {
		return fmt.Errorf("resolve integration failures: consumer_group, source_key, event_key are required")
	}
	if eventID == uuid.Nil {
		return fmt.Errorf("resolve integration failures: event_id is required")
	}
	sql := "UPDATE " + integrationFailureTable + " SET resolved_at = " + d.NowExpr() + " WHERE" +
		" consumer_group = " + d.Placeholder(1) +
		" AND source_key = " + d.Placeholder(2) +
		" AND event_key = " + d.Placeholder(3) +
		" AND event_id = " + d.Placeholder(4) +
		" AND resolved_at IS NULL"
	if err := q.Exec(ctx, sql, consumerGroup, sourceKey, eventKey, eventID); err != nil {
		return fmt.Errorf("resolve integration failures: %w", err)
	}
	return nil
}

// ListPendingIntegrationFailures returns every row with resolved_at
// IS NULL ordered by last_attempt_at ASC. Consumed by operator CLIs
// and by Receiver.RetryPendingFailures.
func ListPendingIntegrationFailures(ctx context.Context, q core.Querier) ([]IntegrationFailureRecord, error) {
	return scanIntegrationFailures(ctx, q, selectIntegrationFailures+orderIntegrationFailures)
}

// ListPendingIntegrationFailuresByGroup narrows the pending list to one
// consumer group. Used by Receiver.RetryPendingFailures so each receiver
// only acts on the events its own group must re-dispatch — multi-group
// services do not interfere with each other.
func ListPendingIntegrationFailuresByGroup(ctx context.Context, q core.Querier, d core.Dialect, consumerGroup string) ([]IntegrationFailureRecord, error) {
	if consumerGroup == "" {
		return nil, fmt.Errorf("list pending integration failures by group: consumer_group required")
	}
	sql := selectIntegrationFailures + " AND consumer_group = " + d.Placeholder(1) + orderIntegrationFailures
	return scanIntegrationFailures(ctx, q, sql, consumerGroup)
}

func scanIntegrationFailures(ctx context.Context, q core.Querier, query string, args ...any) ([]IntegrationFailureRecord, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list pending integration failures: %w", err)
	}
	defer rows.Close()
	var out []IntegrationFailureRecord
	for rows.Next() {
		var r IntegrationFailureRecord
		if err := rows.Scan(
			&r.ID,
			&r.ConsumerGroup,
			&r.SourceKey,
			&r.EventKey,
			&r.EventID,
			&r.RawPayload,
			&r.Error,
			&r.Attempt,
			&r.FirstSeenAt,
			&r.LastAttemptAt,
		); err != nil {
			return nil, fmt.Errorf("list pending integration failures: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending integration failures: rows: %w", err)
	}
	return out, nil
}

// IsAlreadyProcessed pre-checks the dedup table before invoking the
// handler. Returns true when the event_id was already processed by the
// consumer group — receiver skips the handler and acks Kafka. A
// concurrent processor that inserts between this check and the
// post-success INSERT is handled by the INSERT's `ON CONFLICT DO
// NOTHING` clause (sqlInsertIntegrationProcessed). The handler may run
// twice in that millisecond window — the documented at-least-once
// contract.
func IsAlreadyProcessed(ctx context.Context, q core.Querier, d core.Dialect, eventID uuid.UUID, consumerGroup string) (bool, error) {
	if eventID == uuid.Nil || consumerGroup == "" {
		return false, fmt.Errorf("is already processed: event_id and consumer_group are required")
	}
	// Query + Next instead of QueryRow + a no-rows sentinel: pgx and database/sql
	// surface "no rows" with different sentinels (pgx.ErrNoRows vs sql.ErrNoRows),
	// so presence-by-iteration keeps the dedup check engine-neutral.
	//
	// Performance: SELECT 1 filtered on exactly the UNIQUE natural key's columns
	// is an INDEX-ONLY probe on every engine (covering seek on the
	// omnicore_integration_processed_natural_key index) — the surrogate uuid PK
	// costs this read path nothing; only the insert maintains the extra index.
	sql := "SELECT 1 FROM " + integrationProcessedTable +
		" WHERE event_id = " + d.Placeholder(1) + " AND consumer_group = " + d.Placeholder(2)
	rows, err := q.Query(ctx, sql, eventID, consumerGroup)
	if err != nil {
		return false, fmt.Errorf("is already processed: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("is already processed: %w", err)
	}
	return false, nil
}

// MarkProcessed writes the dedup row post-handler-success. Uses ON
// CONFLICT DO NOTHING so concurrent processors that raced past the
// pre-check do not double-fail — the row already exists, and the
// handler-side at-least-once contract documents the race.
func MarkProcessed(ctx context.Context, q core.Querier, d core.Dialect, rec IntegrationProcessedRecord) error {
	if rec.EventID == uuid.Nil || rec.ConsumerGroup == "" {
		return fmt.Errorf("mark processed: event_id and consumer_group are required")
	}
	// Upsert with no update assignments → do-nothing on conflict (the dedup
	// row already exists — its Go-minted surrogate id is simply discarded; the
	// at-least-once race is documented).
	rowID, err := newControlPlaneID()
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	sql := d.BuildUpsert(integrationProcessedTable, integrationProcessedInsertCols, integrationProcessedConflictCols, nil)
	if err := q.Exec(ctx, sql,
		d.EncodeArg(rowID),
		rec.EventID,
		rec.ConsumerGroup,
		rec.SourceKey,
		rec.EventKey,
		rec.Topic,
		rec.EventType,
	); err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	return nil
}
