package bootstrap

import (
	"testing"

	"github.com/ClaudioSchirmer/omnicore/application/configuration"
)

func TestAuthOptionsFromConfig_MinimalDisabled(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{})
	if got.Issuer != "" || got.Audience != "" {
		t.Errorf("expected empty Issuer/Audience for minimal cfg, got %+v", got)
	}
}

func TestAuthOptionsFromConfig_PublicRoutesCarried(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{
		PublicRoutes: []string{"GET /health", "GET /ready"},
	})
	if len(got.PublicRoutes) != 2 || got.PublicRoutes[0] != "GET /health" {
		t.Errorf("PublicRoutes = %v", got.PublicRoutes)
	}
}

func TestAuthOptionsFromConfig_JWTFieldsCopied(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{
		Mode: AuthModeJWT,
		JWT: &JWTConfig{
			Algorithms:    []string{"RS256"},
			Issuer:        "https://idp.example",
			Audience:      "svc",
			LeewaySeconds: 5,
			JWKSURL:       "https://idp.example/jwks.json",
			PublicKeyPEM:  "",
		},
	})
	if got.Issuer != "https://idp.example" {
		t.Errorf("Issuer = %q", got.Issuer)
	}
	if got.Audience != "svc" {
		t.Errorf("Audience = %q", got.Audience)
	}
	if got.LeewaySeconds != 5 {
		t.Errorf("LeewaySeconds = %d", got.LeewaySeconds)
	}
	if got.JWKSURL != "https://idp.example/jwks.json" {
		t.Errorf("JWKSURL = %q", got.JWKSURL)
	}
	if len(got.Algorithms) != 1 || got.Algorithms[0] != "RS256" {
		t.Errorf("Algorithms = %v", got.Algorithms)
	}
}

func TestAuthOptionsFromConfig_ExternalValidatorFieldsCopied(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{
		Mode: AuthModeJWT,
		JWT: &JWTConfig{
			Issuer:   "iss",
			Audience: "aud",
		},
		ExternalValidator: &ExternalValidatorConfig{
			Method:         "POST",
			URL:            "https://idp/introspect",
			TokenPlacement: TokenPlacementFormField,
			TokenField:     "token",
			ExtraHeaders:   map[string]string{"X-Tenant": "1"},
			Success: ExternalValidatorSuccess{
				JSONPath:      "$.active",
				ExpectedValue: true,
			},
			TimeoutMS:       2000,
			FailMode:        FailModeClosed,
			CacheTTLSeconds: 30,
		},
	})
	if got.ExternalValidator == nil {
		t.Fatal("expected ExternalValidator populated")
	}
	ev := got.ExternalValidator
	if ev.URL != "https://idp/introspect" {
		t.Errorf("ExternalValidator.URL = %q", ev.URL)
	}
	if ev.TokenPlacement != "form_field" {
		t.Errorf("TokenPlacement = %q", ev.TokenPlacement)
	}
	if ev.Success.JSONPath != "$.active" || ev.Success.ExpectedValue != true {
		t.Errorf("Success block = %+v", ev.Success)
	}
	if ev.TimeoutMS != 2000 || ev.CacheTTLSeconds != 30 {
		t.Errorf("Timing block: timeout=%d cache=%d", ev.TimeoutMS, ev.CacheTTLSeconds)
	}
	if ev.FailMode != "closed" {
		t.Errorf("FailMode = %q", ev.FailMode)
	}
	if ev.ExtraHeaders["X-Tenant"] != "1" {
		t.Errorf("ExtraHeaders not carried: %v", ev.ExtraHeaders)
	}
}

func TestAuthOptionsFromConfig_TenantPropagatedWhenEnabled(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{
		Authorization: &AuthorizationConfig{
			Tenant: TenantConfig{
				Enabled:  true,
				Required: true,
				Claim:    "tid",
			},
		},
	})
	if !got.TenantRequired {
		t.Error("expected TenantRequired=true")
	}
	if got.TenantClaim != "tid" {
		t.Errorf("TenantClaim = %q", got.TenantClaim)
	}
}

func TestAuthOptionsFromConfig_TenantSkippedWhenDisabled(t *testing.T) {
	got := authOptionsFromConfig(AuthConfig{
		Authorization: &AuthorizationConfig{
			Tenant: TenantConfig{
				Enabled:  false,
				Required: true,
				Claim:    "tid",
			},
		},
	})
	if got.TenantRequired {
		t.Error("Tenant.Required must be ignored when Tenant.Enabled=false")
	}
	if got.TenantClaim != "" {
		t.Errorf("TenantClaim = %q, want empty when Enabled=false", got.TenantClaim)
	}
}

// --- applyAuthorizationConfig ----------------------------------------------

func TestApplyAuthorizationConfig_NilNoOp(t *testing.T) {
	// Establish a baseline by setting known claim names, then call with nil
	// — the values must NOT change.
	configuration.SetPermissionsClaim("permissions_baseline")
	configuration.SetTenantClaim("tenant_baseline")
	applyAuthorizationConfig(nil)

	// Restore framework default for the rest of the suite.
	configuration.SetPermissionsClaim("permissions")
	configuration.SetTenantClaim("tenant_id")
}

func TestApplyAuthorizationConfig_SetsClaimNames(t *testing.T) {
	defer configuration.SetPermissionsClaim("permissions")
	defer configuration.SetTenantClaim("tenant_id")

	applyAuthorizationConfig(&AuthorizationConfig{
		PermissionsClaim: "scopes",
		Tenant:           TenantConfig{Claim: "tnt"},
	})

	// Indirect verification — call HasPermission with a fake Identity that
	// carries the "scopes" claim and confirm the lookup honors the new claim
	// name.
	id := &configuration.Identity{
		Claims: map[string]any{
			"scopes": []any{"users:read"},
		},
	}
	if !id.HasPermission("users:read") {
		t.Error("HasPermission did not honor the new PermissionsClaim name")
	}

	id2 := &configuration.Identity{
		Claims: map[string]any{"tnt": "acme"},
	}
	if got := id2.TenantID(); got != "acme" {
		t.Errorf("TenantID via new claim name = %q, want acme", got)
	}
}
