package openapi

import "net/http"

// Example carries one entry of the OpenAPI 3.x `examples` map a Media Type
// Object can declare alongside its `schema`. Three fields shape what Swagger
// UI renders:
//
//   - Summary       — short label shown in the dropdown picker
//   - Description   — markdown rendered under the selected entry
//   - Value         — the actual payload; any JSON-marshalable value, OR a
//     json.RawMessage for hand-authored JSON the consumer
//     wants passed through verbatim
//
// Consumers declare Examples in three places:
//
//   - Doc.RequestExamples / Doc.ResponseExamples  — canonical Mount routes
//   - RequestBody.Examples / ResponseSpec.Examples — MountRaw routes
//   - Per-property `example:"value"` struct tag — the simple-case shortcut
//     covered by the schema generator (one example per scalar field)
//
// The boot path validates Value against the route's declared type for
// request examples and for examples attached to the operation's
// SuccessStatus. Error-status examples are checked only for JSON validity
// (the envelope shape is shared and accepts arbitrary `errors[]` content).
type Example struct {
	Summary     string
	Description string
	Value       any
}

// defaultErrorExamples is the canonical-example registry the framework
// auto-attaches to error responses on every documented route. The map is
// closed in this file — the only way to add or alter an entry is to land
// the change here, so the public API (DefaultErrorExample,
// DefaultErrorExamples) and the runtime renderer stay in lockstep.
//
// Each entry mirrors what the framework actually emits at runtime for the
// given status (typed notification + canonical context + matching
// semantic). The Value is a free-form map[string]any rather than a typed
// struct because:
//
//   - encoding/json renders it identically to a typed envelope
//   - the renderer wants to be able to inject this into a media type's
//     content-level `example` (singular) without an extra Marshal cycle
//   - keeping it untyped lets the registry evolve when a new standard
//     status joins the family (405, 413, …) without dragging a struct
//     dependency along
var defaultErrorExamples = map[int]Example{
	http.StatusBadRequest: {
		Summary: "Schema violation",
		Value:   errorEnvelopeValue(http.StatusBadRequest, "Schema", "SchemaViolationNotification", "body", "", "Schema"),
	},
	http.StatusUnauthorized: {
		Summary: "Missing or invalid bearer token",
		Value:   errorEnvelopeValue(http.StatusUnauthorized, "Authorization", "MissingAuthorizationNotification", "", "", "Unauthorized"),
	},
	http.StatusForbidden: {
		// MissingPermissionNotification is the illustrative emitter — the runtime
		// notificationKey on a real 403 is whatever the gate fires. Layer 2
		// emissions (Update/Delete/Archive/UnarchiveNotAllowedNotification) and
		// Layer 3 (TenantMismatchNotification) carry the same envelope shape; the
		// example is one canonical instance, not a strict prediction.
		Summary: "Missing required permission",
		Value:   errorEnvelopeValue(http.StatusForbidden, "Authorization", "MissingPermissionNotification", "permission", "users:write", "Forbidden"),
	},
	http.StatusNotFound: {
		Summary: "Record not found",
		Value:   errorEnvelopeValue(http.StatusNotFound, "Request", "RecordNotFoundNotification", "id", "", "NotFound"),
	},
	http.StatusUnprocessableEntity: {
		Summary: "Domain validation failure",
		Value:   errorEnvelopeValue(http.StatusUnprocessableEntity, "Request", "RequiredFieldNotification", "name", "", "Validation"),
	},
	http.StatusInternalServerError: {
		Summary: "Recovered panic / unexpected error",
		Value:   errorEnvelopeValue(http.StatusInternalServerError, "Server", "InternalServerErrorNotification", "", "", "Internal"),
	},
	// The statuses below are vocabulary the framework maps but never emits on
	// its own (the 429 middleware branch aside). They carry a default example
	// so a route that DECLARES one via Doc.ResponseExamples renders the shared
	// envelope instead of falling back to the 500 shape; they stay out of
	// standardErrors, so a route that does not declare them documents nothing.
	http.StatusGone: {
		Summary: "Resource permanently removed",
		Value:   errorEnvelopeValue(http.StatusGone, "Request", "ResourceGoneNotification", "id", "", "Gone"),
	},
	http.StatusPreconditionFailed: {
		Summary: "Conditional request precondition not met",
		Value:   errorEnvelopeValue(http.StatusPreconditionFailed, "Request", "PreconditionFailedNotification", "", "", "PreconditionFailed"),
	},
	http.StatusUnsupportedMediaType: {
		Summary: "Content-Type this endpoint cannot read",
		Value:   errorEnvelopeValue(http.StatusUnsupportedMediaType, "Request", "UnsupportedMediaTypeNotification", "", "", "UnsupportedMediaType"),
	},
	http.StatusTooManyRequests: {
		Summary: "Rate limit or quota exhausted",
		Value:   errorEnvelopeValue(http.StatusTooManyRequests, "Request", "TooManyRequestsNotification", "", "", "TooManyRequests"),
	},
	http.StatusNotImplemented: {
		Summary: "Route declared, capability not built",
		Value:   errorEnvelopeValue(http.StatusNotImplemented, "Request", "NotImplementedNotification", "", "", "NotImplemented"),
	},
	http.StatusBadGateway: {
		Summary: "Upstream answered with an unusable response",
		Value:   errorEnvelopeValue(http.StatusBadGateway, "Server", "BadGatewayNotification", "", "", "BadGateway"),
	},
	http.StatusPaymentRequired: {
		Summary: "Billing or quota gate",
		Value:   errorEnvelopeValue(http.StatusPaymentRequired, "Request", "PaymentRequiredNotification", "", "", "PaymentRequired"),
	},
	http.StatusNotAcceptable: {
		Summary: "No representation satisfies Accept",
		Value:   errorEnvelopeValue(http.StatusNotAcceptable, "Request", "NotAcceptableNotification", "", "", "NotAcceptable"),
	},
	http.StatusRequestedRangeNotSatisfiable: {
		Summary: "Requested byte range does not exist",
		Value:   errorEnvelopeValue(http.StatusRequestedRangeNotSatisfiable, "Request", "RangeNotSatisfiableNotification", "", "", "RangeNotSatisfiable"),
	},
	http.StatusLocked: {
		Summary: "Resource temporarily held — retryable",
		Value:   errorEnvelopeValue(http.StatusLocked, "Request", "ResourceLockedNotification", "id", "", "Locked"),
	},
	http.StatusPreconditionRequired: {
		Summary: "Conditional header is mandatory here",
		Value:   errorEnvelopeValue(http.StatusPreconditionRequired, "Request", "PreconditionRequiredNotification", "", "", "PreconditionRequired"),
	},
	http.StatusUnavailableForLegalReasons: {
		Summary: "Withheld by law",
		Value:   errorEnvelopeValue(http.StatusUnavailableForLegalReasons, "Request", "UnavailableForLegalReasonsNotification", "", "", "UnavailableForLegalReasons"),
	},
	http.StatusInsufficientStorage: {
		Summary: "Storage allowance exhausted",
		Value:   errorEnvelopeValue(http.StatusInsufficientStorage, "Request", "InsufficientStorageNotification", "", "", "InsufficientStorage"),
	},
	// The entries below close a gap this round exposed rather than created:
	// these statuses were always in semanticToStatus, but had no default
	// example, so a route DECLARING one rendered the 500-shaped fallback.
	// With them the registry mirrors the Semantic table exactly.
	http.StatusMethodNotAllowed: {
		Summary: "Path is registered for other verbs",
		Value:   errorEnvelopeValue(http.StatusMethodNotAllowed, "Route", "MethodNotAllowedNotification", "DELETE /resource", "", "MethodNotAllowed"),
	},
	http.StatusRequestTimeout: {
		Summary: "Client was too slow sending the request",
		Value:   errorEnvelopeValue(http.StatusRequestTimeout, "Request", "ReadTimeoutNotification", "POST /resource", "", "RequestTimeout"),
	},
	http.StatusConflict: {
		Summary: "Duplicate — this already exists",
		Value:   errorEnvelopeValue(http.StatusConflict, "Request", "EntityAlreadyAddedNotification", "email", "", "Conflict"),
	},
	http.StatusRequestEntityTooLarge: {
		Summary: "Body exceeds the configured BodyLimit",
		Value:   errorEnvelopeValue(http.StatusRequestEntityTooLarge, "Request", "PayloadTooLargeNotification", "POST /resource", "", "PayloadTooLarge"),
	},
	http.StatusRequestHeaderFieldsTooLarge: {
		Summary: "Header block exceeds the read buffer",
		Value:   errorEnvelopeValue(http.StatusRequestHeaderFieldsTooLarge, "Request", "RequestHeaderFieldsTooLargeNotification", "GET /resource", "", "RequestHeaderFieldsTooLarge"),
	},
	http.StatusServiceUnavailable: {
		Summary: "This service is declining to serve",
		Value:   errorEnvelopeValue(http.StatusServiceUnavailable, "Server", "ServiceUnavailableNotification", "", "", "Unavailable"),
	},
	http.StatusGatewayTimeout: {
		Summary: "Handler exceeded the server-side deadline",
		Value:   errorEnvelopeValue(http.StatusGatewayTimeout, "Request", "RequestTimeoutNotification", "", "", "GatewayTimeout"),
	},
}

