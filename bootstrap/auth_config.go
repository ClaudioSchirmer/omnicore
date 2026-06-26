package bootstrap

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// AuthMode selects the request-authentication behavior wired by the framework.
//
//   - AuthModeJWT     — middleware validates the bearer JWT against the configured
//     keys and, when ExternalValidator is set, also calls the IdP for revocation.
//   - AuthModeDisabled — middleware is bypassed. Allowed only when APP_PROFILE="dev";
//     LoadConfig rejects it under any other profile (prd or QA variants) so a
//     non-dev boot cannot ship without auth wired.
type AuthMode string

const (
	AuthModeJWT      AuthMode = "jwt"
	AuthModeDisabled AuthMode = "disabled"
)

// TokenPlacement names where the external validator request carries the token
// being introspected. Matches Keycloak (FormField), Auth0/custom Bearer-style
// APIs (BearerHeader), JSON body APIs (JSONBody), and legacy query-string APIs
// (QueryParam).
type TokenPlacement string

const (
	TokenPlacementBearerHeader TokenPlacement = "bearer_header"
	TokenPlacementFormField    TokenPlacement = "form_field"
	TokenPlacementJSONBody     TokenPlacement = "json_body"
	TokenPlacementQueryParam   TokenPlacement = "query_param"
)

// FailMode controls how the middleware reacts when the external validator
// itself errors (timeout, unreachable, non-2xx). "closed" rejects the request
// (safer); "open" trusts the local pre-validation result (more available).
type FailMode string

const (
	FailModeClosed FailMode = "closed"
	FailModeOpen   FailMode = "open"
)

// Default JWT signing algorithms allow-listed when none are declared. Only
// asymmetric algorithms — symmetric (HMAC) is intentionally excluded so the
// service never holds the IdP's signing secret.
var defaultJWTAlgorithms = []string{"RS256", "ES256", "EdDSA"}

// AuthConfig is the `auth:` block of microservice.<profile>.yaml.
type AuthConfig struct {
	// Mode selects the runtime behavior. Defaults to AuthModeDisabled when the
	// `auth:` block is absent, which forces operators to enable JWT explicitly
	// before promoting to prd (the profile guard catches the omission at boot).
	Mode AuthMode `yaml:"mode"`

	// JWT carries the local pre-validation knobs. Required when Mode == jwt.
	JWT *JWTConfig `yaml:"jwt,omitempty"`

	// ExternalValidator is optional even when Mode == jwt. When set, every
	// authenticated request fires an introspection call to the IdP after the
	// local pre-validation passes; results are NOT cached so revocation is
	// honored immediately.
	ExternalValidator *ExternalValidatorConfig `yaml:"externalValidator,omitempty"`

	// PublicRoutes lists endpoints that bypass the auth middleware entirely
	// (health/ready probes, login endpoints, etc.). Each entry is "METHOD /path"
	// — exact match, no globs in this phase.
	PublicRoutes []string `yaml:"publicRoutes,omitempty"`

	// AuditClaims is the allowlist of JWT claim names surfaced in the audit
	// log's actorClaims block. Empty (default) keeps the actorClaims field
	// absent from audit lines — Subject and Issuer still appear on their own.
	// Use this to capture custom claims that matter for audit forensics
	// (tenant_id, roles, on-behalf-of, etc.) without dumping the entire
	// claim payload into log storage.
	AuditClaims []string `yaml:"auditClaims,omitempty"`

	// Authorization carries the declarative authz layer wiring — permission
	// claim name + tenant gate. nil (default) leaves the layer off; the
	// runtime gate on Mount/MountRaw still works but no boot-time enforcement
	// runs and the AuthMiddleware does not consult TenantRequired. Operators
	// opt in by declaring `authorization:` (even an empty block enables it
	// via UnmarshalYAML allocating the struct).
	Authorization *AuthorizationConfig `yaml:"authorization,omitempty"`
}

