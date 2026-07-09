package notifications

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/domain"
)

// TestAuthorizationNotifications_Semantic guards the contract that all three
// authorization notifications carry SemanticForbidden — the wire layer reads
// it to emit a 403, and a wrong Semantic would silently surface as 422 or
// some other status. Cheap regression net.
func TestAuthorizationNotifications_Semantic(t *testing.T) {
	cases := []struct {
		name string
		n    domain.Notification
	}{
		{"MissingPermission", MissingPermissionNotification{}},
		{"TenantMissing", TenantMissingNotification{}},
		{"TenantMismatch", TenantMismatchNotification{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.n.Semantic(); got != domain.SemanticForbidden {
				t.Errorf("%s.Semantic() = %v, want SemanticForbidden", tc.name, got)
			}
		})
	}
}

// TestKernelNotifications_Semantic locks the typed Semantic() of every
// application-layer kernel notification. The wire layer maps Semantic→HTTP
// status; a regression here silently breaks the contract documented in
// "HTTP status mapping".
func TestKernelNotifications_Semantic(t *testing.T) {
	cases := []struct {
		name string
		n    domain.Notification
		want domain.NotificationSemantic
	}{
		{"ServiceUnavailable", ServiceUnavailableNotification{}, domain.SemanticUnavailable},
		{"MissingAuthorization", MissingAuthorizationNotification{}, domain.SemanticUnauthorized},
		{"InvalidToken", InvalidTokenNotification{}, domain.SemanticUnauthorized},
		{"ExpiredToken", ExpiredTokenNotification{}, domain.SemanticUnauthorized},
		{"InternalServerError", InternalServerErrorNotification{}, domain.SemanticInternal},
		{"RouteNotFound", RouteNotFoundNotification{}, domain.SemanticNotFound},
		{"MethodNotAllowed", MethodNotAllowedNotification{}, domain.SemanticMethodNotAllowed},
		{"PayloadTooLarge", PayloadTooLargeNotification{}, domain.SemanticPayloadTooLarge},
		{"RequestTimeout", RequestTimeoutNotification{}, domain.SemanticGatewayTimeout},
		{"ReadTimeout", ReadTimeoutNotification{}, domain.SemanticRequestTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.n.Semantic(); got != tc.want {
				t.Errorf("%s.Semantic() = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// TestContextNotInitialized_DefaultsToValidation asserts the no-override type
// inherits SemanticValidation from ApplicationNotificationBase — the default
// 422 path.
func TestContextNotInitialized_DefaultsToValidation(t *testing.T) {
	n := ContextNotInitializedNotification{}
	if got := n.Semantic(); got != domain.SemanticValidation {
		t.Errorf("ContextNotInitializedNotification.Semantic() = %v, want SemanticValidation", got)
	}
}

// TestKernelNotifications_NotificationKey guards the typed identity each
// notification surfaces — clients branch UI on this string and translation
// catalogs key off it. Reading the actual function the wire layer calls
// (NotificationKey) avoids drift if its derivation ever changes.
func TestKernelNotifications_NotificationKey(t *testing.T) {
	cases := []struct {
		n    domain.Notification
		want string
	}{
		{ContextNotInitializedNotification{}, "ContextNotInitializedNotification"},
		{ServiceUnavailableNotification{}, "ServiceUnavailableNotification"},
		{MissingAuthorizationNotification{}, "MissingAuthorizationNotification"},
		{InvalidTokenNotification{}, "InvalidTokenNotification"},
		{ExpiredTokenNotification{}, "ExpiredTokenNotification"},
		{InternalServerErrorNotification{}, "InternalServerErrorNotification"},
		{RouteNotFoundNotification{}, "RouteNotFoundNotification"},
		{MethodNotAllowedNotification{}, "MethodNotAllowedNotification"},
		{PayloadTooLargeNotification{}, "PayloadTooLargeNotification"},
		{ReadTimeoutNotification{}, "ReadTimeoutNotification"},
		{MissingPermissionNotification{}, "MissingPermissionNotification"},
		{TenantMissingNotification{}, "TenantMissingNotification"},
		{TenantMismatchNotification{}, "TenantMismatchNotification"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := domain.NotificationKey(tc.n); got != tc.want {
				t.Errorf("NotificationKey() = %q, want %q", got, tc.want)
			}
		})
	}
}
