package notifications

import "github.com/ClaudioSchirmer/omnicore/domain"

type ContextNotInitializedNotification struct {
	domain.ApplicationNotificationBase
}
type ServiceUnavailableNotification struct {
	domain.ApplicationNotificationBase
}

// ServiceUnavailable is the only kernel application notification whose
// natural transport semantic differs from the default Validation (422).
func (ServiceUnavailableNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnavailable
}

// RequestTimeoutNotification is emitted by pipeline.Run when a handler aborts
// because the request's context deadline (http.requestTimeoutSeconds) elapsed:
// pgx, mongo and outbound httpclient observe the cancellation and return
// context.DeadlineExceeded, which Run maps to this notification so the request
// surfaces as 504 Gateway Timeout instead of a generic 500. The server-side
// budget protects framework resources — a pool connection or goroutine is
// released the moment the deadline fires, rather than being held by a slow
// request indefinitely.
type RequestTimeoutNotification struct {
	domain.ApplicationNotificationBase
}

func (RequestTimeoutNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticGatewayTimeout
}

// ReadTimeoutNotification is emitted by the web ErrorHandler when the fasthttp
// server's read timeout (http.readTimeoutSeconds) fires while reading the
// inbound request off the socket — the client was too slow sending it (the
// slowloris defense). Distinct from RequestTimeoutNotification (504), which is
// the server-side HANDLER deadline: this one is a transport-level read timeout
// and the client, not the server, ran out of time — hence 408 Request Timeout.
type ReadTimeoutNotification struct {
	domain.ApplicationNotificationBase
}

func (ReadTimeoutNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticRequestTimeout
}

// UnsupportedCapabilityNotification is emitted by a read engine when a request
// asks for something the store the view is read from cannot serve — free-text
// search, or a filter or sort on a field a single-root read cannot reach. It names
// no backing on purpose: EVERY engine raises this same notification, so the four
// surfaces render one refusal whatever serves the view, and adding an engine adds
// no vocabulary here. The offending capability or Go field path rides in the
// notification's FieldName. Carries Semantic = SemanticSchema -> 400 Bad Request.
type UnsupportedCapabilityNotification struct {
	domain.ApplicationNotificationBase
}

func (UnsupportedCapabilityNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticSchema
}

// MissingAuthorizationNotification is emitted by the auth middleware when the
// Authorization header is absent or does not follow the `Bearer <token>`
// shape — the client never presented a credential.
type MissingAuthorizationNotification struct {
	domain.ApplicationNotificationBase
}

// InvalidTokenNotification is emitted by the auth middleware when the bearer
// token fails local validation: signature mismatch, wrong issuer / audience /
// algorithm, malformed JWT, or any other reason the token cannot be trusted
// at all. Distinct from ExpiredTokenNotification so clients can branch on
// re-login vs refresh.
type InvalidTokenNotification struct {
	domain.ApplicationNotificationBase
}

// ExpiredTokenNotification is emitted by the auth middleware when the bearer
// token's `exp` claim is in the past (after the configured leeway). Split
// from InvalidTokenNotification because clients typically respond by
// refreshing the token rather than re-authenticating from scratch.
type ExpiredTokenNotification struct {
	domain.ApplicationNotificationBase
}

func (MissingAuthorizationNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnauthorized
}
func (InvalidTokenNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnauthorized
}
func (ExpiredTokenNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnauthorized
}

// InternalServerErrorNotification is emitted by the web ErrorHandler when a
// panic is recovered or any non-NotificationCarrier error escapes a handler
// or middleware. The wire envelope carries only the typed notification key
// and the translated message — the underlying cause stays on the server log
// and is never leaked over the wire.
type InternalServerErrorNotification struct {
	domain.ApplicationNotificationBase
}

func (InternalServerErrorNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticInternal
}

// RouteNotFoundNotification is emitted by the web ErrorHandler when Fiber's
// router cannot match the incoming METHOD + path. The FieldName carries the
// METHOD /path so clients can branch UI on the missing route without parsing
// the translated message.
type RouteNotFoundNotification struct {
	domain.ApplicationNotificationBase
}

func (RouteNotFoundNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticNotFound
}

