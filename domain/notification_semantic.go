package domain

// NotificationSemantic classifies the nature of a Notification in a transport-
// agnostic way. The omnicore/web layer maps each value to an HTTP status code;
// future transport layers (gRPC, GraphQL) would map the same enum to their own
// status systems without touching the notification definitions.
type NotificationSemantic int

const (
	SemanticValidation       NotificationSemantic = iota // → 422 Unprocessable Entity (default)
	SemanticNotFound                                     // → 404 Not Found
	SemanticConflict                                     // → 409 Conflict
	SemanticForbidden                                    // → 403 Forbidden
	SemanticUnauthorized                                 // → 401 Unauthorized
	SemanticUnavailable                                  // → 503 Service Unavailable
	SemanticSchema                                       // → 400 Bad Request (wire-contract violation)
	SemanticInternal                                     // → 500 Internal Server Error (recovered panic, unexpected error)
	SemanticMethodNotAllowed                             // → 405 Method Not Allowed (path matches, HTTP method does not)
	SemanticPayloadTooLarge                              // → 413 Content Too Large (request body exceeds the configured limit)
	SemanticGatewayTimeout                               // → 504 Gateway Timeout (request exceeded the server-side deadline)
)

// String returns the canonical name for logs, debug and wire format.
func (s NotificationSemantic) String() string {
	switch s {
	case SemanticNotFound:
		return "NotFound"
	case SemanticConflict:
		return "Conflict"
	case SemanticForbidden:
		return "Forbidden"
	case SemanticUnauthorized:
		return "Unauthorized"
	case SemanticUnavailable:
		return "Unavailable"
	case SemanticSchema:
		return "Schema"
	case SemanticInternal:
		return "Internal"
	case SemanticMethodNotAllowed:
		return "MethodNotAllowed"
	case SemanticPayloadTooLarge:
		return "PayloadTooLarge"
	case SemanticGatewayTimeout:
		return "GatewayTimeout"
	default:
		return "Validation"
	}
}