// AuthorizationConfig is the `auth.authorization:` sub-block. Drives the boot
// scan (every non-public route must declare RequirePermission) and the per-
// request Identity.HasPermission/TenantID helpers. Keys outside the closed
// set below are rejected by UnmarshalYAML so a typo like permissionClaim vs
// permissionsClaim surfaces at boot, not as a silent miss.
type AuthorizationConfig struct {
	// Enabled toggles the boot scan that requires every non-public route to
	// declare fwopenapi.RequirePermission(...). The runtime gate that wraps
	// individual handlers runs regardless of this flag — annotating routes
	// before flipping the switch is the canonical rollout path.
	Enabled bool `yaml:"enabled"`

	// PermissionsClaim names the JWT claim Identity.HasPermission reads.
	// Default "permissions" (Auth0/AWS/Stripe convention). Empty falls back
	// to the default.
	PermissionsClaim string `yaml:"permissionsClaim,omitempty"`

	// Tenant configures the multi-tenancy sub-feature — claim name +
	// presence enforcement. Zero-value disables tenancy entirely.
	Tenant TenantConfig `yaml:"tenant,omitempty"`
}

// TenantConfig is the `auth.authorization.tenant:` sub-block. Splits the
// "tenant exists" knob from the "tenant is mandatory" knob so a service can
// opt into tenant scoping for reads/writes without forcing every request to
// carry the claim.
type TenantConfig struct {
	// Enabled toggles Identity.TenantID() lookup against the configured
	// Claim. When false the helper returns "" regardless of claim presence —
	// services not using tenancy avoid the lookup entirely.
	Enabled bool `yaml:"enabled"`

	// Claim names the JWT claim TenantID reads. Default "tenant_id". Empty
	// falls back to the default.
	Claim string `yaml:"claim,omitempty"`

	// Required makes the AuthMiddleware reject any non-public request whose
	// Identity has an empty TenantID — emits TenantMissingNotification (403)
	// before any handler runs. Allowed only when Enabled=true; the loader
	// rejects required=true with enabled=false at boot.
	Required bool `yaml:"required"`
}

// UnmarshalYAML for AuthorizationConfig enforces the closed key set.
// Catches typos at boot (e.g. permissionClaim vs permissionsClaim).
func (a *AuthorizationConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias AuthorizationConfig
	if err := node.Decode((*alias)(a)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "auth.authorization",
		"enabled", "permissionsClaim", "tenant")
}

// UnmarshalYAML for TenantConfig enforces the closed key set under tenant.
func (t *TenantConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias TenantConfig
	if err := node.Decode((*alias)(t)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "auth.authorization.tenant",
		"enabled", "claim", "required")
}

// rejectUnknownYAMLFields walks the mapping-node's content (key/value pairs)
// and returns an error naming any key outside `allowed`. Used by sub-blocks
// where typo-catching is more valuable than YAML's default permissive
// behavior — currently only authorization + tenant.
func rejectUnknownYAMLFields(node *yaml.Node, blockPath string, allowed ...string) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	var unknown []string
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i].Value
		if _, ok := set[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%s: unknown key(s) %s (allowed: %s)",
			blockPath,
			strings.Join(quoteEach(unknown), ", "),
			strings.Join(quoteEach(allowed), ", "))
	}
	return nil
}

func quoteEach(items []string) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// JWTConfig describes how to locally verify the bearer JWT before any external
// call. Exactly one of JWKSURL / PublicKeyPEM must be set when Mode == jwt.
type JWTConfig struct {
	Algorithms    []string `yaml:"algorithms,omitempty"`
	Issuer        string   `yaml:"issuer"`
	Audience      string   `yaml:"audience"`
	LeewaySeconds int      `yaml:"leewaySeconds,omitempty"`

	// JWKSURL points at the IdP's JWKS endpoint (preferred — supports key
	// rotation). Mutually exclusive with PublicKeyPEM.
	JWKSURL string `yaml:"jwksUrl,omitempty"`

	// PublicKeyPEM carries the verification key inline (PEM-encoded). Mutually
	// exclusive with JWKSURL.
	PublicKeyPEM string `yaml:"publicKeyPem,omitempty"`
}

