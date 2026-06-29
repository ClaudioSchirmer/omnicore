package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
)

// Rows is the minimal multi-row cursor the audit reader consumes — the read
// twin of Execer's write surface. It mirrors db.Rows method-for-method but is
// declared HERE rather than imported from infra/db so the audit package stays
// free of a dependency on infra/db, which already depends on audit (the
// Build*Event helpers) and would otherwise form an import cycle. The engine's
// neutral db.Rows satisfies it; infra/db bridges the two (NewAuditReader).
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Queryer is the minimal neutral read surface the reader runs its SELECTs
// through — the read counterpart of Execer. One method, Query, returning the
// neutral Rows; the reader detects the no-rows case off the cursor (Next) so it
// never has to name a driver-specific sentinel (pgx and database/sql disagree
// on ErrNoRows). Declared local to audit for the same cycle reason as Rows.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
}

// reader is the single neutral Reader implementation. There is no per-dialect
// reader: the SELECT text is identical on every engine and the only divergence
// (the positional placeholder, "$n" on Postgres / "?" on MySQL) is rendered by
// the placeholder func the engine's dialect supplies — exactly how the criteria
// translator and the view registry are written once against the seam.
type reader struct {
	q           Queryer
	placeholder func(int) string
}

// NewReader builds the neutral audit reader over a query surface + the dialect's
// placeholder renderer. db.NewAuditReader is the canonical constructor that wires
// it from a RelationalEngine; this lower-level entry point exists so a test (or a
// service pinning a bespoke connection) can supply its own Queryer.
func NewReader(q Queryer, placeholder func(int) string) appaudit.Reader {
	return &reader{q: q, placeholder: placeholder}
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

// FindByID returns the audit_events row whose id matches the supplied UUID.
//
// Caveat: the table's primary key is composite — (id, created_at) — to
// satisfy the partition-by-range strategy. A bare `WHERE id = $1` lookup
// triggers a multi-partition index scan; B-tree per partition keeps the
// constant-factor cost low and BRIN on created_at narrows hot ranges,
// but a forensic lookup deep in archived partitions can be slow. A caller
// holding an approximate created_at can narrow the scan with a bespoke query
// against the engine's Querier; this helper stays minimal because the common
// case (a recent row from a slog line) is fast enough as-is.
//
// The id binds as canonical text on every dialect (Postgres' UUID column and
// MySQL's CHAR(36) both accept it), mirroring how InsertAuditEvent writes it —
// no BINARY(16) value codec is involved on the audit trail.
func (r *reader) FindByID(ctx context.Context, id uuid.UUID) (*appaudit.AuditEvent, error) {
	if r.q == nil {
		return nil, errors.New("audit: nil querier")
	}
	sql := selectAuditEventCols + ` WHERE id = ` + r.placeholder(1)
	rows, err := r.q.Query(ctx, sql, id.String())
	if err != nil {
		return nil, fmt.Errorf("audit: find by id: %w", err)
	}
	defer rows.Close()
	// No-rows is read off the cursor, not a driver sentinel: pgx and
	// database/sql disagree on ErrNoRows, so Next()==false (with a clean Err)
	// is the one neutral signal.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("audit: find by id: %w", err)
		}
		return nil, appaudit.ErrAuditNotFound
	}
	ev, err := scanAuditRow(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("audit: find by id: %w", err)
	}
	return ev, nil
}

// FindByAggregate returns every audit_events row for one aggregate, newest
// first. Index-served by audit_events_entity_timeline_idx
// (entity_type, aggregate_id, occurred_at DESC) — the canonical "give me
// this user's audit timeline" query the table was designed for.
//
// Returns an empty slice + nil error when the aggregate has no audit
// rows (e.g. it was created before audit was enabled, or the destinations
// list excluded `database`). Pagination is the caller's job today — the
// helper returns every matching row; large aggregates should slice the
// result downstream, or run a bespoke query with explicit LIMIT/OFFSET
// against the engine's Querier when the cardinality is known to be high.
func (r *reader) FindByAggregate(ctx context.Context, entityType, aggregateID string) ([]*appaudit.AuditEvent, error) {
	if r.q == nil {
		return nil, errors.New("audit: nil querier")
	}
	if entityType == "" || aggregateID == "" {
		return nil, errors.New("audit: find by aggregate requires non-empty entityType and aggregateID")
	}
	sql := selectAuditEventCols +
		` WHERE entity_type = ` + r.placeholder(1) +
		` AND aggregate_id = ` + r.placeholder(2) +
		` ORDER BY occurred_at DESC`
	rows, err := r.q.Query(ctx, sql, entityType, aggregateID)
	if err != nil {
		return nil, fmt.Errorf("audit: find by aggregate: %w", err)
	}
	defer rows.Close()

	out := []*appaudit.AuditEvent{}
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
	ActorClaims map[string]any                   `json:"actorClaims,omitempty"`
	Snapshot    map[string]any                   `json:"snapshot,omitempty"`
	Changes     []appaudit.FieldChange           `json:"changes,omitempty"`
	Children    map[string][]appaudit.ChildEvent `json:"children,omitempty"`
}

// scanAuditRow consumes one row (either the single FindByID row or one
// rows.Next() iteration of FindByAggregate) and assembles the *AuditEvent.
// The neutral Rows.Scan is `func(...any) error` regardless of engine, so the
// helper takes scan as a func to stay agnostic.
//
// NULL handling for columns that the persister writes as nullable
// (actor / actor_issuer / tenant_id): scanned into `*string` and turned
// back into the empty string on nil so the AuditEvent shape stays
// non-pointer (callers never have to nil-check these scalars). JSON
// numeric drift on payload.Snapshot is documented — encoding/json
// returns float64 / string / bool / nil / map / slice, not the original
// Go types the write side handed over. Compare snapshot values as JSON
// (or stringified) rather than asserting on int / time.Time.
func scanAuditRow(scan func(dest ...any) error) (*appaudit.AuditEvent, error) {
	var (
		id           uuid.UUID
		aggregateID  uuid.UUID
		threadID     uuid.UUID
		actor        *string
		actorIssuer  *string
		tenantID     *string
		payloadBytes []byte
		ev           appaudit.AuditEvent
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

// stringOrEmpty dereferences a *string returned when scanning a nullable
// column. nil → "" so the AuditEvent struct stays flat.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
