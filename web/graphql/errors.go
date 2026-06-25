package graphql

import (
	"fmt"

	"github.com/ClaudioSchirmer/omnicore/application/notifications"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// GraphQLError is one entry of the GraphQL response `errors` array. The
// framework's typed notification identity travels in extensions so clients
// branch on `notificationKey` / `semantic` exactly as REST clients branch on
// the envelope's NotificationKey / Semantic — GraphQL always returns HTTP 200,
// so the semantic is data, not a status line.
type GraphQLError struct {
	Message    string         `json:"message"`
	Path       []any          `json:"path,omitempty"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

// Response is the GraphQL response envelope.
type Response struct {
	Data   map[string]any `json:"data,omitempty"`
	Errors []GraphQLError `json:"errors,omitempty"`
}

// errf builds a single-message GraphQLError for an argument/translation fault
// surfaced by the executor itself (not a domain notification).
func errf(format string, args ...any) *GraphQLError {
	return &GraphQLError{Message: fmt.Sprintf(format, args...)}
}

// fromNotifications maps a Result.Failure's notification DTOs into GraphQL
// errors, one per message, carrying the typed identity in extensions.
func fromNotifications(ctxs []notifications.ContextDTO) []GraphQLError {
	var out []GraphQLError
	for _, ctx := range ctxs {
		for _, m := range ctx.Messages {
			ext := map[string]any{"semantic": m.Semantic.String()}
			// The REST envelope groups messages under a `context` (the
			// translated context name, e.g. "User"); the flat GraphQL
			// errors[] has no grouping level, so the context travels per
			// message in extensions. Emitted only when non-empty, matching
			// the wire's omitempty so services that don't name a context
			// stay byte-identical.
			if ctx.Context != "" {
				ext["context"] = ctx.Context
			}
			if m.NotificationKey != "" {
				ext["notificationKey"] = m.NotificationKey
			}
			if m.FieldName != "" {
				ext["field"] = m.FieldName
			}
			// Mirror the REST ErrorMessage (web/from_notifications.go): the
			// translated human label (from the `labelKey` tag), the echoed
			// value, and the funcName all travel in extensions so a GraphQL
			// client reads the same vocabulary the REST envelope carries.
			// Each is emitted only when non-empty, matching the wire's
			// omitempty so services that don't use these stay byte-identical.
			if m.FieldLabel != "" {
				ext["fieldLabel"] = m.FieldLabel
			}
			if m.FieldValue != "" {
				ext["value"] = m.FieldValue
			}
			if m.FuncName != "" {
				ext["funcName"] = m.FuncName
			}
			out = append(out, GraphQLError{Message: m.Message, Extensions: ext})
		}
	}
	return out
}

// internalError is the opaque error surfaced for a Result.Exception — the
// panic value / stack stay server-side, mirroring the REST 500 posture.
func internalError() []GraphQLError {
	return []GraphQLError{{
		Message:    "internal server error",
		Extensions: map[string]any{"semantic": "Internal"},
	}}
}

// fromGqlErrors adapts gqlparser parse/validation errors into the response
// envelope's error shape.
func fromGqlErrors(errs gqlerror.List) []GraphQLError {
	out := make([]GraphQLError, 0, len(errs))
	for _, e := range errs {
		out = append(out, GraphQLError{Message: e.Message})
	}
	return out
}