// ExternalValidatorConfig describes the per-request HTTP call to the IdP that
// catches revocation. Cache is opt-in via CacheTTLSeconds and default-off: the
// purpose of this call is to detect revocation, and a positive cache trades
// some revocation latency (up to TTL seconds) for fewer IdP round-trips.
type ExternalValidatorConfig struct {
	Method         string                   `yaml:"method"`
	URL            string                   `yaml:"url"`
	TokenPlacement TokenPlacement           `yaml:"tokenPlacement"`
	TokenField     string                   `yaml:"tokenField,omitempty"`
	ExtraHeaders   map[string]string        `yaml:"extraHeaders,omitempty"`
	Success        ExternalValidatorSuccess `yaml:"success"`
	TimeoutMS      int                      `yaml:"timeoutMs,omitempty"`
	FailMode       FailMode                 `yaml:"failMode,omitempty"`

	// CacheTTLSeconds, when > 0, enables an in-memory positive-only cache of
	// successful validator answers, keyed by the SHA-256 hash of the bearer
	// token. Negative answers and transport errors are NEVER cached so a
	// revocation hits the IdP on the next request. Default 0 (cache off) —
	// operators opt in explicitly when they have measured the IdP cost and
	// accept the revocation window of up to TTL seconds.
	CacheTTLSeconds int `yaml:"cacheTtlSeconds,omitempty"`
}

// ExternalValidatorSuccess declares how the framework decides whether the IdP
// considers the token still valid. JSONPath is matched against the response
// body; ExpectedValue is what the field must equal (typically `true`).
type ExternalValidatorSuccess struct {
	JSONPath      string `yaml:"jsonPath"`
	ExpectedValue any    `yaml:"expectedValue"`
}

// applyDefaults fills in opinionated defaults so consumers only declare what
// differs from the canonical shape. Called by Config.applyDefaults.
func (a *AuthConfig) applyDefaults() {
	if a.Mode == "" {
		a.Mode = AuthModeDisabled
	}
	if a.Mode != AuthModeJWT {
		return
	}
	if a.JWT != nil {
		if len(a.JWT.Algorithms) == 0 {
			a.JWT.Algorithms = append(a.JWT.Algorithms, defaultJWTAlgorithms...)
		}
	}
	if a.ExternalValidator != nil {
		if a.ExternalValidator.Method == "" {
			a.ExternalValidator.Method = "POST"
		}
		if a.ExternalValidator.FailMode == "" {
			a.ExternalValidator.FailMode = FailModeClosed
		}
		if a.ExternalValidator.TimeoutMS == 0 {
			a.ExternalValidator.TimeoutMS = 2000
		}
	}
	if a.Authorization != nil {
		a.Authorization.applyDefaults()
	}
}

// applyDefaults fills the claim names that downstream code looks up under
// stable default keys. Empty PermissionsClaim and Tenant.Claim resolve to
// the same defaults Identity.HasPermission/TenantID use, so leaving them
// blank yields the canonical Auth0/Keycloak shape ("permissions" and
// "tenant_id" respectively).
func (a *AuthorizationConfig) applyDefaults() {
	if a.PermissionsClaim == "" {
		a.PermissionsClaim = "permissions"
	}
	if a.Tenant.Claim == "" {
		a.Tenant.Claim = "tenant_id"
	}
}

// validate enforces the schema invariants of the auth block. Profile-aware
// guards (e.g., AuthModeDisabled forbidden under prd) live in load.go.
func (a *AuthConfig) validate() error {
	switch a.Mode {
	case AuthModeDisabled:
		// authorization.enabled requires authentication — a service that
		// disables auth cannot enforce per-route permissions.
		if a.Authorization != nil && a.Authorization.Enabled {
			return fmt.Errorf("auth.authorization.enabled=true requires auth.mode=%q (got %q)", AuthModeJWT, a.Mode)
		}
		return nil
	case AuthModeJWT:
		if err := a.validateJWTMode(); err != nil {
			return err
		}
		if a.Authorization != nil {
			return a.Authorization.validate()
		}
		return nil
	default:
		return fmt.Errorf("auth.mode %q is invalid (expected %q or %q)", a.Mode, AuthModeJWT, AuthModeDisabled)
	}
}

