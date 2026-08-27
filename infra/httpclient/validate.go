package httpclient

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// defaultTimeout, defaultThreadIDHeader, defaultRequestIDHeader are the
// fallbacks applied to a Defaults block when the YAML leaves a field unset.
const (
	defaultTimeout         = 30 * time.Second
	defaultThreadIDHeader  = "X-Thread-Id"
	defaultRequestIDHeader = "X-Request-ID"
	defaultRequestCodec    = "json"
	defaultResponseCodec   = "json"
)

// supportedCodecs is the closed set of body codec names accepted today.
// The codec registry in binding/ stays private — adding a name here
// requires also adding the codec implementation, by framework patch.
var supportedCodecs = map[string]struct{}{
	"json":            {},
	"xml":             {},
	"form-urlencoded": {},
}

func isSupportedCodec(name string) bool {
	if name == "" {
		return true // empty defaults to json elsewhere
	}
	_, ok := supportedCodecs[name]
	return ok
}

// validHTTPMethods is the closed set of methods recognized by the validator.
// Compose-style helpers (CONNECT, TRACE) and gRPC verbs are not in scope for
// the outbound HTTP subsystem.
var validHTTPMethods = map[string]struct{}{
	"GET":    {},
	"POST":   {},
	"PUT":    {},
	"PATCH":  {},
	"DELETE": {},
	"HEAD":   {},
}

// applyDefaults fills missing Defaults entries with the framework's stock
// values. Called once at New, never per request.
func (c *Config) applyDefaults() {
	if c == nil {
		return
	}
	if c.Defaults.Timeout == 0 {
		c.Defaults.Timeout = Duration(defaultTimeout)
	}
	if c.Defaults.ThreadIDHeader == "" {
		c.Defaults.ThreadIDHeader = defaultThreadIDHeader
	}
	if c.Defaults.RequestIDHeader == "" {
		c.Defaults.RequestIDHeader = defaultRequestIDHeader
	}
	if c.Defaults.LogBodies == nil {
		t := true
		c.Defaults.LogBodies = &t
	}
	for name, sc := range c.Services {
		for ep, ec := range sc.Endpoints {
			if ec.RequestCodec == "" {
				ec.RequestCodec = defaultRequestCodec
			}
			if ec.ResponseCodec == "" {
				ec.ResponseCodec = defaultResponseCodec
			}
			sc.Endpoints[ep] = ec
		}
		c.Services[name] = sc
	}
}

// Validate runs schema + semantic checks on the Config and returns a single
// error listing every issue found. New aborts with this error before
// constructing any transport so a partially valid configuration never reaches
// the runtime.
//
// The checks are accumulated rather than short-circuiting so an operator who
// authored a YAML with multiple problems sees all of them on the first boot
// attempt.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	var errs validationErrors
	c.validateDefaults(&errs)
	c.validateServices(&errs)
	if len(errs) == 0 {
		return nil
	}
	return errs
}

func (c *Config) validateDefaults(errs *validationErrors) {
	if c.Defaults.Timeout < 0 {
		errs.add("httpClient.defaults.timeout: must be non-negative")
	}
	for _, msg := range validateRetryConfig("httpClient.defaults.retry", c.Defaults.Retry) {
		errs.add(msg)
	}
	for _, msg := range validateCacheDefaults("httpClient.defaults.cache", c.Defaults.Cache) {
		errs.add(msg)
	}
	for _, msg := range validateBreakerConfig("httpClient.defaults.circuitBreaker", c.Defaults.CircuitBreaker) {
		errs.add(msg)
	}
	for _, msg := range validateTLSConfig("httpClient.defaults.tls", c.Defaults.TLS) {
		errs.add(msg)
	}
	for _, msg := range validatePoolConfig("httpClient.defaults.pool", c.Defaults.Pool) {
		errs.add(msg)
	}
	for _, msg := range validateRedactionConfig("httpClient.defaults.redaction", c.Defaults.Redaction) {
		errs.add(msg)
	}
	for _, msg := range validateAuthProviders(c.AuthProviders) {
		errs.add(msg)
	}
	for _, msg := range validateServiceAuthReferences(c.Services, c.AuthProviders) {
		errs.add(msg)
	}
}

func (c *Config) validateServices(errs *validationErrors) {
	for _, name := range sortedKeys(c.Services) {
		sc := c.Services[name]
		validateService(errs, name, sc)
	}
}

