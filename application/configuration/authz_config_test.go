package configuration

import "testing"

func TestSetPermissionsClaim_Default(t *testing.T) {
	if got := permissionsClaimName(); got != defaultPermissionsClaim {
		t.Errorf("default permissions claim = %q, want %q", got, defaultPermissionsClaim)
	}
}

func TestSetPermissionsClaim_Override(t *testing.T) {
	defer SetPermissionsClaim("") // restore
	SetPermissionsClaim("scope")
	if got := permissionsClaimName(); got != "scope" {
		t.Errorf("after Set(\"scope\"), get = %q", got)
	}
}

func TestSetPermissionsClaim_EmptyRestoresDefault(t *testing.T) {
	SetPermissionsClaim("scope")
	SetPermissionsClaim("")
	if got := permissionsClaimName(); got != defaultPermissionsClaim {
		t.Errorf("Set(\"\") must restore default, got %q", got)
	}
}

func TestSetTenantClaim_Default(t *testing.T) {
	if got := tenantClaimName(); got != defaultTenantClaim {
		t.Errorf("default tenant claim = %q, want %q", got, defaultTenantClaim)
	}
}

func TestSetTenantClaim_Override(t *testing.T) {
	defer SetTenantClaim("") // restore
	SetTenantClaim("org")
	if got := tenantClaimName(); got != "org" {
		t.Errorf("after Set(\"org\"), get = %q", got)
	}
}

func TestSetTenantClaim_EmptyRestoresDefault(t *testing.T) {
	SetTenantClaim("org")
	SetTenantClaim("")
	if got := tenantClaimName(); got != defaultTenantClaim {
		t.Errorf("Set(\"\") must restore default, got %q", got)
	}
}
