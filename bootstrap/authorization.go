package bootstrap

import "github.com/ClaudioSchirmer/omnicore/application/configuration"

// applyAuthorizationConfig threads the authz claim names from yaml into the
// package-level slots Identity.HasPermission and Identity.TenantID consult
// per request. Called by buildApp before AuthMiddleware so the very first
// request sees the configured claim names.
//
// When the yaml carries no authorization block (cfg.Auth.Authorization is
// nil), the framework defaults stand — "permissions" and "tenant_id" — and
// no setter call is needed. The function is idempotent: calling it twice
// with the same cfg has no observable side effect.
func applyAuthorizationConfig(authz *AuthorizationConfig) {
	if authz == nil {
		return
	}
	configuration.SetPermissionsClaim(authz.PermissionsClaim)
	configuration.SetTenantClaim(authz.Tenant.Claim)
}
