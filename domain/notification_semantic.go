package domain

// NotificationSemantic classifies the nature of a Notification in a transport-
// agnostic way. The omnicore/web layer maps each value to an HTTP status code;
// future transport layers (gRPC, GraphQL) would map the same enum to their own
// status systems without touching the notification definitions.
type NotificationSemantic int

const (
	SemanticValidation                  NotificationSemantic = iota // → 422 Unprocessable Entity (default)
	SemanticNotFound                                                // → 404 Not Found
	SemanticConflict                                                // → 409 Conflict
	SemanticForbidden                                               // → 403 Forbidden
	SemanticUnauthorized                                            // → 401 Unauthorized
	SemanticUnavailable                                             // → 503 Service Unavailable
	SemanticSchema                                                  // → 400 Bad Request (wire-contract violation)
	SemanticInternal                                                // → 500 Internal Server Error (recovered panic, unexpected error)
	SemanticMethodNotAllowed                                        // → 405 Method Not Allowed (path matches, HTTP method does not)
	SemanticPayloadTooLarge                                         // → 413 Content Too Large (request body exceeds the configured limit)
	SemanticGatewayTimeout                                          // → 504 Gateway Timeout (request exceeded the server-side deadline)
	SemanticStateConflict                                           // → 409 Conflict (entity/system in the wrong state — precondition failure, not a duplicate)
	SemanticRequestTimeout                                          // → 408 Request Timeout (the client did not send the full request within the transport read timeout)
	SemanticGone                                                    // → 410 Gone (the resource existed and was permanently removed — a NotFound the server can vouch for)
	SemanticPreconditionFailed                                      // → 412 Precondition Failed (a conditional header the client sent — If-Match / If-Unmodified-Since — did not hold)
	SemanticUnsupportedMediaType                                    // → 415 Unsupported Media Type (the request's Content-Type is not one this endpoint accepts)
	SemanticTooManyRequests                                         // → 429 Too Many Requests (rate limit or quota exhausted — the client should retry later)
	SemanticNotImplemented                                          // → 501 Not Implemented (the endpoint is declared but the capability behind it is not built)
	SemanticBadGateway                                              // → 502 Bad Gateway (an upstream this service depends on answered with something unusable)
	SemanticPaymentRequired                                         // → 402 Payment Required (a billing or quota gate: settle up and retry)
	SemanticNotAcceptable                                           // → 406 Not Acceptable (no representation the endpoint can produce satisfies the request's Accept)
	SemanticRangeNotSatisfiable                                     // → 416 Range Not Satisfiable (the requested byte range does not exist in the representation)
	SemanticLocked                                                  // → 423 Locked (the resource is temporarily held — retryable, unlike the 409 flavors)
	SemanticPreconditionRequired                                    // → 428 Precondition Required (the request must carry a conditional header and did not)
	SemanticUnavailableForLegalReasons                              // → 451 Unavailable For Legal Reasons (nobody may have this here, by law — not a permission the caller could hold)
	SemanticInsufficientStorage                                     // → 507 Insufficient Storage (the storage allowance for this writer is exhausted)
	SemanticRequestHeaderFieldsTooLarge                             // → 431 Request Header Fields Too Large (the header block or request line exceeds the server's read buffer)
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
	case SemanticStateConflict:
		return "StateConflict"
	case SemanticRequestTimeout:
		return "RequestTimeout"
	case SemanticGone:
		return "Gone"
	case SemanticPreconditionFailed:
		return "PreconditionFailed"
	case SemanticUnsupportedMediaType:
		return "UnsupportedMediaType"
	case SemanticTooManyRequests:
		return "TooManyRequests"
	case SemanticNotImplemented:
		return "NotImplemented"
	case SemanticBadGateway:
		return "BadGateway"
	case SemanticPaymentRequired:
		return "PaymentRequired"
	case SemanticNotAcceptable:
		return "NotAcceptable"
	case SemanticRangeNotSatisfiable:
		return "RangeNotSatisfiable"
	case SemanticLocked:
		return "Locked"
	case SemanticPreconditionRequired:
		return "PreconditionRequired"
	case SemanticUnavailableForLegalReasons:
		return "UnavailableForLegalReasons"
	case SemanticInsufficientStorage:
		return "InsufficientStorage"
	case SemanticRequestHeaderFieldsTooLarge:
		return "RequestHeaderFieldsTooLarge"
	default:
		return "Validation"
	}
}