// MethodNotAllowedNotification is emitted by the web ErrorHandler when Fiber's
// router matches the path but rejects the HTTP method (the path is registered
// for at least one other verb). FieldName carries the METHOD /path so clients
// can surface the mismatch without parsing the translated message.
type MethodNotAllowedNotification struct {
	domain.ApplicationNotificationBase
}

func (MethodNotAllowedNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticMethodNotAllowed
}

// PayloadTooLargeNotification is emitted by the web ErrorHandler when Fiber's
// BodyLimit middleware rejects a request whose body exceeds the configured
// maximum size. FieldName carries the METHOD /path of the rejected request.
type PayloadTooLargeNotification struct {
	domain.ApplicationNotificationBase
}

func (PayloadTooLargeNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticPayloadTooLarge
}

// TooManyRequestsNotification is the rate-limit / quota refusal: the request
// was well-formed and authorized, but the caller has spent its allowance for
// now and should retry later. The framework never emits it on its own — it
// ships no rate limiter — but it owns the vocabulary so a service (or a
// third-party middleware rejecting through fiber.ErrTooManyRequests) surfaces
// the refusal in the canonical envelope instead of a bare status line.
// Carries SemanticTooManyRequests -> 429 on HTTP and RESOURCE_EXHAUSTED on gRPC.
//
// A 429 is only half an answer without a retry hint. The envelope carries no
// headers, so a handler that knows when the window reopens sets
// c.Set(fiber.HeaderRetryAfter, ...) before responding — the header the
// framework's own httpclient honors on the way back in.
type TooManyRequestsNotification struct {
	domain.ApplicationNotificationBase
}

func (TooManyRequestsNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticTooManyRequests
}

// ResourceGoneNotification is the stronger sibling of
// RecordNotFoundNotification: the resource DID exist at this address and was
// permanently removed, and the server is willing to say so. Reach for it when
// absence is a fact worth publishing (a hard-deleted aggregate, a retired
// endpoint) and for plain "no row matched" keep the 404 — a 410 tells caches
// and crawlers to stop asking, which is not something to claim by accident.
// Carries SemanticGone -> 410 on HTTP and NOT_FOUND on gRPC.
type ResourceGoneNotification struct {
	domain.ApplicationNotificationBase
}

func (ResourceGoneNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticGone
}

// PreconditionFailedNotification is the conditional-request refusal: the
// client sent a precondition header (If-Match, If-Unmodified-Since) and the
// resource no longer satisfies it. Distinct from
// ConcurrentModificationNotification (409 StateConflict), which is the
// framework's own revision guard firing INSIDE a write: there the client
// asserted nothing and the collision was detected for it; here the client
// stated a condition up front and the server is answering that statement.
// Carries SemanticPreconditionFailed -> 412 on HTTP and FAILED_PRECONDITION
// on gRPC.
type PreconditionFailedNotification struct {
	domain.ApplicationNotificationBase
}

func (PreconditionFailedNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticPreconditionFailed
}

// UnsupportedMediaTypeNotification is the Content-Type refusal: the endpoint
// cannot read the representation the client sent. Distinct from
// SchemaViolationNotification (400 Schema), which is a body the endpoint DID
// read and found malformed — this one is refused before parsing is attempted.
// Carries SemanticUnsupportedMediaType -> 415 on HTTP and INVALID_ARGUMENT on
// gRPC (the same code the Schema flavor uses; the envelope's semantic string
// disambiguates).
type UnsupportedMediaTypeNotification struct {
	domain.ApplicationNotificationBase
}

func (UnsupportedMediaTypeNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnsupportedMediaType
}

// NotImplementedNotification is the "the server does not do this" refusal,
// with two authors. A SERVICE emits it when a route exists and the request is
// valid but the capability behind it is not built yet. The web ErrorHandler
// emits it when Fiber reports an unsupported HTTP request method — a verb this
// server implements nowhere, which is the same statement one layer down.
// Distinct from MethodNotAllowedNotification (405), which says the path is
// registered for other verbs but not this one; a 501 says the verb (or the
// capability) is not served at all. Carries SemanticNotImplemented -> 501 on
// HTTP and UNIMPLEMENTED on gRPC.
type NotImplementedNotification struct {
	domain.ApplicationNotificationBase
}

