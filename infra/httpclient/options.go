package httpclient

import (
	"crypto/tls"
	"net/url"
	"time"
)

// InvokeOption customizes a single call. The set is intentionally narrow.
// Per-call overrides of YAML-declared fields are consolidated under
// WithConfig(CallConfig); the standalone options here cover concerns the
// CallConfig struct cannot express cleanly:
//
//   - WithExtraHeader / WithExtraQuery — runtime additive injection, not a
//     YAML field override.
//   - WithClientCert — binary tls.Certificate, not a YAML-declarable string.
//
// Everything else (BaseURL, Method, Path, Timeout, Auth, Codecs, cache
// flags, idempotency key, acceptable statuses, retry policy, inline auth)
// goes through WithConfig.
type InvokeOption func(*invokeConfig)

// invokeConfig is the accumulated per-call state. Each option mutates it
// cumulatively. Defaults match the "no-op" semantics: empty extras, no
// runtime acceptable status, no timeout override.
type invokeConfig struct {
	// Additive runtime injection
	extraHeaders map[string]string
	extraQuery   url.Values
	clientCert   *tls.Certificate

	// CallConfig-sourced overrides
	baseURLOverride       string
	timeout               time.Duration
	authOverride          string
	methodOverride        string
	pathOverride          string
	requestCodecOverride  string
	responseCodecOverride string
	acceptableStatus      map[int]struct{}
	noCache               bool
	cacheKey              string
	idempotencyKey        string
	inlineAuth            *InlineAuth
	retryOverride         *RetryOverride
}

// effectiveMethod returns the per-call method override when set, falling
// back to the endpoint's YAML method.
func (c *invokeConfig) effectiveMethod(yaml string) string {
	if c == nil || c.methodOverride == "" {
		return yaml
	}
	return c.methodOverride
}

// effectivePath returns the per-call path override when set, falling back
// to the endpoint's YAML path.
func (c *invokeConfig) effectivePath(yaml string) string {
	if c == nil || c.pathOverride == "" {
		return yaml
	}
	return c.pathOverride
}

// effectiveRequestCodec returns the per-call requestCodec override when
// set, falling back to the endpoint's YAML codec.
func (c *invokeConfig) effectiveRequestCodec(yaml string) string {
	if c == nil || c.requestCodecOverride == "" {
		return yaml
	}
	return c.requestCodecOverride
}

// effectiveResponseCodec returns the per-call responseCodec override when
// set, falling back to the endpoint's YAML codec.
func (c *invokeConfig) effectiveResponseCodec(yaml string) string {
	if c == nil || c.responseCodecOverride == "" {
		return yaml
	}
	return c.responseCodecOverride
}

// applyInvokeOptions folds the supplied options into a fresh invokeConfig.
func applyInvokeOptions(opts []InvokeOption) *invokeConfig {
	c := &invokeConfig{
		extraHeaders:     map[string]string{},
		extraQuery:       url.Values{},
		acceptableStatus: map[int]struct{}{},
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

// WithConfig applies a CallConfig as a per-call override of YAML-declared
// fields. Fields left at their zero value preserve the YAML / framework
// defaults; see CallConfig's doc for the per-field semantics.
//
// This is the canonical per-call override surface. Multiple WithConfig
// calls in the same opts slice are cumulative — fields set later overwrite
// earlier ones; nil pointer fields preserve the prior value (no clear).
func WithConfig(cfg CallConfig) InvokeOption {
	return func(c *invokeConfig) {
		if cfg.BaseURL != "" {
			c.baseURLOverride = cfg.BaseURL
		}
		if cfg.Timeout > 0 {
			c.timeout = cfg.Timeout
		}
		if cfg.AuthProvider != "" {
			c.authOverride = cfg.AuthProvider
		}
		if cfg.Method != "" {
			c.methodOverride = cfg.Method
		}
		if cfg.Path != "" {
			c.pathOverride = cfg.Path
		}
		if cfg.RequestCodec != "" {
			c.requestCodecOverride = cfg.RequestCodec
		}
		if cfg.ResponseCodec != "" {
			c.responseCodecOverride = cfg.ResponseCodec
		}
		for _, code := range cfg.AcceptableStatus {
			c.acceptableStatus[code] = struct{}{}
		}
		if cfg.NoCache {
			c.noCache = true
		}
		if cfg.CacheKey != "" {
			c.cacheKey = cfg.CacheKey
		}
		if cfg.IdempotencyKey != "" {
			c.idempotencyKey = cfg.IdempotencyKey
		}
		if cfg.Retry != nil {
			c.retryOverride = cfg.Retry
		}
		if cfg.InlineAuth != nil {
			c.inlineAuth = cfg.InlineAuth
		}
	}
}

// WithExtraHeader sets an extra request header. Repeated keys overwrite
// (the last value wins). Applied on top of the YAML defaults/service/endpoint
// header cascade. Use for runtime-only headers the YAML cannot anticipate
// (e.g., a per-call tenant id generated in the handler).
func WithExtraHeader(key, value string) InvokeOption {
	return func(c *invokeConfig) {
		c.extraHeaders[key] = value
	}
}

// WithExtraQuery appends a query parameter to the URL. Repeated keys append
// values — matches url.Values semantics so callers can build CSV/multi-value
// query strings ad-hoc without touching the binding tags.
func WithExtraQuery(key, value string) InvokeOption {
	return func(c *invokeConfig) {
		c.extraQuery.Add(key, value)
	}
}

// WithClientCert overrides the per-service TLS client certificate for
// this single call. Typical use: a vault-rotated cert delivered via the
// secrets manager. The override is ephemeral — it does not pollute the
// service's transport pool, and subsequent calls without the option keep
// using the YAML-configured cert (if any).
//
// This option does not live in CallConfig because tls.Certificate is a
// binary structure rather than a declarable string. Use the YAML for CA
// bundle, cipher suites, and minVersion.
func WithClientCert(cert tls.Certificate) InvokeOption {
	return func(c *invokeConfig) {
		certCopy := cert
		c.clientCert = &certCopy
	}
}
