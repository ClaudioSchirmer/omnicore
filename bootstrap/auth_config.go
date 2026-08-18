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
// asymmetric algorithms — symmetric (HMAC) is intentionally excluded
// permanently. The property this buys: a VALIDATOR never holds signing
// material, only the ISSUER does (whether that issuer is an external IdP or
// this service's own auth.issuer block — see IssuerConfig), and an issuer's
// private key has a public half that is safe to publish. HMAC breaks that:
// every validator would need the same secret used to mint tokens, so any
// compromised or merely careless validator becomes a forgery vector for the
// whole mesh. That failure mode is what this allowlist exists to prevent.
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

	// Issuer is the outbound mirror of JWT: this service's OWN capability to
	// mint tokens (asymmetric signing, key rotation, optional refresh
	// tokens), built into Deps.Issuer. nil or Enabled=false means the
	// service never signs — every other auth.* knob keeps validating
	// whatever tokens arrive, unaffected. See IssuerConfig.
	Issuer *IssuerConfig `yaml:"issuer,omitempty"`
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
	// Enabled is the master switch for the tenancy feature: it gates whether
	// the AuthMiddleware honors Required (the empty-TenantID presence gate).
	// It does NOT change Identity.TenantID(), which always reads the configured
	// Claim from the token regardless of this flag.
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
// behavior.
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

// issuerKeyStateNext / issuerKeyStateCurrent / issuerKeyStatePrevious are the
// yaml values for IssuerKeyConfig.State — the wire form of authcore.KeyState.
const (
	issuerKeyStateNext     = "next"
	issuerKeyStateCurrent  = "current"
	issuerKeyStatePrevious = "previous"
)

// defaultIssuerJWKSPath is IssuerJWKSConfig.Path's default when the jwks:
// block is present but Path is empty.
const defaultIssuerJWKSPath = "/.well-known/jwks.json"

// IssuerConfig is the `auth.issuer:` block — this service's own capability
// to mint JWTs, the outbound mirror of JWTConfig. Closed key set (typos
// surface at boot via UnmarshalYAML), matching the AuthorizationConfig
// precedent.
type IssuerConfig struct {
	// Enabled is the master switch. false (default) — everything below is
	// ignored and Deps.Issuer stays nil.
	Enabled bool `yaml:"enabled"`

	// SelfURL is the `iss` claim every token this service mints carries.
	// Deliberately not named "issuer" (that word already labels this
	// block) and never inherited from JWTConfig.Issuer — a service may
	// issue without ever validating locally. Required when Enabled; when
	// auth.jwt is also configured on the same service, SelfURL must equal
	// JWTConfig.Issuer (a service cannot disagree with itself about who it
	// is).
	SelfURL string `yaml:"selfUrl"`

	// Audience is the default `aud` claim for every minted token. Required
	// (non-empty) when Enabled.
	Audience []string `yaml:"audience"`

	// TokenTTLSeconds is the default access-token lifetime. Required (> 0)
	// — there is no framework default, because a silent zero would mint
	// tokens that expire the instant they are issued.
	TokenTTLSeconds int `yaml:"tokenTtlSeconds"`

	// MaxTokenTTLSeconds is the hard ceiling on any access token's
	// lifetime, including a per-request TTL override. Required
	// (>= TokenTTLSeconds).
	MaxTokenTTLSeconds int `yaml:"maxTokenTtlSeconds"`

	// RefreshTokenTTLSeconds, when > 0, is the lifetime of a freshly minted
	// refresh token and enables IssueWithRefresh / RedeemRefreshToken. 0
	// (default) disables refresh tokens entirely. Requires
	// Wiring.RefreshTokenStore to be non-nil — checked at boot against the
	// live Wiring, not here (this struct has no visibility into Wiring).
	RefreshTokenTTLSeconds int `yaml:"refreshTokenTtlSeconds,omitempty"`

	// Keys is the rotation set: exactly one State=current, any number of
	// next/previous. Required (non-empty) when Enabled.
	//
	// Signing material (PrivateKeyPEM) is expected to arrive via
	// ${ENV_VAR} substitution (see load.go's interpolate), never as a
	// literal PEM block committed to the yaml file — the same operational
	// convention every other secret in a microservice.<profile>.yaml
	// follows (DSNs, API keys, …). This is NOT machine-enforced: by the
	// time this struct is populated, interpolate() has already replaced
	// ${...} references in the raw file text, so a literal PEM and a
	// substituted one are indistinguishable at validate() time.
	Keys []IssuerKeyConfig `yaml:"keys"`

	// JWKS is opt-in: nil (default) means the public key never touches the
	// network — every consumer validates via auth.jwt.externalValidator
	// against this service instead. A non-nil (even empty) block mounts
	// GET <path> as a public route, following the same
	// presence-is-the-switch shape as GraphQLConfig/OpenAPIConfig.
	JWKS *IssuerJWKSConfig `yaml:"jwks,omitempty"`
}

// IssuerKeyConfig is one signing key in IssuerConfig.Keys — the yaml mirror
// of authcore.SigningKey.
type IssuerKeyConfig struct {
	KID           string `yaml:"kid"`
	Algorithm     string `yaml:"algorithm"`
	State         string `yaml:"state"` // "next" | "current" | "previous"
	PrivateKeyPEM string `yaml:"privateKeyPem"`
}

// IssuerJWKSConfig is the `auth.issuer.jwks:` sub-block. Its mere presence
// on IssuerConfig.JWKS is the switch that mounts the route — see JWKS route
// in the token-issuance manual section.
type IssuerJWKSConfig struct {
	// Path is the Fiber route (GET) the JWKS document is served on.
	// Default "/.well-known/jwks.json". Must not collide with a reserved
	// framework route.
	Path string `yaml:"path,omitempty"`
}

