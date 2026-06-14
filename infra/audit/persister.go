package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// insertAuditEventSQL is the canonical SQL for the in-TX audit write. The
// table is provisioned by the framework's embedded migration 0001 and the
// column list is identical across services. payload is a JSONB envelope
// carrying the variable parts of the event (actorClaims + snapshot +
// changes + children) — the indexed top-level columns stay narrow so the
// timeline / actor / tenant / thread indexes hit predictable shapes.
const insertAuditEventSQL = `
INSERT INTO audit_events (
    aggregate_id, entity_type, verb, action_name, kind,
    actor, actor_issuer, tenant_id, thread_id,
    occurred_at, payload
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11
)`

// InsertAuditEvent writes ev as one row in audit_events inside the provided
// transaction. Returns an error if the INSERT fails so the caller can
// rollback the whole TX — audit-on-write atomicity is the entire point of
// the database destination.
//
// Sentinel handling:
//   - Actor == "" or domain.AnonymousActor → NULL on the row so the
//     audit_events_actor_idx (partial: WHERE actor IS NOT NULL) excludes
//     anonymous writes. Filters for "what alice did" stay on the index.
//   - ActorIssuer / TenantID empty → NULL on the row, same rationale.
func InsertAuditEvent(ctx context.Context, tx pgx.Tx, ev AuditEvent) error {
	payload, err := buildAuditPayload(ev)
	if err != nil {
		return fmt.Errorf("audit: marshal payload: %w", err)
	}
	_, err = tx.Exec(ctx, insertAuditEventSQL,
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
	)
	if err != nil {
		return fmt.Errorf("audit: insert into audit_events: %w", err)
	}
	return nil
}

// buildAuditPayload marshals the variable parts of ev into a single JSONB
// blob. Empty/nil sub-blocks are elided so payload size scales with what
// actually exists on the event, not with the AuditEvent's struct shape.
func buildAuditPayload(ev AuditEvent) ([]byte, error) {
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

// nullableActor returns nil when the actor is empty or the anonymous
// sentinel — both map to a NULL column. Any other value passes through so
// the row's actor column carries the JWT sub verbatim.
func nullableActor(actor string) any {
	if actor == "" || actor == domain.AnonymousActor {
		return nil
	}
	return actor
}

// nullableString returns nil for empty strings (NULL column) and the value
// otherwise. Consumed by ActorIssuer + TenantID where empty truly means
// absent rather than "use a sentinel".
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
