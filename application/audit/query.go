package audit

import (
	"fmt"
	"strconv"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/application/translation"
	"github.com/ClaudioSchirmer/omnicore/domain"
)

// FindByAggregateQuery is the application-layer read of one aggregate's audit
// timeline — the Query behind the framework's own audit read surfaces
// (audit.endpoint in microservice.<profile>.yaml).
//
// It is a plain Query, not a queries.QueryWithParams: the audit trail is read
// from the relational system of record through the audit.Reader port, never
// from a Mongo projection, so none of the view-side machinery (ReadCriteria,
// ViewReader, keyset cursors) applies. What it borrows from the read side is
// the part that IS shared — the ceiling semantics: Limit is resolved by the
// transport from audit.endpoint.maxLimit before dispatch, and always reaches
// the reader positive, so the database is never asked for an unbounded
// timeline.
type FindByAggregateQuery struct {
	pipeline.QueryBase

	// EntityType is the audited aggregate's Go type name, as the write path
	// stamped it into audit_events.entity_type (e.g. "User").
	EntityType string

	// AggregateID is the audited aggregate root's id.
	AggregateID string

	// First is the window the caller asked for (`?first=N` on REST). Zero
	// means "not informed" — the handler then answers a full MaxLimit window.
	First int

	// MaxLimit is the configured ceiling (audit.endpoint.maxLimit). Always
	// positive: the transport reads it from the resolved configuration.
	MaxLimit int
}

// FindByAggregateQueryHandler serves FindByAggregateQuery through the
// audit.Reader port.
//
// PRECONDITION — the audit trail must be routed to `database`. The handler
// reads the audit_events table; a service whose audit.destinations omit
// `database` has no rows to read, which is why the framework refuses to mount
// its read surfaces under that combination rather than answering an empty
// timeline forever (see bootstrap.AuditConfig.Validate). A deployment routing
// audit only to slog reads it from the log stream instead, where RenderLabels
// / RenderLabelsInJSON translate the field labels just the same.
//
// RenderLabels resolves each FieldChange's catalog key into the request's
// locale before the events leave the application layer, so every transport
// receives values already translated — the framework convention that the
// backend renders structured output for every channel.
type FindByAggregateQueryHandler struct {
	Reader       Reader
	Translator   *translation.Translator
	RenderLabels bool
}

// Handle returns the aggregate's timeline, newest first, capped at the
// resolved window. An aggregate with no rows yields an empty (non-nil) slice —
// an absent timeline is a legitimate answer (the record predates audit, or its
// writes were routed elsewhere), never a 404.
func (h *FindByAggregateQueryHandler) Handle(
	ctx *configuration.AppContext, q *FindByAggregateQuery,
) ([]*AuditEvent, error) {
	if h.Reader == nil {
		return nil, fmt.Errorf("audit: find by aggregate: nil reader")
	}
	limit, err := q.resolveLimit()
	if err != nil {
		return nil, err
	}
	events, err := h.Reader.FindByAggregate(ctx, q.EntityType, q.AggregateID, limit)
	if err != nil {
		return nil, err
	}
	if h.RenderLabels {
		lang := ctx.Language()
		for _, ev := range events {
			RenderLabels(ev, h.Translator, lang)
		}
	}
	return events, nil
}

// resolveLimit applies the same window cascade the paged read side applies to
// a view, rendered here for a surface that has no view: a requested window
// above the ceiling is REFUSED (never silently clamped — the consumer asked
// for a page it will not get, and learning that from a truncated array is
// worse than learning it from a 400), a non-positive one is a schema
// violation, and an absent one defers to the ceiling so the statement the
// reader builds always carries a bound.
//
// The rejections travel as *domain.DomainError, so pipeline.Dispatch turns
// them into a Failure and each transport renders its own idiom of the
// canonical envelope — 400 on REST via SemanticSchema. FieldValue carries the
// effective ceiling, matching what the view-side LimitExceededError puts on
// the wire, so a consumer shows "max is N" without parsing a message.
func (q *FindByAggregateQuery) resolveLimit() (int, error) {
	max := q.MaxLimit
	if max <= 0 {
		return 0, fmt.Errorf("audit: find by aggregate: non-positive maxLimit (%d) reached the handler", max)
	}
	switch {
	case q.First < 0:
		return 0, domain.NewDomainErrorWith("Schema", domain.NotificationMessage{
			FieldName:    auditFirstControl,
			Notification: domain.SchemaViolationNotification{},
		})
	case q.First > max:
		return 0, domain.NewDomainErrorWith("Schema", domain.NotificationMessage{
			FieldName:    auditFirstControl,
			FieldValue:   strconv.Itoa(max),
			Notification: domain.LimitExceededNotification{},
		})
	case q.First == 0:
		return max, nil
	default:
		return q.First, nil
	}
}

// auditFirstControl is the wire spelling of the window control the rejections
// above anchor to — the same reserved key the paged read side uses, so a
// consumer meets one vocabulary across every framework surface.
const auditFirstControl = "first"
