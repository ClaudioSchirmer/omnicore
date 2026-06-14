package httpclient

import "time"

// CallConfig consolidates every per-call override of a YAML-declared field
// into a single value. Pass it via WithConfig(c CallConfig).
//
// Zero values mean "no override on that field" — the YAML / framework
// defaults stand. Specific zero-value semantics:
//
//   - Timeout: 0 = inherit (positive = override)
//   - NoCache: false = respect YAML; true = bypass cache for this call
//   - AcceptableStatus: nil = no addition; non-nil = union with YAML
//   - InlineAuth: nil = use named provider (CallConfig.AuthProvider or YAML)
//   - Retry: nil = use endpoint's YAML policy; non-nil = full replacement
//
// What this struct intentionally does NOT cover:
//
//   - TLS (minVersion, cipher suites, CA bundle, insecureSkipVerify) and
//     connection pool tuning — these are bound to the http.Transport built
//     once at New, and overriding them per-call would require rebuilding the
//     transport (invalidates pool, defeats keep-alive). Per-call TLS client
//     certificate is the single exception via WithClientCert(cert), which
//     builds an ephemeral cloned transport for that one call only.
//   - Headers / query parameters — these are additive runtime inputs rather
//     than YAML field overrides; use WithExtraHeader / WithExtraQuery.
//   - Auth provider definitions (tokenEndpoint, clientID, scopes, etc.) —
//     providers are wired at boot. For per-call credentials supplied at
//     runtime, set CallConfig.InlineAuth instead of trying to override the
//     definition.
type CallConfig struct {
	// Service-level overrides

	// BaseURL overrides services.<name>.baseURL for this call. Takes
	// precedence over the registered BaseURLResolver and the YAML.
	BaseURL string

	// Timeout overrides the per-call timeout cascade. 0 inherits.
	Timeout time.Duration

	// AuthProvider selects a YAML-declared auth provider by name. Empty
	// inherits the service's default. Mutually exclusive with InlineAuth;
	// InlineAuth wins when both are set.
	AuthProvider string

	// Endpoint-level overrides

	// Method overrides endpoint.method. Empty inherits.
	Method string

	// Path overrides endpoint.path. Empty inherits. Must start with '/'.
	Path string

	// RequestCodec overrides endpoint.requestCodec. Empty inherits.
	RequestCodec string

	// ResponseCodec overrides endpoint.responseCodec. Empty inherits.
	ResponseCodec string

	// AcceptableStatus extends endpoint.acceptableStatus by union. nil =
	// no addition.
	AcceptableStatus []int

	// NoCache bypasses the GET cache for this call when true.
	NoCache bool

	// CacheKey overrides the framework-computed cache key. Empty preserves
	// the computed key.
	CacheKey string

	// IdempotencyKey supplies the idempotency key explicitly. Required
	// when the endpoint declares idempotency.source: explicit; ignored
	// otherwise.
	IdempotencyKey string

	// Retry replaces the endpoint's YAML retry policy when non-nil. nil
	// keeps the YAML-resolved policy. Total replacement (no field-level
	// merging) — set every field you care about; framework defaults fill
	// the rest.
	Retry *RetryOverride

	// InlineAuth supplies credentials at call time instead of via a YAML-
	// declared provider. Mutually exclusive with AuthProvider; InlineAuth
	// wins on conflict. obs.AuthProvider logs "inline:<scheme>" when used.
	//
	// Canonical use case: webhook callbacks where each customer brings
	// its own credentials from the DB — registering a YAML provider per
	// customer would not scale.
	InlineAuth *InlineAuth
}

// InlineAuth carries credentials supplied at call time. Exactly one of
// Bearer / APIKey / Basic must be set; setting more than one returns
// ErrTokenAcquire before dialing. Setting none with InlineAuth itself
// non-nil also errors — a non-nil InlineAuth is a deliberate opt-in to
// the inline path, so the framework rejects ambiguous shapes early.
type InlineAuth struct {
	// Bearer attaches Authorization: Bearer <Bearer>.
	Bearer string

	// APIKey attaches a custom header (default X-API-Key when Header is empty).
	APIKey *APIKeyAuth

	// Basic attaches Authorization: Basic base64(Username:Password).
	Basic *BasicAuth
}

// APIKeyAuth supplies a custom header credential. Header defaults to
// "X-API-Key" when empty. Value is required.
type APIKeyAuth struct {
	Header string
	Value  string
}

// BasicAuth supplies HTTP Basic credentials. Both fields are required.
type BasicAuth struct {
	Username string
	Password string
}

// RetryOverride replaces the endpoint's YAML retry policy for one call.
// Total replacement: nil RetryOverride keeps the YAML policy; non-nil
// builds a brand new policy from these fields with framework defaults for
// any field the caller leaves zero.
type RetryOverride struct {
	// MaxAttempts is the total attempt budget (1 = no retry; 0 inherits
	// framework default 1).
	MaxAttempts int

	// Backoff is the curve between attempts: constant | linear |
	// exponential | exponential-jitter. Empty defaults to
	// exponential-jitter.
	Backoff string

	// InitialDelay is the base wait between attempts. 0 defaults to 100ms.
	InitialDelay time.Duration

	// MaxDelay is the cap. 0 defaults to 5s.
	MaxDelay time.Duration

	// RetryOn lists what triggers a retry: status codes as strings ("503",
	// "504") plus the sentinels "network", "timeout", "dns". Empty defaults
	// to ["502", "503", "504", "network", "timeout"].
	RetryOn []string

	// RespectRetryAfter honors RFC 7231 Retry-After when true. Pointer
	// to disambiguate "use framework default (true)" from "explicitly
	// disabled (false)": nil = framework default; non-nil = explicit.
	RespectRetryAfter *bool
}