func validateService(errs *validationErrors, name string, sc ServiceConfig) {
	prefix := fmt.Sprintf("httpClient.services.%s", name)
	// baseURL is optional: services that delegate routing to a runtime
	// BaseURLResolver leave it empty. The framework surfaces a clear
	// call-time error when both the resolver and the YAML are empty.
	if sc.BaseURL != "" {
		if u, err := url.Parse(sc.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			errs.addf("%s.baseURL: %q is not a valid absolute URL", prefix, sc.BaseURL)
		}
	}
	if sc.Timeout < 0 {
		errs.addf("%s.timeout: must be non-negative", prefix)
	}
	for _, msg := range validateTLSConfig(prefix+".tls", sc.TLS) {
		errs.add(msg)
	}
	for _, msg := range validatePoolConfig(prefix+".pool", sc.Pool) {
		errs.add(msg)
	}
	for _, msg := range validateRedactionConfig(prefix+".redaction", sc.Redaction) {
		errs.add(msg)
	}
	for _, msg := range validateSigningConfig(prefix+".signing", sc.Signing) {
		errs.add(msg)
	}
	if len(sc.Endpoints) == 0 {
		errs.addf("%s.endpoints: must declare at least one endpoint", prefix)
		return
	}
	for _, ep := range sortedKeys(sc.Endpoints) {
		ec := sc.Endpoints[ep]
		validateEndpoint(errs, prefix+".endpoints."+ep, ec)
	}
}

func validateEndpoint(errs *validationErrors, prefix string, ec EndpointConfig) {
	method := strings.ToUpper(ec.Method)
	if method == "" {
		errs.addf("%s.method: required", prefix)
	} else if _, ok := validHTTPMethods[method]; !ok {
		errs.addf("%s.method: %q is not a supported HTTP method (got one of GET/POST/PUT/PATCH/DELETE/HEAD)", prefix, ec.Method)
	}
	if ec.Path == "" {
		errs.addf("%s.path: required", prefix)
	} else if !strings.HasPrefix(ec.Path, "/") {
		errs.addf("%s.path: must start with '/' (got %q)", prefix, ec.Path)
	}
	if !isSupportedCodec(ec.RequestCodec) {
		errs.addf("%s.requestCodec: %q is not one of json|xml|form-urlencoded", prefix, ec.RequestCodec)
	}
	if !isSupportedCodec(ec.ResponseCodec) {
		errs.addf("%s.responseCodec: %q is not one of json|xml|form-urlencoded", prefix, ec.ResponseCodec)
	}
	for _, code := range ec.AcceptableStatus {
		if code < 100 || code > 599 {
			errs.addf("%s.acceptableStatus: %d is not a valid HTTP status code (must be in 100..599)", prefix, code)
		}
	}
	for _, msg := range validateRetryConfig(prefix+".retry", ec.Retry) {
		errs.add(msg)
	}
	// POST/PATCH retry requires an idempotency block on the same endpoint.
	// Without it, retrying a non-idempotent method risks double-write
	// semantics on the upstream.
	if ec.Retry != nil && ec.Retry.MaxAttempts > 1 && !methodAllowsRetry(ec.Method) && ec.Idempotency == nil {
		errs.addf("%s.retry.maxAttempts: %d > 1 on %s requires idempotency: block on the same endpoint", prefix, ec.Retry.MaxAttempts, strings.ToUpper(ec.Method))
	}
	for _, msg := range validateEndpointCache(prefix+".cache", ec.Method, ec.Cache) {
		errs.add(msg)
	}
	for _, msg := range validateIdempotencyConfig(prefix+".idempotency", ec.Idempotency) {
		errs.add(msg)
	}
	if ec.ResponseStream && ec.ResponseSSE {
		errs.addf("%s: responseStream and responseSSE are mutually exclusive", prefix)
	}
	if ec.ResponseStream && ec.Cache != nil {
		errs.addf("%s: cache: cannot coexist with responseStream (caching a stream is undefined)", prefix)
	}
	if ec.ResponseSSE && ec.Cache != nil {
		errs.addf("%s: cache: cannot coexist with responseSSE (caching a stream is undefined)", prefix)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validationErrors accumulates messages and renders them as one error so the
// caller sees every issue in one boot attempt instead of fix-one-redeploy.
type validationErrors []string

func (e *validationErrors) add(msg string)                  { *e = append(*e, msg) }
func (e *validationErrors) addf(f string, a ...interface{}) { *e = append(*e, fmt.Sprintf(f, a...)) }

func (e validationErrors) Error() string {
	if len(e) == 1 {
		return "httpclient: invalid config: " + e[0]
	}
	return "httpclient: invalid config:\n  - " + strings.Join(e, "\n  - ")
}
