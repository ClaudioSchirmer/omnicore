package events

import "time"

// EventLog is the wire shape emitted by SlogPublisher for each domain event
// the entity accumulated via entity.RegisterEvent. Fire-and-forget signal —
// no snapshot/diff semantics (those belong to the auditor); just the event
// payload that the domain registered.
//
// Top-level slog attrs mirror the audit event shape so log aggregators
// query both surfaces with the same vocabulary (threadId, entityType,
// actor, dateTime). EventType discriminates the severity hint and selects
// the slog.Level the line is written at.
//
// EntityType is omitempty because a domain event need not be bound to one
// specific entity class (system-level events, scheduled jobs, etc. emit
// with an empty ClassName).
type EventLog struct {
	ThreadID    string    `json:"threadId"`
	EntityType  string    `json:"entityType,omitempty"`
	EventType   string    `json:"eventType"`
	Actor       string    `json:"actor,omitempty"`
	ActorIssuer string    `json:"actorIssuer,omitempty"`
	DateTime    time.Time `json:"dateTime"`
	Message     string    `json:"message,omitempty"`
	Values      any       `json:"values,omitempty"`
	Exception   string    `json:"exception,omitempty"`
}