// DefaultErrorExample returns the canonical envelope example the framework
// auto-attaches to the given error status when the consumer declares
// nothing for it. Useful when a consumer wants to customize the example
// (override Description, reuse Value as a base) or include the canonical
// entry alongside their own variants on a status that they otherwise
// override entirely.
//
// ok=true for every status the framework's Semantic table maps — the registry
// mirrors web.semanticToStatus one-for-one, plus 400, which the transport-level
// MalformedRequestNotification shares with the payload-level Schema flavor.
// ok=false for any other status (418 and the rest of the registry the framework
// deliberately does not map) — the consumer is on their own for those.
func DefaultErrorExample(status int) (Example, bool) {
	ex, ok := defaultErrorExamples[status]
	return ex, ok
}

// DefaultErrorExamples returns a fresh copy of every framework-default
// error example keyed by status code. The returned map is owned by the
// caller — mutating it does NOT affect the framework's internal registry.
// Iterate when building a custom Responses map programmatically.
func DefaultErrorExamples() map[int]Example {
	out := make(map[int]Example, len(defaultErrorExamples))
	for status, ex := range defaultErrorExamples {
		out[status] = ex
	}
	return out
}

// errorEnvelopeValue produces the canonical map payload a wire-format
// error envelope carries. Parameters mirror the framework's runtime
// emission for each standard status — see web/error_handler.go and
// web/handle_command_with_body.go for the originating sites. The `value`
// parameter is the illustrative field-value the notification carries; when
// empty, the `value` JSON property is omitted (matches what the runtime
// emits when no NotificationMessage.FieldValue was set).
func errorEnvelopeValue(status int, contextName, notificationKey, field, value, semantic string) map[string]any {
	message := map[string]any{
		"notificationKey": notificationKey,
		"semantic":        semantic,
		"message":         http.StatusText(status) + ".",
	}
	if field != "" {
		message["field"] = field
	}
	if value != "" {
		message["value"] = value
	}
	return map[string]any{
		"success":     false,
		"status":      status,
		"description": http.StatusText(status),
		"errors": []any{
			map[string]any{
				"context":  contextName,
				"messages": []any{message},
			},
		},
	}
}