// validate enforces the AuthorizationConfig invariants. Currently only the
// tenant cross-field rule (required without enabled is incoherent) — every
// other field is either a closed set (Enabled bool) or free-form
// (claim names, validated downstream by the runtime helpers).
func (a *AuthorizationConfig) validate() error {
	if a.Tenant.Required && !a.Tenant.Enabled {
		return fmt.Errorf("auth.authorization.tenant.required=true is incompatible with tenant.enabled=false (enable tenant first)")
	}
	return nil
}

func (a *AuthConfig) validateJWTMode() error {
	if a.JWT == nil {
		return fmt.Errorf("auth.jwt block is required when auth.mode=%q", AuthModeJWT)
	}
	if a.JWT.Issuer == "" {
		return fmt.Errorf("auth.jwt.issuer is required when auth.mode=%q", AuthModeJWT)
	}
	if a.JWT.Audience == "" {
		return fmt.Errorf("auth.jwt.audience is required when auth.mode=%q", AuthModeJWT)
	}
	hasJWKS := a.JWT.JWKSURL != ""
	hasPEM := a.JWT.PublicKeyPEM != ""
	if hasJWKS == hasPEM {
		return fmt.Errorf("auth.jwt requires exactly one of jwksUrl or publicKeyPem (got jwksUrl=%t, publicKeyPem=%t)", hasJWKS, hasPEM)
	}
	for _, alg := range a.JWT.Algorithms {
		if !isAllowedAlgorithm(alg) {
			return fmt.Errorf("auth.jwt.algorithms contains unsupported %q (allowed: %s)", alg, strings.Join(defaultJWTAlgorithms, ", "))
		}
	}
	if a.ExternalValidator != nil {
		return a.ExternalValidator.validate()
	}
	return nil
}

func (e *ExternalValidatorConfig) validate() error {
	if e.URL == "" {
		return fmt.Errorf("auth.externalValidator.url is required")
	}
	switch strings.ToUpper(e.Method) {
	case "GET", "POST":
	default:
		return fmt.Errorf("auth.externalValidator.method %q is invalid (expected GET or POST)", e.Method)
	}
	switch e.TokenPlacement {
	case TokenPlacementBearerHeader, TokenPlacementFormField, TokenPlacementJSONBody, TokenPlacementQueryParam:
	default:
		return fmt.Errorf("auth.externalValidator.tokenPlacement %q is invalid", e.TokenPlacement)
	}
	if e.TokenPlacement != TokenPlacementBearerHeader && e.TokenField == "" {
		return fmt.Errorf("auth.externalValidator.tokenField is required when tokenPlacement=%q", e.TokenPlacement)
	}
	switch e.FailMode {
	case FailModeClosed, FailModeOpen:
	default:
		return fmt.Errorf("auth.externalValidator.failMode %q is invalid (expected %q or %q)", e.FailMode, FailModeClosed, FailModeOpen)
	}
	if e.Success.JSONPath == "" {
		return fmt.Errorf("auth.externalValidator.success.jsonPath is required")
	}
	if e.Success.ExpectedValue == nil {
		return fmt.Errorf("auth.externalValidator.success.expectedValue is required")
	}
	if e.CacheTTLSeconds < 0 {
		return fmt.Errorf("auth.externalValidator.cacheTtlSeconds must be >= 0 (0 = cache disabled)")
	}
	return nil
}

func isAllowedAlgorithm(alg string) bool {
	for _, a := range defaultJWTAlgorithms {
		if a == alg {
			return true
		}
	}
	return false
}
