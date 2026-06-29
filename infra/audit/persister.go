package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	appaudit "github.com/ClaudioSchirmer/omnicore/application/audit"
	"github.com/ClaudioSchirmer/omnicore/application/persistence"
)

// auditEventColumns is the column list for the in-TX audit write, identical
// across dialects. id is generated in Go (the framework is the id authority on
// every backend); created_at is omitted (a DB DEFAULT on both Postgres and
// MySQL). payload is a JSON envelope carrying the variable parts of the event
// (actorClaims + snapshot + changes + children) — the indexed top-level columns
// stay narrow so the timeline / actor / tenant / thread indexes hit predictable
// shapes.
var auditEventColumns = []string{
	"id", "aggregate_id", "entity_type", "verb", "action_name", "kind",
	"actor", "actor_issuer", "tenant_id", "thread_id",
	"occurred_at", "payload", "trace_id",
}

// Execer is the minimal in-TX write surface the audit insert needs — Exec with
// the backend-neutral signature db.Tx exposes (the engines' pgTx / mysqlTx both
// satisfy it). It is declared HERE rather than imported from infra/db so the
// audit package stays free of a dependency on infra/db — which already depends
// on audit (the Build*Event helpers) and would otherwise form an import cycle.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) error
}

// InsertAuditEvent writes ev as one row in audit_events through the supplied
// in-TX exec surface, with positional placeholders rendered by the engine's
// dialect (placeholder(n) → "$n" on Postgres, "?" on MySQL). Returns an error if
// the INSERT fails so the caller can roll the whole TX back — audit-on-write
// atomicity is the entire point of the database destination.
//
// The audit row id is generated in Go and bound explicitly on every dialect:
// Postgres' UUID column accepts the canonical text (its gen_random_uuid()
// DEFAULT is simply overridden) just as MySQL's CHAR(36) does — one column list,
// one binding order, no dialect fork. The aggregate_id binds verbatim (uuid text
// on Postgres' UUID column, CHAR(36) on MySQL — never BINARY(16); that encoding
// is for entity tables, not the audit trail).
//
// Sentinel handling:
//   - Actor == "" or persistence.AnonymousActor → NULL on the row so the
//     audit_events_actor_idx (partial on Postgres: WHERE actor IS NOT NULL)
//     excludes anonymous writes. Filters for "what alice did" stay on the index.
//   - ActorIssuer / TenantID empty → NULL on the row, same rationale.
func InsertAuditEvent(ctx context.Context, exec Execer, placeholder func(int) string, ev appaudit.AuditEvent) error {
	payload, err := buildAuditPayload(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal payload: %w", err)
	}
	ph := make([]string, len(auditEventColumns))
	for i := range auditEventColumns {
		ph[i] = placeholder(i + 1)
	}
	sql := fmt.Sprintf("INSERT INTO audit_events (%s) VALUES (%s)",
		strings.Join(auditEventColumns, ", "), strings.Join(ph, ", "))
	if err := exec.Exec(ctx, sql,
		uuid.NewString(), // audit row id — Go-generated on every dialect
		ev.EntityID,
		ev.EntityType,
		ev.Verb,
		ev.ActionName,
		ev.Kind,
		nullableActor(ev.Actor),
		nullableString(ev.ActorIssuer),
		nullableString(ev.TenantID),
		ev.ThreadID,
		ev.DateTime,
		payload,
		// Pivot to the request's trace; NULL when tracing is off. Sourced from
		// the event (populateContext) so the in-TX row and the slog echo carry
		// the identical value. The bridge keeps it == AppContext.CorrelationID().
		nullableString(ev.TraceID),
	); err != nil {
		return fmt.Errorf("audit: insert into audit_events: %w", err)
	}
	return nil
}

// buildAuditPayload marshals the variable parts of ev into a single JSON blob.
// Empty/nil sub-blocks are elided so payload size scales with what actually
// exists on the event, not with the AuditEvent's struct shape.
func buildAuditPayload(ev appaudit.AuditEvent) ([]byte, error) {
	payload := map[string]any{}
	if len(ev.ActorClaims) > 0 {
		payload["actorClaims"] = ev.ActorClaims
	}
	if ev.Snapshot != nil {
		payload["snapshot"] = ev.Snapshot
	}
	if len(ev.Changes) > 0 {
		payload["changes"] = ev.Changes
	}
	if len(ev.Children) > 0 {
		payload["children"] = ev.Children
	}
	return json.Marshal(payload)
}

// nullableActor returns nil when the actor is empty or the anonymous sentinel —
// both map to a NULL column. Any other value passes through so the row's actor
// column carries the JWT sub verbatim.
func nullableActor(actor string) any {
	if actor == "" || actor == persistence.AnonymousActor {
		return nil
	}
	return actor
}

// nullableString returns nil for empty strings (NULL column) and the value
// otherwise. Consumed by ActorIssuer + TenantID where empty truly means absent
// rather than "use a sentinel".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
