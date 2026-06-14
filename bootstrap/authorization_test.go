package bootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// --- YAML parsing of AuthorizationConfig + TenantConfig ---------------------

func unmarshalAuthorization(t *testing.T, src string) (*AuthorizationConfig, error) {
	t.Helper()
	var a AuthorizationConfig
	err := yaml.Unmarshal([]byte(src), &a)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func TestAuthorizationConfig_KnownKeysAccepted(t *testing.T) {
	src := `
enabled: true
permissionsClaim: scope
tenant:
  enabled: true
  claim: org
  required: true
`
	a, err := unmarshalAuthorization(t, src)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !a.Enabled {
		t.Error("Enabled = false; want true")
	}
	if a.PermissionsClaim != "scope" {
		t.Errorf("PermissionsClaim = %q", a.PermissionsClaim)
	}
	if !a.Tenant.Enabled || a.Tenant.Claim != "org" || !a.Tenant.Required {
		t.Errorf("Tenant = %+v", a.Tenant)
	}
}

func TestAuthorizationConfig_UnknownKeyRejected(t *testing.T) {
	// Typo: "permissionClaim" instead of "permissionsClaim"
	src := `
enabled: true
permissionClaim: scope
`
	_, err := unmarshalAuthorization(t, src)
	if err == nil {
		t.Fatal("expected error on unknown key")
	}
	if !strings.Contains(err.Error(), "permissionClaim") {
		t.Errorf("error should name offending key, got: %v", err)
	}
}

func TestTenantConfig_UnknownKeyRejected(t *testing.T) {
	src := `
enabled: true
claim: org
mandatory: true
`
	var tc TenantConfig
	err := yaml.Unmarshal([]byte(src), &tc)
	if err == nil {
		t.Fatal("expected error on unknown tenant key")
	}
	if !strings.Contains(err.Error(), "mandatory") {
		t.Errorf("error should name offending key, got: %v", err)
	}
}

// --- validate cross-field rules ---------------------------------------------

func TestAuthorizationConfig_Validate_TenantRequiredWithoutEnabledFails(t *testing.T) {
	a := AuthorizationConfig{
		Enabled: true,
		Tenant:  TenantConfig{Enabled: false, Required: true},
	}
	if err := a.validate(); err == nil {
		t.Fatal("expected error: required without enabled")
	}
}

func TestAuthorizationConfig_Validate_TenantRequiredWithEnabledPasses(t *testing.T) {
	a := AuthorizationConfig{
		Enabled: true,
		Tenant:  TenantConfig{Enabled: true, Required: true},
	}
	if err := a.validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAuthConfig_Validate_AuthzEnabledWithModeDisabledFails(t *testing.T) {
	a := AuthConfig{
		Mode:          AuthModeDisabled,
		Authorization: &AuthorizationConfig{Enabled: true},
	}
	if err := a.validate(); err == nil {
		t.Fatal("expected error: authz.enabled requires auth.mode=jwt")
	}
}

func TestAuthConfig_Validate_AuthzDisabledWithModeDisabledPasses(t *testing.T) {
	a := AuthConfig{
		Mode:          AuthModeDisabled,
		Authorization: &AuthorizationConfig{Enabled: false},
	}
	if err := a.validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- applyDefaults ----------------------------------------------------------

func TestAuthorizationConfig_ApplyDefaults_FillsClaims(t *testing.T) {
	a := AuthorizationConfig{}
	a.applyDefaults()
	if a.PermissionsClaim != "permissions" {
		t.Errorf("PermissionsClaim default = %q, want %q", a.PermissionsClaim, "permissions")
	}
	if a.Tenant.Claim != "tenant_id" {
		t.Errorf("Tenant.Claim default = %q, want %q", a.Tenant.Claim, "tenant_id")
	}
}

func TestAuthorizationConfig_ApplyDefaults_PreservesExplicit(t *testing.T) {
	a := AuthorizationConfig{
		PermissionsClaim: "custom",
		Tenant:           TenantConfig{Claim: "org"},
	}
	a.applyDefaults()
	if a.PermissionsClaim != "custom" {
		t.Errorf("PermissionsClaim = %q, want %q", a.PermissionsClaim, "custom")
	}
	if a.Tenant.Claim != "org" {
		t.Errorf("Tenant.Claim = %q, want %q", a.Tenant.Claim, "org")
	}
}
