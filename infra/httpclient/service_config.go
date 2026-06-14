package httpclient

// ServiceConfig describes one external system the service talks to. Carries
// the connection point (BaseURL), per-service knobs (Timeout, Headers), and
// the endpoint catalog that the call surface dispatches against.
type ServiceConfig struct {
	BaseURL   string                    `yaml:"baseURL"`
	Timeout   Duration                  `yaml:"timeout"`
	Headers   map[string]string         `yaml:"headers"`
	Endpoints map[string]EndpointConfig `yaml:"endpoints"`

	// Auth links the service to a provider declared under
	// httpClient.authProviders. The validator rejects services that
	// reference an undeclared provider.
	Auth *ServiceAuthConfig `yaml:"auth,omitempty"`
	// TLS overrides the defaults-level TLS config field by field. Use to
	// pin mTLS certs or downgrade minVersion for legacy backends.
	TLS *TLSConfig `yaml:"tls,omitempty"`

	// Pool overrides the defaults-level transport pool knobs (typically
	// to match an upstream-imposed per-host limit).
	Pool *PoolConfig `yaml:"pool,omitempty"`

	// Signing declares an HMAC request-signing policy. Optional; when
	// present, the framework injects timestamp + content-sha256 +
	// signature headers on every outbound request, with re-signing per
	// retry attempt. See SigningConfig for the field catalog and the
	// signing middleware (chain position 8) for the exact behavior.
	Signing *SigningConfig `yaml:"signing,omitempty"`

	// Redaction extends defaults — never replaces. Per-service entries
	// add headers, body JSONPaths, or query keys on top of the cascade.
	Redaction *RedactionConfig `yaml:"redaction,omitempty"`
}

// EndpointConfig describes one operation against a service. Method and Path
// are mandatory; the codecs default to JSON; AcceptableStatus declares
// non-error statuses the consumer expects (e.g. a 404 on a presence-check
// endpoint).
//
// The reserved fields (Cache, Retry, Idempotency, ResponseStream, ResponseSSE)
// match the design schema but are rejected by Validate in the current phase.
type EndpointConfig struct {
	Method           string            `yaml:"method"`
	Path             string            `yaml:"path"`
	RequestCodec     string            `yaml:"requestCodec"`
	ResponseCodec    string            `yaml:"responseCodec"`
	AcceptableStatus []int             `yaml:"acceptableStatus"`
	Headers          map[string]string `yaml:"headers"`

	// Cache enables caching for this endpoint. Presence (with or without
	// children) opts in; absence leaves the endpoint bypass-only. TTL and
	// VaryOn shape the entry behavior.
	Cache *EndpointCacheConfig `yaml:"cache,omitempty"`

	// CacheAcceptable opts into caching responses whose status appears in
	// AcceptableStatus (e.g. 404 on a presence-check). Default false —
	// only 2xx is cached.
	CacheAcceptable bool `yaml:"cacheAcceptable"`

	// Retry holds the endpoint-level retry policy. Overrides defaults
	// field by field; framework defaults fill any remaining gap.
	Retry *RetryConfig `yaml:"retry,omitempty"`

	// Idempotency enables per-call idempotency-key injection. Presence
	// unblocks POST/PATCH retry when retry: is configured on the same
	// endpoint — the same key is reused across retry attempts so the
	// upstream can dedupe.
	Idempotency *IdempotencyConfig `yaml:"idempotency,omitempty"`

	// ResponseStream marks the endpoint as a download-streaming endpoint.
	// The Resp type passed to Call must be httpclient.StreamResponse; the
	// framework hands the open response body to the caller without
	// decoding or buffering. Cache and ResponseSSE are mutually exclusive
	// with this flag.
	ResponseStream bool `yaml:"responseStream,omitempty"`

	// ResponseSSE marks the endpoint as a Server-Sent Events stream. The
	// Resp type must be httpclient.SSEResponse; the framework parses the
	// text/event-stream body and emits events on a channel. Mutually
	// exclusive with Cache and ResponseStream.
	ResponseSSE bool `yaml:"responseSSE,omitempty"`
}
