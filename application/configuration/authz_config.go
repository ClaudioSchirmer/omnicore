package configuration

import "sync"

// Default JWT claim names consumed by Identity helpers. Overridable via the
// SetPermissionsClaim / SetTenantClaim package-level setters, which
// bootstrap.Run invokes from authorization.permissionsClaim and
// authorization.tenant.claim respectively.
const (
	defaultPermissionsClaim = "permissions"
	defaultTenantClaim      = "tenant_id"
)

var (
	authzMu             sync.RWMutex
	permissionsClaimCfg = defaultPermissionsClaim
	tenantClaimCfg      = defaultTenantClaim
)

// SetPermissionsClaim configures the JWT claim name Identity.HasPermission
// reads. Called by bootstrap.Run from authorization.permissionsClaim before
// any feature mounts. Passing an empty string restores the default
// ("permissions"). Idempotent and concurrent-safe; in practice called once
// per process at boot.
func SetPermissionsClaim(name string) {
	authzMu.Lock()
	defer authzMu.Unlock()
	if name == "" {
		permissionsClaimCfg = defaultPermissionsClaim
		return
	}
	permissionsClaimCfg = name
}

// SetTenantClaim configures the JWT claim name Identity.TenantID reads.
// Same lifecycle rules as SetPermissionsClaim.
func SetTenantClaim(name string) {
	authzMu.Lock()
	defer authzMu.Unlock()
	if name == "" {
		tenantClaimCfg = defaultTenantClaim
		return
	}
	tenantClaimCfg = name
}

// permissionsClaimName / tenantClaimName are the read paths Identity
// helpers consult. RLock is cheap (uncontended at runtime — setters are
// boot-only) so consumers can call HasPermission / TenantID per request
// without measurable overhead.
func permissionsClaimName() string {
	authzMu.RLock()
	defer authzMu.RUnlock()
	return permissionsClaimCfg
}

func tenantClaimName() string {
	authzMu.RLock()
	defer authzMu.RUnlock()
	return tenantClaimCfg
}
