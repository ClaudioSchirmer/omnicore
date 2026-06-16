package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrAuditNotFound is the sentinel FindByID returns when no audit_events
// row matches the supplied id. Callers branch with `errors.Is(err,
// ErrAuditNotFound)` to map the miss to whatever transport response shape
// suits them (HTTP 404, empty CLI output, etc.). Transport / SQL failures
// surface as the underlying error wrapped with a "audit: ..." prefix.
var ErrAuditNotFound = errors.New("audit: event not found")

// pgExec is the minimal interface the audit reader helpers consume. Both
// *pgxpool.Pool, *pgxpool.Conn, *pgx.Conn, and pgx.Tx satisfy it. Same
// shape pg_view_registry already uses in the infra package — kept local
// to the audit package so the read helpers stay self-contained.
type pgExec interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// selectAuditEventCols is the canonical column list every read helper
// shares so column-order drift between helpers is impossible. Mirrors the
// INSERT column list from persister.go in spirit, plus `id` (DB-generated)
// and `created_at` (server-side timestamp, equal to occurred_at on the
// happy path but technically distinct).
const selectAuditEventCols = `
SELECT id, entity_type, aggregate_id, verb, action_name, kind,
       actor, actor_issuer, tenant_id, thread_id, occurred_at, payload
FROM audit_events`

// FindByID returns the audit_events row whose id matches the supplied
// UUID. Returns (nil, ErrAuditNotFound) on miss; (nil, err) on transport
// failure; (*AuditEvent, nil) on hit.
//
// Caveat: the table's primary key is composite — (id, created_at) — to
// satisfy the partition-by-range strategy. A bare `WHERE id = $1` lookup
// triggers a multi-partition index scan; B-tree per partition keeps the
// constant-factor cost low and BRIN on created_at narrows hot ranges,
// but a forensic lookup deep in archived partitions can be slow. Callers
// holding an approximate created_at should add it to the WHERE manually
// via the raw `exec` if performance matters at that scale — this helper
// stays minimal because the common case (recent rows from a slog line)
// is fast enough as-is.
func FindByID(ctx context.Context, exec pgExec, id uuid.UUID) (*AuditEvent, error) {
	if exec == nil {
		return nil, errors.New("audit: nil exec")
	}
	row := exec.QueryRow(ctx, selectAuditEventCols+` WHERE id = $1`, id)
	ev, err := scanAuditRow(row.Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAuditNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("audit: find by id: %w", err)
	}
	return ev, nil
}

// FindByAggregate returns every audit_events row for one aggregate,
// newest first. Index-served by audit_events_entity_timeline_idx
// (entity_type, aggregate_id, occurred_at DESC) — the canonical "give me
// this user's audit timeline" query the table was designed for.
//
// Returns an empty slice + nil error when the aggregate has no audit
// rows (e.g. it was created before audit was enabled, or the destinations
// list excluded `database`). Pagination is the caller's job today — the
// helper returns every matching row; large aggregates should slice the
// result downstream, or call raw `exec` with explicit LIMIT/OFFSET when
// the cardinality is known to be high.
func FindByAggregate(ctx context.Context, exec pgExec, entityType, aggregateID string) ([]*AuditEvent, error) {
	if exec == nil {
		return nil, errors.New("audit: nil exec")
	}
	if entityType == "" || aggregateID == "" {
		return nil, errors.New("audit: find by aggregate requires non-empty entityType and aggregateID")
	}
	rows, err := exec.Query(ctx,
		selectAuditEventCols+` WHERE entity_type = $1 AND aggregate_id = $2 ORDER BY occurred_at DESC`,
		entityType, aggregateID)
	if err != nil {
		return nil, fmt.Errorf("audit: find by aggregate: %w", err)
	}
	defer rows.Close()

	out := []*AuditEvent{}
	for rows.Next() {
		ev, err := scanAuditRow(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("audit: scan row: %w", err)
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("audit: iterate rows: %w", err)
	}
	return out, nil
}

// auditPayload is the deserialization view of the payload jsonb column.
// Mirrors the shape buildAuditPayload (persister.go) writes so a row's
// roundtrip through the table preserves the AuditEvent semantic.
type auditPayload struct {
	ActorClaims map[string]any          `json:"actorClaims,omitempty"`
	Snapshot    map[string]any          `json:"snapshot,omitempty"`
	Changes     []FieldChange           `json:"changes,omitempty"`
	Children    map[string][]ChildEvent `json:"children,omitempty"`
}

// scanAuditRow consumes one pgx row (either pgx.Row from QueryRow or one
// rows.Next() iteration of pgx.Rows) and assembles the *AuditEvent. Both
// pgx.Row.Scan and pgx.Rows.Scan are `func(...any) error` so they share
// the same signature — scan is passed in to keep the helper agnostic.
//
// NULL handling for columns that the persister writes as nullable
// (actor / actor_issuer / tenant_id): scanned into `*string` and turned
// back into the empty string on nil so the AuditEvent shape stays
// non-pointer (callers never have to nil-check these scalars). JSON
// numeric drift on payload.Snapshot is documented — encoding/json
// returns float64 / string / bool / nil / map / slice, not the original
// Go types the write side handed over. Compare snapshot values as JSON
// (or stringified) rather than asserting on int / time.Time.
func scanAuditRow(scan func(dest ...any) error) (*AuditEvent, error) {
	var (
		id           uuid.UUID
		aggregateID  uuid.UUID
		threadID     uuid.UUID
		actor        *string
		actorIssuer  *string
		tenantID     *string
		payloadBytes []byte
		ev           AuditEvent
	)
	err := scan(
		&id,
		&ev.EntityType,
		&aggregateID,
		&ev.Verb,
		&ev.ActionName,
		&ev.Kind,
		&actor,
		&actorIssuer,
		&tenantID,
		&threadID,
		&ev.DateTime,
		&payloadBytes,
	)
	if err != nil {
		return nil, err
	}
	ev.EntityID = aggregateID.String()
	ev.ThreadID = threadID.String()
	ev.Actor = stringOrEmpty(actor)
	ev.ActorIssuer = stringOrEmpty(actorIssuer)
	ev.TenantID = stringOrEmpty(tenantID)

	if len(payloadBytes) > 0 {
		var pl auditPayload
		if err := json.Unmarshal(payloadBytes, &pl); err != nil {
			return nil, fmt.Errorf("unmarshal payload (id=%s): %w", id, err)
		}
		ev.ActorClaims = pl.ActorClaims
		ev.Snapshot = pl.Snapshot
		ev.Changes = pl.Changes
		ev.Children = pl.Children
	}
	return &ev, nil
}

// stringOrEmpty dereferences a *string returned by pgx when scanning a
// nullable column. nil → "" so the AuditEvent struct stays flat.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