func (NotImplementedNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticNotImplemented
}

// BadGatewayNotification is the upstream-answered-garbage refusal: this
// service reached a dependency it composes over (an httpclient endpoint, a
// gRPC peer, an upstream a view subscribes to) and got back a response it
// cannot use — a broken payload, an unusable status, a contract the peer no
// longer honors. Distinct from ServiceUnavailableNotification (503), which is
// THIS service declining to serve, and from RequestTimeoutNotification (504),
// which is the deadline elapsing: a 502 means the conversation completed and
// the answer was wrong. Carries SemanticBadGateway -> 502 on HTTP and
// UNAVAILABLE on gRPC.
type BadGatewayNotification struct {
	domain.ApplicationNotificationBase
}

func (BadGatewayNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticBadGateway
}

// PaymentRequiredNotification is the billing / quota gate: the request is
// well-formed and the caller is authenticated, and the account has to settle
// up before it is served. Distinct from TooManyRequestsNotification (429),
// which is a rate the caller merely has to wait out, and from
// MissingPermissionNotification (403), which no payment would lift.
// Carries SemanticPaymentRequired -> 402 on HTTP and PERMISSION_DENIED on gRPC.
type PaymentRequiredNotification struct {
	domain.ApplicationNotificationBase
}

func (PaymentRequiredNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticPaymentRequired
}

// NotAcceptableNotification is the response half of content negotiation: the
// endpoint can produce nothing that satisfies the request's Accept header. The
// request half is UnsupportedMediaTypeNotification (415) — that one refuses
// what the client SENT, this one refuses what the client asked to RECEIVE.
// The framework negotiates no media type of its own (an export route binds its
// format at construction), so this is vocabulary for a service that does.
// Carries SemanticNotAcceptable -> 406 on HTTP and INVALID_ARGUMENT on gRPC.
type NotAcceptableNotification struct {
	domain.ApplicationNotificationBase
}

func (NotAcceptableNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticNotAcceptable
}

// RangeNotSatisfiableNotification is the byte-range refusal: the Range the
// client asked for does not exist in the representation being served. Relevant
// on file-shaped responses — the tabular export surface serves one, with
// Content-Disposition and a concrete Content-Type. Carries
// SemanticRangeNotSatisfiable -> 416 on HTTP and OUT_OF_RANGE on gRPC, the one
// connect code that means exactly this.
type RangeNotSatisfiableNotification struct {
	domain.ApplicationNotificationBase
}

func (RangeNotSatisfiableNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticRangeNotSatisfiable
}

// ResourceLockedNotification is the temporary-hold refusal: someone else holds
// the resource right now and the caller should come back. The distinction from
// the two 409 flavors is RETRYABILITY, and it is the whole reason this exists:
// EntityAlreadyAddedNotification (Conflict) and ConcurrentModificationNotification
// (StateConflict) both mean "reload and reconcile — repeating this request
// unchanged will fail again", while a lock means "repeat it later and it will
// work". Pairs with Retry-After the same way 429 does.
// Carries SemanticLocked -> 423 on HTTP and ABORTED on gRPC.
type ResourceLockedNotification struct {
	domain.ApplicationNotificationBase
}

func (ResourceLockedNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticLocked
}

// PreconditionRequiredNotification is the other half of the conditional-request
// story: PreconditionFailedNotification (412) answers a condition the client
// STATED and that no longer holds; this one refuses a request that had to carry
// a condition (If-Match, If-Unmodified-Since) and carried none. It is what
// closes the lost-update hole a 412-only service still has — without it, a
// client that simply omits the header is never told to send one.
// Carries SemanticPreconditionRequired -> 428 on HTTP and FAILED_PRECONDITION
// on gRPC.
type PreconditionRequiredNotification struct {
	domain.ApplicationNotificationBase
}

func (PreconditionRequiredNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticPreconditionRequired
}

// UnavailableForLegalReasonsNotification is the withheld-by-law refusal: a
// court order, a takedown, a geographic restriction. Distinct from the 403
// family in WHO it is about — a 403 says THIS caller may not, and some other
// caller might; a 451 says nobody may have this here, and no permission the
// caller could hold would change it. Carries
// SemanticUnavailableForLegalReasons -> 451 on HTTP and PERMISSION_DENIED on
// gRPC (the ErrorInfo.reason disambiguates from the 403 flavor).
type UnavailableForLegalReasonsNotification struct {
	domain.ApplicationNotificationBase
}

func (UnavailableForLegalReasonsNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticUnavailableForLegalReasons
}

// InsufficientStorageNotification is the storage-allowance refusal: the writer
// has no room left for what it is trying to persist. The useful reading is a
// QUOTA (a tenant past its allowance), not a full disk — a genuinely full
// volume surfaces as the infrastructure exception it is, through the 500 path.
// Distinct from TooManyRequestsNotification (429), which meters a rate, and
// from PaymentRequiredNotification (402), which meters an account.
// Carries SemanticInsufficientStorage -> 507 on HTTP and RESOURCE_EXHAUSTED on
// gRPC.
type InsufficientStorageNotification struct {
	domain.ApplicationNotificationBase
}

func (InsufficientStorageNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticInsufficientStorage
}

// RequestHeaderFieldsTooLargeNotification is emitted by the web ErrorHandler
// when the header block or the request line does not fit the server's read
// buffer — fasthttp raises ErrSmallBuffer while parsing and Fiber normalizes it
// to this status before the app's error handler runs. It is the header sibling
// of PayloadTooLargeNotification (413), which meters the body. FieldName
// carries the METHOD /path of the rejected request. Carries
// SemanticRequestHeaderFieldsTooLarge -> 431 on HTTP and RESOURCE_EXHAUSTED on
// gRPC.
type RequestHeaderFieldsTooLargeNotification struct {
	domain.ApplicationNotificationBase
}

func (RequestHeaderFieldsTooLargeNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticRequestHeaderFieldsTooLarge
}

// MalformedRequestNotification is emitted by the web ErrorHandler when the
// request could not be read as HTTP at all — Fiber's catch-all for a parse
// failure it has no more specific status for. Deliberately NOT
// SchemaViolationNotification: that type's contract is about the request
// PAYLOAD not matching the expected shape, and reusing it here would make a
// documented domain contract false. The underlying parse error never reaches
// the wire; FieldName carries the METHOD /path. Carries the same SemanticSchema
// -> 400 the payload flavor does.
type MalformedRequestNotification struct {
	domain.ApplicationNotificationBase
}

func (MalformedRequestNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticSchema
}

// MissingPermissionNotification is emitted by Mount/MountRaw's runtime gate
// when the request's Identity does not carry the required permission declared
// via fwopenapi.RequirePermission. The required permission string is carried
// as the notification's FieldValue so the wire response surfaces it (the
// FieldName is "permission" by convention).
type MissingPermissionNotification struct {
	domain.ApplicationNotificationBase
}

func (MissingPermissionNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticForbidden
}

// TenantMissingNotification is emitted by the auth middleware when
// authorization.tenant.required: true is set and the authenticated Identity
// carries no tenant claim. Short-circuits the request before reaching any
// handler — the principal cannot be scoped to a tenant the framework does not
// know.
type TenantMissingNotification struct {
	domain.ApplicationNotificationBase
}

func (TenantMissingNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticForbidden
}

// TenantMismatchNotification is a convenience type service code can emit
// inside BuildRules or Query.ToCriteria(ctx) when the resource's tenant id
// does not match the requesting principal's. The framework does not trigger
// it automatically (tenant scoping lives in application/domain, not in
// infra) — it is offered here so services do not need to declare their own
// tenant-mismatch type per aggregate.
type TenantMismatchNotification struct {
	domain.ApplicationNotificationBase
}

func (TenantMismatchNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticForbidden
}

// FieldAccessForbiddenNotification is emitted by ReadCriteria.Restrict when a
// read ACTIVELY references a field the requesting principal may not see — a
// ?orderBy=, a filter key, or explicit ?fields= on a field the Query restricted in
// ToCriteria. The field is removed from the read either way; the 403 marks the
// active attempt (a passively-omitted field gets no notification, just absence).
// The restricted Go field path is carried as the notification's FieldName.
type FieldAccessForbiddenNotification struct {
	domain.ApplicationNotificationBase
}

func (FieldAccessForbiddenNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticForbidden
}
