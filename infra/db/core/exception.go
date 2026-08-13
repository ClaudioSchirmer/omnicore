package core

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

type InfrastructureError struct {
	Contexts []*domain.NotificationContext
}

func NewInfrastructureError(contexts []*domain.NotificationContext) *InfrastructureError {
	return &InfrastructureError{Contexts: contexts}
}

func NewInfrastructureErrorWith(contextName string, msg domain.NotificationMessage) *InfrastructureError {
	ctx := domain.NewNotificationContext(contextName)
	ctx.AddNotificationMessage(msg)
	return &InfrastructureError{Contexts: []*domain.NotificationContext{ctx}}
}

func (e *InfrastructureError) Error() string {
	return fmt.Sprintf("infrastructure error: %d context(s)", len(e.Contexts))
}

func (e *InfrastructureError) NotificationContexts() []*domain.NotificationContext {
	return e.Contexts
}

// SingleNotificationError packages a single notification into *InfrastructureError.
func SingleNotificationError(contextName, fieldName string, n domain.Notification) *InfrastructureError {
	return NewInfrastructureErrorWith(contextName, domain.NotificationMessage{
		FieldName: fieldName, Notification: n,
	})
}

// FieldErrorWithCause includes a raw error as cause (typical for constraint catch).
func FieldErrorWithCause(contextName, fieldName string, cause error, n domain.Notification) *InfrastructureError {
	return NewInfrastructureErrorWith(contextName, domain.NotificationMessage{
		FieldName: fieldName, Err: cause, Notification: n,
	})
}

// LimitExceededError packages a per-view "page size too high" rejection in the
// canonical Schema envelope. FieldName names the directional control that
// carried the size — "first" forward, "last" backward — so the 400 points at
// the exact wire key the consumer sent; FieldValue carries the effective
// ceiling so the consumer surfaces "max is X" without parsing the translated
// message. Wired to SemanticSchema → 400 via the kernel
// LimitExceededNotification.
func LimitExceededError(maxLimit int64, backward bool) *InfrastructureError {
	field := "first"
	if backward {
		field = "last"
	}
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    field,
		FieldValue:   fmt.Sprintf("%d", maxLimit),
		Notification: domain.LimitExceededNotification{},
	})
}

// InvalidCursorError packages a keyset-cursor rejection (undecodable, tuple-
// length mismatch, or context-hash mismatch) in the canonical Schema envelope.
// FieldName is "cursor"; the cause is carried for diagnostics. Wired to
// SemanticSchema → 400-equivalent via the kernel SchemaViolationNotification —
// the SAME notification the REST wrapper's pre-dispatch check emits, so both
// surfaces report an identical notificationKey. The reader reaches this only
// on surfaces that do not pre-validate the cursor (the REST wrapper rejects it
// before dispatch); without it, a stale/foreign cursor would surface as a
// plain error → 500/Internal instead of a legible Schema rejection.
func InvalidCursorError(cause error) *InfrastructureError {
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    "cursor",
		Err:          cause,
		Notification: domain.SchemaViolationNotification{},
	})
}

// UnresolvedFieldPathError packages "this view cannot resolve that Go field
// path" in the canonical Schema envelope — a filter, sort or projection path
// that the view's TableSchema tree does not translate to a physical column.
// FieldName is the offending dotted Go path.
//
// The alternative the reader used to take was to pass the path through to Mongo
// verbatim, which silently matched nothing: a filter returned an empty page and
// a sort did nothing, both with a 200. A path the view cannot name is a schema
// mismatch between the caller's DTO and the view's declaration — the same class
// of error as an unknown `?fields=` token, and it gets the same 400 (via
// SemanticSchema on the kernel SchemaViolationNotification). The relational
// backing already rejects its unservable fields this way, so both backings
// answer an unknown path identically.
func UnresolvedFieldPathError(goPath string) *InfrastructureError {
	return NewInfrastructureErrorWith("Schema", domain.NotificationMessage{
		FieldName:    goPath,
		Notification: domain.SchemaViolationNotification{},
	})
}
