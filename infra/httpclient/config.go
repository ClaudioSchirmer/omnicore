// Package httpclient is the framework's outbound HTTP subsystem. Services
// describe the external systems they talk to in microservice.<profile>.yaml
// under the httpClient: block, and the framework constructs the shared
// *HttpClient registry on bootstrap.Deps.HttpClient.
//
// The current call surface scope: per-service HTTP transports built from a
// declarative YAML schema (defaults + services + endpoints). The runtime
// Call[Req, Resp] generic, tag binding, codec, middleware chain, auth
// providers, retry, cache, circuit breaker, redaction, signing and streaming
// are introduced as separate canonical surfaces in the dedicated phases.
package httpclient

// Config carries the httpClient: block of microservice.<profile>.yaml.
//
// Presence on bootstrap.Config.HttpClient is the meaningful signal for the
// bootstrap layer: when the YAML carries httpClient: (with or without
// children), bootstrap.Deps.HttpClient is materialized via New; when the
// block is absent, Deps.HttpClient stays nil and features that need the
// client must guard at composition time.
type Config struct {
	Defaults Defaults                 `yaml:"defaults"`
	Services map[string]ServiceConfig `yaml:"services"`

	// Reserved fields from the design schema. Present on the type so the
	// parser keeps the line/column information for actionable validator
	// errors; rejected by Validate with a phase reference until the
	// implementing phase wires them.
	// AuthProviders holds the named provider configurations referenced by
	// services via service.auth.provider. Validate enforces type-specific
	// shape and rejects references to undeclared providers.
	AuthProviders map[string]AuthProviderConfig `yaml:"authProviders,omitempty"`
}

// Defaults are the cross-service knobs applied when a ServiceConfig or
// EndpointConfig omits a value. Population happens once at New, never per
// request.
type Defaults struct {
	// Timeout is the default per-request timeout. Falls back to 30s when
	// neither defaults nor the service override the value.
	Timeout Duration `yaml:"timeout"`

	// ThreadIDHeader is the outbound header that carries AppContext.ID()
	// on every request. Defaults to "X-Thread-Id".
	ThreadIDHeader string `yaml:"threadIdHeader"`

	// RequestIDHeader is the header used to propagate AppContext.RequestID()
	// when set. Defaults to "X-Request-ID".
	RequestIDHeader string `yaml:"requestIdHeader"`

	// LogBodies controls whether the slog observation line includes the full
	// request and response bodies (subject to redaction). Defaults to true.
	// *bool so explicit false is distinguishable from omitted (default true).
	LogBodies *bool `yaml:"logBodies"`

	// Headers are merged into every outbound request as the first layer of
	// the cascade (defaults → service → endpoint, last write wins).
	Headers map[string]string `yaml:"headers"`

	// Retry holds the defaults-level retry policy. Endpoint-level retry
	// blocks override these field by field; framework defaults fill any
	// remaining gap.
	Retry *RetryConfig `yaml:"retry,omitempty"`

	// Cache holds the defaults-level cache controls. Endpoint blocks enable
	// caching per-endpoint; this block decides whether the runtime layer
	// participates at all (Enabled) and sizes the in-memory store.
	Cache *CacheDefaults `yaml:"cache,omitempty"`

	// CircuitBreaker holds the defaults-level breaker policy. Per the
	// design, configuration is shared across all (service, endpoint)
	// pairs; runtime state is tracked per pair.
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuitBreaker,omitempty"`
	// Pool holds the defaults-level transport pool tuning; services may
	// override per-host limits when an upstream imposes them.
	Pool *PoolConfig `yaml:"pool,omitempty"`

	// TLS holds the defaults-level TLS overrides; services override field
	// by field via their own tls: block.
	TLS *TLSConfig `yaml:"tls,omitempty"`

	// Redaction extends the framework's hard-coded block list with
	// per-deployment header names, body JSONPaths, and query keys.
	// Per-service blocks extend (do not replace) defaults.
	Redaction *RedactionConfig `yaml:"redaction,omitempty"`
}
