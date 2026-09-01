package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/domain"
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
	encode      func(any) any
	applyLimit  func(sql string, n int) string
}

// NewReader builds the neutral audit reader over a query surface + the dialect's
// placeholder renderer, value codec and row cap. db.NewAuditReader is the canonical
// constructor that wires it from a RelationalEngine; this lower-level entry point
// exists so a test (or a service pinning a bespoke connection) can supply its own
// Queryer.
//
// applyLimit is the dialect's Dialect.ApplyLimit — the cap is NOT a shared
// ` LIMIT n` suffix, because an engine whose cap sits in the SELECT head (SQL
// Server's TOP) rewrites the statement instead. A nil applyLimit leaves the
// timeline read uncapped, which only a caller wiring the reader by hand can
// produce; every framework path supplies the dialect's renderer.
func NewReader(q Queryer, placeholder func(int) string, encode func(any) any, applyLimit func(string, int) string) appaudit.Reader {
	return &reader{q: q, placeholder: placeholder, encode: encode, applyLimit: applyLimit}
}

// selectAuditEventCols is the canonical column list every read helper
// shares so column-order drift between helpers is impossible. Mirrors the
// INSERT column list from persister.go in spirit, plus `id` (DB-generated)
// and `created_at` (server-side timestamp, equal to occurred_at on the
// happy path but technically distinct).
// It MUST start at "SELECT " with no leading whitespace: Dialect.ApplyLimit
// takes a complete SELECT and some engines cap by rewriting the statement HEAD
// (SQL Server's TOP), which they can only find at position zero.
const selectAuditEventCols = `SELECT id, entity_type, aggregate_id, verb, action_name, kind,
       actor, actor_issuer, tenant_id, thread_id, trace_id, occurred_at, payload
FROM audit_events`

// FindByID returns the audit_events row whose id matches the supplied UUID.
//
// Served directly by the primary key (id): audit_events is a plain table (no
// partitioning) on every backend, so `WHERE id = $1` is a single unique-index
// lookup. The id is a time-ordered UUID v7, so inserts append at the index tail
// and the recent rows a forensic lookup usually wants sit in the hot pages.
//
// The id binds through the dialect's value codec (uuid text on PG, BINARY(16)
// elsewhere) — the framework id standard, mirroring how InsertAuditEvent
// writes it. The aggregate_id reference stays canonical text on every dialect.
func (r *reader) FindByID(ctx context.Context, id uuid.UUID) (*appaudit.AuditEvent, error) {
	if r.q == nil {
		return nil, errors.New("audit: nil querier")
	}
	sql := selectAuditEventCols + ` WHERE id = ` + r.placeholder(1)
	rows, err := r.q.Query(ctx, sql, r.encode(domain.NewID(id.String())))
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

// FindByAggregate returns the newest `limit` audit_events rows of one
// aggregate, newest first. Index-served by audit_events_entity_timeline_idx
// (entity_type, aggregate_id, occurred_at DESC) — the canonical "give me
// this user's audit timeline" query the table was designed for, and the
// index that also makes the capped read a partial index walk rather than a
// full timeline sort.
//
// The cap is rendered INTO the statement by the dialect (Dialect.ApplyLimit),
// never applied to the materialized slice: an aggregate with a long history
// must not cross the wire from the database only to be trimmed in Go. limit
// must be positive — the caller resolves its ceiling (the framework endpoint
// reads audit.endpoint.maxLimit) before calling.
//
// Returns an empty slice + nil error when the aggregate has no audit
// rows (e.g. it was created before audit was enabled, or the destinations
// list excluded `database`).
func (r *reader) FindByAggregate(ctx context.Context, entityType, aggregateID string, limit int) ([]*appaudit.AuditEvent, error) {
	if r.q == nil {
		return nil, errors.New("audit: nil querier")
	}
	if entityType == "" || aggregateID == "" {
		return nil, errors.New("audit: find by aggregate requires non-empty entityType and aggregateID")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("audit: find by aggregate requires a positive limit (got %d)", limit)
	}
	sql := selectAuditEventCols +
		` WHERE entity_type = ` + r.placeholder(1) +
		` AND aggregate_id = ` + r.placeholder(2) +
		` ORDER BY occurred_at DESC`
	if r.applyLimit != nil {
		sql = r.applyLimit(sql, limit)
	}
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
	ClientIP    string                           `json:"clientIp,omitempty"`
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
		traceID      *string
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
		&traceID,
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
	// trace_id is the pivot to the producing request's trace. The persister
	// writes it (NULL when no span was active), so a read that skipped the
	// column would hand every consumer an empty TraceID and silently break
	// the audit-row → trace jump the column exists for.
	ev.TraceID = stringOrEmpty(traceID)

	if len(payloadBytes) > 0 {
		var pl auditPayload
		if err := json.Unmarshal(payloadBytes, &pl); err != nil {
			return nil, fmt.Errorf("unmarshal payload (id=%s): %w", id, err)
		}
		ev.ClientIP = pl.ClientIP
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
