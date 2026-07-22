package audit

import "time"

// AuditEvent is the wire shape of an audit log line. One AuditEvent
// corresponds to one aggregate write (Insert / Update / PartialUpdate /
// Archive / Unarchive / Delete) — granularity B preserved (the aggregate is
// the event unit, identical to the outbox row that ships to Kafka).
//
// Kind discriminates the body payload — exactly one of the three regimes
// applies per event:
//
//   - "snapshot": Snapshot is populated, Changes is empty. Used for Insert
//     (state immediately after the write) and Delete (state at the moment
//     of removal, captured by Old() before the transaction commits).
//
//   - "delta": Changes is populated, Snapshot is empty. Used for Update and
//     PartialUpdate. Each FieldChange carries {field, from, to} for one root
//     field (named in the domain vocabulary — the Go field name) whose value
//     differed between Old() and current. Unchanged fields are omitted (no
//     redundancy with snapshot).
//
//   - "transition": neither Snapshot nor Changes is populated. Used for
//     Archive and Unarchive. The verb itself encodes the transition; the
//     symmetric inverse verb is the recovery path.
//
// Children carries the cascade block when the source entity implements
// AggregateRootProvider and at least one child was touched. The map is keyed
// by the Go type name of the AggregateValueObject (e.g. "Address"); each
// entry describes one touched child. Omitted entirely when no child is
// relevant for the verb (flat entities, Update with no aggregate change).
//
// JSON tags double as slog attribute names — the auditor emits each field as
// a top-level slog.Attr so log aggregators index without parsing nested
// envelopes. ActorClaims is filtered by the auth.auditClaims allowlist
// before reaching this struct; events.Publisher never widens that surface.
type AuditEvent struct {
	ThreadID string `json:"threadId"`
	// TraceID is the active span's trace id (32-char hex) when tracing is on,
	// else empty. The single source both audit destinations mirror — the
	// in-TX audit_events.trace_id column and the slog echo's traceId attribute —
	// so a forensic row or echo line pivots to its trace in the collector.
	TraceID     string         `json:"traceId,omitempty"`
	EntityType  string         `json:"entityType"`
	EntityID    string         `json:"entityId"`
	Verb        string         `json:"verb"`
	ActionName  string         `json:"actionName"`
	Kind        string         `json:"kind"`
	Actor       string         `json:"actor,omitempty"`
	ActorIssuer string         `json:"actorIssuer,omitempty"`
	ActorClaims map[string]any `json:"actorClaims,omitempty"`
	// TenantID carries the multi-tenant scope of the request, extracted from
	// the JWT's "tenant_id" claim (the default Auth0/Keycloak convention).
	// Empty when the request is anonymous or when the claim is absent.
	// Surfaced as a top-level column in audit_events so per-tenant retention
	// and filter queries stay on an indexed path; also emitted as a top-level
	// slog attribute. Customized claim names (auth.authorization.tenant.claim)
	// are NOT honored by audit today — the column stays empty for services
	// that diverge from the default convention.
	TenantID string                  `json:"tenantId,omitempty"`
	DateTime time.Time               `json:"dateTime"`
	Snapshot map[string]any          `json:"snapshot,omitempty"`
	Changes  []FieldChange           `json:"changes,omitempty"`
	Children map[string][]ChildEvent `json:"children,omitempty"`
}

// FieldChange describes a single field diff on a kind=delta event.
// Field is the faithful domain name — the raw Go field name (PascalCase, e.g.
// "Email", "ZipCode") — NOT the physical column. Audit speaks the domain
// vocabulary and is map-blind, so a TableSchema column rename never disturbs the
// audit timeline. From and To are the pre- and post-mutation values; both are
// JSON round-tripable so future recovery code can apply the inverse delta back
// through the same wire format.
//
// FieldLabelKey carries the catalog key declared on the source struct's
// `labelKey:"<catalogKey>"` tag at write time. Stored as the raw key (not the
// rendered string) so future audit readers can render the label in any
// locale the catalog supports — preserving the immutability of the audit
// row across catalog evolution. Empty when the source field has no `label`
// tag; the omitempty elides it from both the audit_events JSON payload and
// the slog echo so existing rows stay byte-identical.
//
// FieldLabel is the read-time slot the framework's RenderLabels populates
// after consuming FieldLabelKey: the renderer clears the key, looks it up
// via Translator.Render, and stores the rendered string here. Mutually
// exclusive in practice with FieldLabelKey (omitempty on both): the
// in-flight write carries FieldLabelKey, the rendered read carries
// FieldLabel.
type FieldChange struct {
	Field         string `json:"field"`
	FieldLabelKey string `json:"fieldLabelKey,omitempty"`
	FieldLabel    string `json:"fieldLabel,omitempty"`
	From          any    `json:"from"`
	To            any    `json:"to"`
}

// ChildEvent describes one cascade entry under AuditEvent.Children[typeName].
// Op carries the per-child operation; Snapshot or Changes is populated per
// the same kind discipline that governs the root AuditEvent — never both.
//
//	inserted    → Snapshot only (the new child as inserted)
//	updated     → Changes only  (per-column diff between Old() and current)
//	archived    → Snapshot only (soft-deleted during an update — taken from Old() — or the cascaded child at root archive)
//	unarchived  → Snapshot only (the restored child)
//	deleted     → Snapshot only (the cascaded child at delete moment)
type ChildEvent struct {
	ID       string         `json:"id,omitempty"`
	Op       string         `json:"op"`
	Snapshot map[string]any `json:"snapshot,omitempty"`
	Changes  []FieldChange  `json:"changes,omitempty"`
}