// UnmarshalYAML for IssuerConfig enforces the closed key set.
func (c *IssuerConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias IssuerConfig
	if err := node.Decode((*alias)(c)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "auth.issuer",
		"enabled", "selfUrl", "audience", "tokenTtlSeconds", "maxTokenTtlSeconds",
		"refreshTokenTtlSeconds", "keys", "jwks")
}

// UnmarshalYAML for IssuerKeyConfig enforces the closed key set.
func (k *IssuerKeyConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias IssuerKeyConfig
	if err := node.Decode((*alias)(k)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "auth.issuer.keys[]",
		"kid", "algorithm", "state", "privateKeyPem")
}

// UnmarshalYAML for IssuerJWKSConfig enforces the closed key set.
func (j *IssuerJWKSConfig) UnmarshalYAML(node *yaml.Node) error {
	type alias IssuerJWKSConfig
	if err := node.Decode((*alias)(j)); err != nil {
		return err
	}
	return rejectUnknownYAMLFields(node, "auth.issuer.jwks", "path")
}

// applyDefaults fills IssuerJWKSConfig.Path when the jwks: block is present
// but left the path empty. No-op when Enabled is false — an unmounted
// Issuer has nothing to default.
func (c *IssuerConfig) applyDefaults() {
	if !c.Enabled {
		return
	}
	if c.JWKS != nil && c.JWKS.Path == "" {
		c.JWKS.Path = defaultIssuerJWKSPath
	}
}

// validate enforces the IssuerConfig invariants. jwtIssuer is
// AuthConfig.JWT.Issuer when auth.jwt is configured on the same service
// (empty otherwise) — passed in because a service that both issues and
// validates must agree with itself about SelfURL.
func (c *IssuerConfig) validate(jwtIssuer string) error {
	if !c.Enabled {
		return nil
	}
	if c.SelfURL == "" {
		return fmt.Errorf("auth.issuer.selfUrl is required when auth.issuer.enabled=true")
	}
	if jwtIssuer != "" && c.SelfURL != jwtIssuer {
		return fmt.Errorf("auth.issuer.selfUrl %q must equal auth.jwt.issuer %q when both are configured", c.SelfURL, jwtIssuer)
	}
	if len(c.Audience) == 0 {
		return fmt.Errorf("auth.issuer.audience is required when auth.issuer.enabled=true")
	}
	if c.TokenTTLSeconds <= 0 {
		return fmt.Errorf("auth.issuer.tokenTtlSeconds must be > 0")
	}
	if c.MaxTokenTTLSeconds <= 0 {
		return fmt.Errorf("auth.issuer.maxTokenTtlSeconds must be > 0")
	}
	if c.MaxTokenTTLSeconds < c.TokenTTLSeconds {
		return fmt.Errorf("auth.issuer.maxTokenTtlSeconds (%d) must be >= tokenTtlSeconds (%d)", c.MaxTokenTTLSeconds, c.TokenTTLSeconds)
	}
	if c.RefreshTokenTTLSeconds < 0 {
		return fmt.Errorf("auth.issuer.refreshTokenTtlSeconds must be >= 0 (0 = refresh tokens disabled)")
	}
	if len(c.Keys) == 0 {
		return fmt.Errorf("auth.issuer.keys must declare at least one key when auth.issuer.enabled=true")
	}
	var currentCount int
	for idx, k := range c.Keys {
		if k.KID == "" {
			return fmt.Errorf("auth.issuer.keys[%d].kid is required", idx)
		}
		if !isAllowedAlgorithm(k.Algorithm) {
			return fmt.Errorf("auth.issuer.keys[%d].algorithm %q is invalid (allowed: %s)", idx, k.Algorithm, strings.Join(defaultJWTAlgorithms, ", "))
		}
		switch k.State {
		case issuerKeyStateNext, issuerKeyStatePrevious:
		case issuerKeyStateCurrent:
			currentCount++
		default:
			return fmt.Errorf("auth.issuer.keys[%d].state %q is invalid (expected %q, %q or %q)", idx, k.State, issuerKeyStateNext, issuerKeyStateCurrent, issuerKeyStatePrevious)
		}
		if k.PrivateKeyPEM == "" {
			return fmt.Errorf("auth.issuer.keys[%d].privateKeyPem is required", idx)
		}
	}
	if currentCount != 1 {
		return fmt.Errorf("auth.issuer.keys must declare exactly one key with state=%q, got %d", issuerKeyStateCurrent, currentCount)
	}
	if c.JWKS != nil {
		if err := c.JWKS.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (j *IssuerJWKSConfig) validate() error {
	if !strings.HasPrefix(j.Path, "/") {
		return fmt.Errorf("auth.issuer.jwks.path %q must start with %q", j.Path, "/")
	}
	if collidesFrameworkPath(j.Path) {
		return fmt.Errorf("auth.issuer.jwks.path %q collides with a framework route", j.Path)
	}
	return nil
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
	// Issuer (outbound: this service minting tokens) is orthogonal to Mode
	// (inbound: how this service validates tokens) — a service can issue
	// without validating locally, so its defaults apply regardless of Mode.
	if a.Issuer != nil {
		a.Issuer.applyDefaults()
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
	// Issuer is orthogonal to Mode — validated regardless of how (or
	// whether) this service validates inbound tokens. jwtIssuer is only
	// non-empty under AuthModeJWT, which is exactly when a same-service
	// SelfURL/Issuer agreement is meaningful to check.
	var jwtIssuer string
	if a.Mode == AuthModeJWT && a.JWT != nil {
		jwtIssuer = a.JWT.Issuer
	}
	if a.Issuer != nil {
		if err := a.Issuer.validate(jwtIssuer); err != nil {
			return err
		}
	}

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
