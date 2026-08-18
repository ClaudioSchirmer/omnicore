// Package auditapi is the framework's own HTTP surface over the audit trail —
// the connector behind `audit.endpoint.rest` in microservice.<profile>.yaml.
//
// It sits in web/ like every other transport concern and depends only on the
// application layer: the audit.Reader port and the audit Query/handler pair
// reach it as values, and the concrete relational reader is built by the
// composition root. Nothing here imports infra.
//
// READS THE DATABASE. Every route below answers from the `audit_events`
// table, which is written only when audit.destinations includes `database`.
// A deployment routing audit to slog alone has no rows for these routes to
// read — which is why the boot refuses the combination outright instead of
// serving an endpoint that could only ever answer an empty timeline. Such a
// deployment builds its read surface over the log stream, where the
// framework's translation primitives (audit.RenderLabels for a typed event,
// audit.RenderLabelsInJSON for a parsed log line) work unchanged.
package auditapi

import (
	"time"

	"github.com/ClaudioSchirmer/omnicore/application/audit"

	"github.com/google/uuid"
)

// FindAuditByAggregateRequest binds the two path segments the timeline read
// takes plus the one window control it accepts.
//
// AggregateID is typed uuid.UUID so a malformed segment is rejected upfront
// with the canonical 400 SchemaViolationNotification envelope; left as a
// string it would reach the driver and surface as a 500 from the exception
// path. EntityType stays a string — it is a Go type name, not an id.
//
// First DECLARES the window control — it is the endpoint's contract that
// `?first=` exists, which is what the spec generator reflects and what makes
// the DTO the single statement of what this endpoint accepts. It is not the
// binding seat: BindPath fills path tags only, and the criteria parser behind
// the paged wrappers speaks a view vocabulary this endpoint does not have, so
// the value itself is read in parseFirst. The wire key is asserted against
// that reader's constant by a test, so the two spellings cannot drift.
//
// Pointer-typed so the spec renders it OPTIONAL: the generator marks
// non-pointer query fields required, which would contradict the contract that
// an absent window means one full window (audit.endpoint.maxLimit).
type FindAuditByAggregateRequest struct {
	EntityType  string    `path:"entityType" example:"User"`
	AggregateID uuid.UUID `path:"aggregateId" example:"7b3c1f10-3c7e-4a8d-9f0e-9d2a8e6d4b51"`
	First       *int      `query:"first" description:"How many events to return, newest first. Absent returns one full window; above the configured ceiling the request is refused with LimitExceededNotification rather than silently truncated."`
}

// AuditEventResponse is the wire projection of one audit row.
//
// The framework owns this shape deliberately rather than serializing
// application/audit.AuditEvent straight to the wire: that type is the audit
// record every destination shares (the in-TX row, the slog echo, the
// in-process reader), and letting an HTTP contract ride on it would mean any
// change to the record — a field the echo wants, a rename the persister
// needs — silently becomes a wire break for every consumer of this endpoint.
// One explicit mapper (fromEvent) is the price of keeping the two free to
// move apart.
type AuditEventResponse struct {
	ThreadID    string         `json:"threadId"`
	TraceID     string         `json:"traceId,omitempty"`
	EntityType  string         `json:"entityType"`
	EntityID    string         `json:"entityId"`
	Verb        string         `json:"verb"`
	ActionName  string         `json:"actionName"`
	Kind        string         `json:"kind"`
	Actor       string         `json:"actor,omitempty"`
	ActorIssuer string         `json:"actorIssuer,omitempty"`
	ActorClaims map[string]any `json:"actorClaims,omitempty"`
	TenantID    string         `json:"tenantId,omitempty"`
	DateTime    time.Time      `json:"dateTime"`
	Snapshot    map[string]any `json:"snapshot,omitempty"`

	Changes  []AuditFieldChangeResponse           `json:"changes,omitempty"`
	Children map[string][]AuditChildEventResponse `json:"children,omitempty"`
}

// AuditFieldChangeResponse is one field diff on a kind=delta event.
//
// FieldLabel carries the label already rendered in the actor's locale when
// audit.endpoint.renderLabels is on; with it off, FieldLabelKey carries the
// raw catalog key instead and FieldLabel stays empty. Exactly one of the two
// is populated, which is what makes both a machine consumer (stable key) and
// a human-facing one (translated string) first-class.
type AuditFieldChangeResponse struct {
	Field         string `json:"field"`
	FieldLabel    string `json:"fieldLabel,omitempty"`
	FieldLabelKey string `json:"fieldLabelKey,omitempty"`
	From          any    `json:"from"`
	To            any    `json:"to"`
}

// AuditChildEventResponse is one cascade entry under children[typeName].
type AuditChildEventResponse struct {
	ID       string                     `json:"id,omitempty"`
	Op       string                     `json:"op"`
	Snapshot map[string]any             `json:"snapshot,omitempty"`
	Changes  []AuditFieldChangeResponse `json:"changes,omitempty"`
}

// fromEvent projects one application-layer audit record onto the wire shape.
// A nil event yields the zero response — the reader never produces one, and
// the guard keeps the mapper total.
func fromEvent(ev *audit.AuditEvent) AuditEventResponse {
	if ev == nil {
		return AuditEventResponse{}
	}
	out := AuditEventResponse{
		ThreadID:    ev.ThreadID,
		TraceID:     ev.TraceID,
		EntityType:  ev.EntityType,
		EntityID:    ev.EntityID,
		Verb:        ev.Verb,
		ActionName:  ev.ActionName,
		Kind:        ev.Kind,
		Actor:       ev.Actor,
		ActorIssuer: ev.ActorIssuer,
		ActorClaims: ev.ActorClaims,
		TenantID:    ev.TenantID,
		DateTime:    ev.DateTime,
		Snapshot:    ev.Snapshot,
		Changes:     fromChanges(ev.Changes),
	}
	if len(ev.Children) > 0 {
		out.Children = make(map[string][]AuditChildEventResponse, len(ev.Children))
		for typeName, children := range ev.Children {
			entries := make([]AuditChildEventResponse, 0, len(children))
			for _, c := range children {
				entries = append(entries, AuditChildEventResponse{
					ID:       c.ID,
					Op:       c.Op,
					Snapshot: c.Snapshot,
					Changes:  fromChanges(c.Changes),
				})
			}
			out.Children[typeName] = entries
		}
	}
	return out
}

// fromChanges projects a slice of field diffs, returning nil for an empty one
// so `omitempty` elides the key rather than emitting an empty array.
func fromChanges(changes []audit.FieldChange) []AuditFieldChangeResponse {
	if len(changes) == 0 {
		return nil
	}
	out := make([]AuditFieldChangeResponse, 0, len(changes))
	for _, c := range changes {
		out = append(out, AuditFieldChangeResponse{
			Field:         c.Field,
			FieldLabel:    c.FieldLabel,
			FieldLabelKey: c.FieldLabelKey,
			From:          c.From,
			To:            c.To,
		})
	}
	return out
}
