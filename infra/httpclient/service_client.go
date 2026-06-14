package httpclient

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Pool defaults applied to every per-service http.Transport. The design
// schema documents per-service overrides under pool: but that block is
// reserved until the dedicated phase; today every transport carries the same
// constants.
const (
	defaultMaxIdleConnsPerHost = 100
	defaultMaxConnsPerHost     = 200
	defaultIdleConnTimeout     = 90 * time.Second
)

// serviceClient is the per-service runtime built once at New and consumed by
// the call surface (Phase 1.C onward). One serviceClient per service entry in
// the YAML; each carries an isolated http.Transport so a misbehaving upstream
// cannot starve the pool of well-behaved ones.
type serviceClient struct {
	name         string
	baseURL      string
	httpClient   *http.Client
	headers      map[string]string
	endpoints    map[string]endpointSpec
	threadID     string
	requestID    string
	logBodies    bool
	authProvider string // YAML provider name; empty when no auth
	tlsConfig    *tls.Config
	transport    *http.Transport
	redaction    redactionPolicy
	signing      signingPolicy // disabled when no signing: block
}

// endpointSpec is the per-endpoint frozen view that the call surface
// dispatches against. Method is upper-cased, AcceptableStatus is a set, and
// the header layers are merged so the request path performs no map lookups
// against the declarative YAML.
type endpointSpec struct {
	method           string
	path             string
	requestCodec     string
	responseCodec    string
	acceptableStatus map[int]struct{}
	headers          map[string]string
	retry            retryPolicy
	cache            cachePolicy
	cacheAcceptable  bool
	idempotency      idempotencyPolicy
	responseStream   bool
	responseSSE      bool
}

// buildServiceClient materializes a serviceClient from validated config. The
// caller is expected to have run Validate beforehand — buildServiceClient
// trusts shapes and does not re-validate. Returns an error when TLS asset
// loading (cert pair, CA bundle) fails at boot.
func buildServiceClient(name string, sc ServiceConfig, defaults Defaults) (*serviceClient, error) {
	timeout := sc.Timeout.ToTime()
	if timeout == 0 {
		timeout = defaults.Timeout.ToTime()
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}

	tlsCfg, err := resolveTLSConfig(defaults.TLS, sc.TLS)
	if err != nil {
		return nil, fmt.Errorf("httpclient: service %q: %w", name, err)
	}
	maxIdle, maxConns, idleTimeout, disableKA := resolvePoolConfig(defaults.Pool, sc.Pool)
	transport := &http.Transport{
		MaxIdleConnsPerHost: maxIdle,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     idleTimeout,
		DisableKeepAlives:   disableKA,
		TLSClientConfig:     tlsCfg,
	}

	serviceHeaders := mergeHeaders(defaults.Headers, sc.Headers)
	endpoints := make(map[string]endpointSpec, len(sc.Endpoints))
	for epName, ec := range sc.Endpoints {
		method := strings.ToUpper(ec.Method)
		endpoints[epName] = endpointSpec{
			method:           method,
			path:             ec.Path,
			requestCodec:     ec.RequestCodec,
			responseCodec:    ec.ResponseCodec,
			acceptableStatus: statusSet(ec.AcceptableStatus),
			headers:          mergeHeaders(serviceHeaders, ec.Headers),
			retry:            resolveRetryPolicy(method, defaults.Retry, ec.Retry, ec.Idempotency != nil),
			cache:            resolveCachePolicy(defaults.Cache, ec.Cache),
			cacheAcceptable:  ec.CacheAcceptable,
			idempotency:      resolveIdempotencyPolicy(ec.Idempotency),
			responseStream:   ec.ResponseStream,
			responseSSE:      ec.ResponseSSE,
		}
	}

	logBodies := true
	if defaults.LogBodies != nil {
		logBodies = *defaults.LogBodies
	}

	authProvider := ""
	if sc.Auth != nil {
		authProvider = sc.Auth.Provider
	}

	signingPol := resolveSigningPolicy(sc.Signing)
	redaction := resolveRedactionPolicy(defaults.Redaction, sc.Redaction)
	// Auto-redact the signature + keyId headers so the slog observation
	// never leaks a credential the operator did not explicitly mark.
	// Operators who want to see the value (debugging) extend the YAML
	// redaction block to override... actually they cannot — the cascade
	// is union-only. Documented in the signing section: signatures are
	// always redacted in logs, never on the wire.
	addRedactedHeader(&redaction, signingPol.signatureHeader)
	addRedactedHeader(&redaction, signingPol.keyIdHeader)

	return &serviceClient{
		name: name,
		// baseURL is preserved as-is; the call surface concatenates the
		// endpoint path after path-param substitution. Trailing slash
		// stripped here so endpoints can always start with '/' without
		// producing "//path".
		baseURL:      strings.TrimRight(sc.BaseURL, "/"),
		httpClient:   &http.Client{Transport: transport, Timeout: timeout},
		headers:      serviceHeaders,
		endpoints:    endpoints,
		threadID:     defaults.ThreadIDHeader,
		requestID:    defaults.RequestIDHeader,
		logBodies:    logBodies,
		authProvider: authProvider,
		tlsConfig:    tlsCfg,
		transport:    transport,
		redaction:    redaction,
		signing:      signingPol,
	}, nil
}

// addRedactedHeader inserts the given header name into the redaction
// policy's set (canonical MIME form) so the logging middleware redacts
// the value. No-op when name is empty.
func addRedactedHeader(policy *redactionPolicy, name string) {
	if name == "" || policy == nil {
		return
	}
	if policy.headerSet == nil {
		policy.headerSet = map[string]struct{}{}
	}
	policy.headerSet[http.CanonicalHeaderKey(name)] = struct{}{}
}

// cloneServiceWithClientCert returns a copy of svc whose http.Client uses
// a transport with a TLSConfig carrying the supplied certificate. The
// original svc and its transport are not modified; the clone is
// ephemeral (one call) so the registry's pool stays clean.
func cloneServiceWithClientCert(svc *serviceClient, cert tls.Certificate) (*serviceClient, error) {
	if svc.transport == nil {
		return nil, fmt.Errorf("httpclient: service %q has no transport (skeleton client)", svc.name)
	}
	transportClone := svc.transport.Clone()
	var tlsCfg *tls.Config
	if svc.tlsConfig != nil {
		tlsCfg = svc.tlsConfig.Clone()
	} else {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tlsCfg.Certificates = []tls.Certificate{cert}
	transportClone.TLSClientConfig = tlsCfg

	clone := *svc
	clone.transport = transportClone
	clone.tlsConfig = tlsCfg
	clone.httpClient = &http.Client{Transport: transportClone, Timeout: svc.httpClient.Timeout}
	return &clone, nil
}

// mergeHeaders applies the cascade (caller → callee) returning a fresh map.
// callee wins on conflict; nil inputs are tolerated and treated as empty.
func mergeHeaders(a, b map[string]string) map[string]string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func statusSet(codes []int) map[int]struct{} {
	if len(codes) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(codes))
	for _, c := range codes {
		out[c] = struct{}{}
	}
	return out
}
