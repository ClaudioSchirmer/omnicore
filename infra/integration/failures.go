package integration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// IntegrationFailureRecord mirrors one row of
// omnicore_integration_failures. Mirrors the shape UpstreamFailureRecord
// already follows so operators have a single mental model across both
// failure registries. RawPayload is the JSON bytes verbatim — preserved
// so an operator-driven retry replays the exact payload the receiver saw.
type IntegrationFailureRecord struct {
	ID            int64
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

// pgExec is the minimal interface the failure / processed helpers
// require. *pgxpool.Pool, *pgxpool.Conn, and pgx.Tx all satisfy it, so
// the same helpers work against the framework's own pool and against
// any test scaffold that holds a pinned connection.
type pgExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const (
	sqlRecordIntegrationFailure = `
INSERT INTO omnicore_integration_failures
  (consumer_group, source_key, event_key, event_id, payload, error)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (consumer_group, source_key, event_key, event_id)
DO UPDATE SET
  error           = EXCLUDED.error,
  payload         = EXCLUDED.payload,
  attempt         = omnicore_integration_failures.attempt + 1,
  last_attempt_at = NOW(),
  resolved_at     = NULL`

	sqlResolveIntegrationFailures = `
UPDATE omnicore_integration_failures
   SET resolved_at = NOW()
 WHERE consumer_group = $1
   AND source_key     = $2
   AND event_key      = $3
   AND event_id       = $4
   AND resolved_at IS NULL`

	sqlListPendingIntegrationFailures = `
SELECT id, consumer_group, source_key, event_key, event_id, payload,
       error, attempt, first_seen_at, last_attempt_at
  FROM omnicore_integration_failures
 WHERE resolved_at IS NULL
 ORDER BY last_attempt_at ASC`

	sqlListPendingIntegrationFailuresByGroup = `
SELECT id, consumer_group, source_key, event_key, event_id, payload,
       error, attempt, first_seen_at, last_attempt_at
  FROM omnicore_integration_failures
 WHERE resolved_at IS NULL
   AND consumer_group = $1
 ORDER BY last_attempt_at ASC`

	sqlPreCheckIntegrationProcessed = `
SELECT 1
  FROM omnicore_integration_processed
 WHERE event_id       = $1
   AND consumer_group = $2`

	sqlInsertIntegrationProcessed = `
INSERT INTO omnicore_integration_processed
  (event_id, consumer_group, source_key, event_key, topic, event_type)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (event_id, consumer_group) DO NOTHING`
)

// RecordIntegrationFailure upserts one failure row under the natural
// key (consumer_group, source_key, event_key, event_id). Repeats
// increment attempt + refresh last_attempt_at + overwrite error +
// reopen resolved_at to NULL — same shape RecordUpstreamFailure draws,
// for parity in operator tooling.
func RecordIntegrationFailure(ctx context.Context, exec pgExec, rec IntegrationFailureRecord) error {
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
	_, err := exec.Exec(ctx, sqlRecordIntegrationFailure,
		rec.ConsumerGroup,
		rec.SourceKey,
		rec.EventKey,
		rec.EventID,
		payload,
		rec.Error,
	)
	if err != nil {
		return fmt.Errorf("record integration failure: %w", err)
	}
	return nil
}

// ResolveIntegrationFailures marks every pending failure for the given
// natural key as resolved. Called by Receiver.RetryPendingFailures
// after a successful re-dispatch; idempotent (zero affected rows is
// not an error).
func ResolveIntegrationFailures(ctx context.Context, exec pgExec, consumerGroup, sourceKey, eventKey string, eventID uuid.UUID) error {
	if consumerGroup == "" || sourceKey == "" || eventKey == "" {
		return fmt.Errorf("resolve integration failures: consumer_group, source_key, event_key are required")
	}
	if eventID == uuid.Nil {
		return fmt.Errorf("resolve integration failures: event_id is required")
	}
	_, err := exec.Exec(ctx, sqlResolveIntegrationFailures, consumerGroup, sourceKey, eventKey, eventID)
	if err != nil {
		return fmt.Errorf("resolve integration failures: %w", err)
	}
	return nil
}

// ListPendingIntegrationFailures returns every row with resolved_at
// IS NULL ordered by last_attempt_at ASC. Consumed by operator CLIs
// and by Receiver.RetryPendingFailures.
func ListPendingIntegrationFailures(ctx context.Context, exec pgExec) ([]IntegrationFailureRecord, error) {
	return scanIntegrationFailures(ctx, exec, sqlListPendingIntegrationFailures)
}

// ListPendingIntegrationFailuresByGroup narrows the pending list to one
// consumer group. Used by Receiver.RetryPendingFailures so each receiver
// only acts on the events its own group must re-dispatch — multi-group
// services do not interfere with each other.
func ListPendingIntegrationFailuresByGroup(ctx context.Context, exec pgExec, consumerGroup string) ([]IntegrationFailureRecord, error) {
	if consumerGroup == "" {
		return nil, fmt.Errorf("list pending integration failures by group: consumer_group required")
	}
	return scanIntegrationFailures(ctx, exec, sqlListPendingIntegrationFailuresByGroup, consumerGroup)
}

func scanIntegrationFailures(ctx context.Context, exec pgExec, query string, args ...any) ([]IntegrationFailureRecord, error) {
	rows, err := exec.Query(ctx, query, args...)
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
func IsAlreadyProcessed(ctx context.Context, exec pgExec, eventID uuid.UUID, consumerGroup string) (bool, error) {
	if eventID == uuid.Nil || consumerGroup == "" {
		return false, fmt.Errorf("is already processed: event_id and consumer_group are required")
	}
	row := exec.QueryRow(ctx, sqlPreCheckIntegrationProcessed, eventID, consumerGroup)
	var one int
	if err := row.Scan(&one); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("is already processed: %w", err)
	}
	return true, nil
}

// MarkProcessed writes the dedup row post-handler-success. Uses ON
// CONFLICT DO NOTHING so concurrent processors that raced past the
// pre-check do not double-fail — the row already exists, and the
// handler-side at-least-once contract documents the race.
func MarkProcessed(ctx context.Context, exec pgExec, rec IntegrationProcessedRecord) error {
	if rec.EventID == uuid.Nil || rec.ConsumerGroup == "" {
		return fmt.Errorf("mark processed: event_id and consumer_group are required")
	}
	_, err := exec.Exec(ctx, sqlInsertIntegrationProcessed,
		rec.EventID,
		rec.ConsumerGroup,
		rec.SourceKey,
		rec.EventKey,
		rec.Topic,
		rec.EventType,
	)
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	return nil
}

// PoolExec is a convenience adapter for the package's PG pool when the
// receiver pipeline needs an exec handle without exposing pgxpool to
// the rest of the integration package's call sites.
type PoolExec = *pgxpool.Pool
