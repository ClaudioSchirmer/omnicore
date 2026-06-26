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
// ?sort=, ?filters=, or explicit ?fields= on a field the Query restricted in
// ToCriteria. The field is removed from the read either way; the 403 marks the
// active attempt (a passively-omitted field gets no notification, just absence).
// The restricted Go field path is carried as the notification's FieldName.
type FieldAccessForbiddenNotification struct {
	domain.ApplicationNotificationBase
}

func (FieldAccessForbiddenNotification) Semantic() domain.NotificationSemantic {
	return domain.SemanticForbidden
}
