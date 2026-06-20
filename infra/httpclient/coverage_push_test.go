package httpclient

import (
	"strings"
	"testing"
	"time"
)

// --- resolveCachePolicy remaining branches ---

func TestResolveCachePolicy_NilEndpointIsEmpty(t *testing.T) {
	if p := resolveCachePolicy(&CacheDefaults{}, nil); p.enabled {
		t.Error("nil endpoint must yield a disabled policy")
	}
}

func TestResolveCachePolicy_FrameworkTTLAndVaryOn(t *testing.T) {
	// defaults nil + endpoint TTL 0 → framework default TTL; honor override true.
	honor := true
	d := &CacheDefaults{HonorCacheControl: &honor}
	e := &EndpointCacheConfig{
		VaryOn: []string{"header:X-Tenant", "query:locale", "  ", "garbage"},
	}
	p := resolveCachePolicy(d, e)
	if !p.enabled {
		t.Fatal("expected enabled policy")
	}
	if p.ttl != frameworkCacheDefaultTTL {
		t.Errorf("expected framework default TTL, got %v", p.ttl)
	}
	if !p.honorCacheControl {
		t.Error("expected honorCacheControl from defaults override")
	}
	if len(p.varyHeaders) != 1 || p.varyHeaders[0] != "X-Tenant" {
		t.Errorf("varyHeaders = %v", p.varyHeaders)
	}
	if len(p.varyQueries) != 1 || p.varyQueries[0] != "locale" {
		t.Errorf("varyQueries = %v", p.varyQueries)
	}
}

func TestResolveCachePolicy_DefaultsTTLUsedWhenEndpointZero(t *testing.T) {
	d := &CacheDefaults{DefaultTTL: Duration(2 * time.Minute)}
	e := &EndpointCacheConfig{} // TTL 0 → falls back to defaults.DefaultTTL
	p := resolveCachePolicy(d, e)
	if p.ttl != 2*time.Minute {
		t.Errorf("expected defaults DefaultTTL, got %v", p.ttl)
	}
}

// --- resolveRedactionPolicy: empty/whitespace entries skipped ---

func TestResolveRedactionPolicy_SkipsBlankEntries(t *testing.T) {
	cfg := &RedactionConfig{
		Headers:      []string{"  ", "X-Real"},
		BodyJSONPath: []string{"", "  ", "$.secret"},
		QueryKeys:    []string{"", "Token"},
	}
	p := resolveRedactionPolicy(cfg, nil)
	if len(p.bodyJSONPaths) != 1 || p.bodyJSONPaths[0] != "$.secret" {
		t.Errorf("bodyJSONPaths = %v", p.bodyJSONPaths)
	}
	if _, ok := p.queryKeys["token"]; !ok {
		t.Error("expected lowercased 'token' query key")
	}
}

// --- normalizeSignedHeaders ---

func TestNormalizeSignedHeaders(t *testing.T) {
	if got := normalizeSignedHeaders(nil); got != nil {
		t.Errorf("empty input must yield nil, got %v", got)
	}
	got := normalizeSignedHeaders([]string{"Host", "  ", "host", "X-Date"})
	if len(got) != 2 || got[0] != "host" || got[1] != "x-date" {
		t.Errorf("expected deduped+sorted lowercase, got %v", got)
	}
}

// --- resolveContentSHA256Header ---

func TestResolveContentSHA256Header(t *testing.T) {
	if got := resolveContentSHA256Header(""); got != "X-Content-SHA256" {
		t.Errorf("empty must default, got %q", got)
	}
	if got := resolveContentSHA256Header("-"); got != "" {
		t.Errorf("dash must disable, got %q", got)
	}
	if got := resolveContentSHA256Header("  X-Hash "); got != "X-Hash" {
		t.Errorf("custom must trim, got %q", got)
	}
}

// --- buildInlineAuthProvider edge cases ---

func TestBuildInlineAuthProvider_Edges(t *testing.T) {
	if _, err := buildInlineAuthProvider(&InlineAuth{}); err == nil {
		t.Error("expected error when no credential set")
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{Bearer: "t", Basic: &BasicAuth{Username: "u", Password: "p"}}); err == nil {
		t.Error("expected error when multiple credentials set")
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{Bearer: "tok"}); err != nil {
		t.Errorf("bearer should build: %v", err)
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{APIKey: &APIKeyAuth{}}); err == nil {
		t.Error("expected error for APIKey with empty Value")
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{APIKey: &APIKeyAuth{Value: "k"}}); err != nil {
		t.Errorf("apikey default header should build: %v", err)
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{Basic: &BasicAuth{Username: "u"}}); err == nil {
		t.Error("expected error for Basic with missing password")
	}
	if _, err := buildInlineAuthProvider(&InlineAuth{Basic: &BasicAuth{Username: "u", Password: "p"}}); err != nil {
		t.Errorf("basic should build: %v", err)
	}
}

// --- resolveAuthProvider: no-auth-configured branches ---

func TestResolveAuthProvider_NoProviderName(t *testing.T) {
	c := &HttpClient{}
	svc := &serviceClient{name: "svc"} // no authProvider
	p, rev, err := c.resolveAuthProvider(svc, "")
	if p != nil || rev || err != nil {
		t.Errorf("expected nil provider, false, nil; got %v %v %v", p, rev, err)
	}
}

func TestResolveAuthProvider_OverrideWithoutRegistryErrors(t *testing.T) {
	c := &HttpClient{} // c.auth == nil
	svc := &serviceClient{name: "svc"}
	if _, _, err := c.resolveAuthProvider(svc, "missing"); err == nil ||
		!strings.Contains(err.Error(), "CallConfig.AuthProvider") {
		t.Errorf("expected override-without-registry error, got %v", err)
	}
}

func TestResolveAuthProvider_ServiceRefWithoutRegistryErrors(t *testing.T) {
	c := &HttpClient{} // c.auth == nil
	svc := &serviceClient{name: "svc", authProvider: "p1"}
	if _, _, err := c.resolveAuthProvider(svc, ""); err == nil ||
		!strings.Contains(err.Error(), "references provider") {
		t.Errorf("expected service-ref-without-registry error, got %v", err)
	}
}

// --- validateSigningConfig remaining branches ---

func TestValidateSigningConfig_RemainingBranches(t *testing.T) {
	cfg := &SigningConfig{
		Type:            "hmac-sha256",
		Secret:          "s",
		SignedHeaders:   []string{"host", "  "}, // empty entry → emit + break
		TimestampHeader: "X-Date",
		TimestampFormat: "weird", // unsupported → emit
		SignatureHeader: "X-Sig",
		KeyIdHeader:     "X-Kid", // set, but KeyId empty → emit
	}
	errs := validateSigningConfig("sign", cfg)
	joined := strings.Join(errs, "\n")
	for _, want := range []string{"contains an empty entry", "timestampFormat", "keyId: required"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in %v", want, errs)
		}
	}
}

// --- validateRedactionConfig empty bodyJSONPath entry ---

func TestValidateRedactionConfig_EmptyBodyPathEntry(t *testing.T) {
	errs := validateRedactionConfig("r", &RedactionConfig{BodyJSONPath: []string{"  "}})
	if len(errs) == 0 || !strings.Contains(errs[0], "bodyJSONPath[0]: empty") {
		t.Errorf("expected empty-bodyJSONPath error, got %v", errs)
	}
}

// --- mergeRetryConfig / policyFromConfig / applyRetryOnEntry branches ---

func TestMergeRetryConfig_CopiesSlicesAndRespectFlag(t *testing.T) {
	yes := true
	defaults := &RetryConfig{RetryOn: []string{"network"}}
	endpoint := &RetryConfig{
		MaxAttempts:       3,
		Backoff:           "linear",
		InitialDelay:      Duration(time.Second),
		MaxDelay:          Duration(2 * time.Second),
		RetryOn:           []string{"dns"},
		RespectRetryAfter: &yes,
	}
	out := mergeRetryConfig(defaults, endpoint)
	if out == nil || out.MaxAttempts != 3 || out.RespectRetryAfter == nil || !*out.RespectRetryAfter {
		t.Fatalf("merge result unexpected: %+v", out)
	}
	if len(out.RetryOn) != 1 || out.RetryOn[0] != "dns" {
		t.Errorf("endpoint RetryOn should win: %v", out.RetryOn)
	}
}

func TestPolicyFromConfig_DefaultsAndDNSEntry(t *testing.T) {
	// MaxAttempts 0 → enabled-default; RetryOn includes dns/timeout/network
	// to drive every applyRetryOnEntry sentinel branch.
	p := policyFromConfig(&RetryConfig{RetryOn: []string{"dns", "timeout", "network", "503"}})
	if p.maxAttempts != frameworkRetryEnabledDefault {
		t.Errorf("expected enabled-default maxAttempts, got %d", p.maxAttempts)
	}
	if !p.retryOnDNS || !p.retryOnTimeout || !p.retryOnNetwork {
		t.Errorf("expected all sentinels set, got %+v", p)
	}
	if _, ok := p.retryOnStatus[503]; !ok {
		t.Error("expected status 503 in retryOnStatus")
	}
}

// --- validateDefaults: negative timeout branch ---

func TestValidate_NegativeDefaultsTimeout(t *testing.T) {
	c := &Config{Defaults: Defaults{Timeout: Duration(-1)}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "defaults.timeout") {
		t.Errorf("expected negative-timeout error, got %v", err)
	}
}

// TestApplyDefaults_NilReceiver covers the c == nil guard.
func TestApplyDefaults_NilReceiver(t *testing.T) {
	var c *Config
	c.applyDefaults() // must not panic
}

// TestValidate_EveryDefaultsAndEndpointSubValidatorFires builds one Config
// that makes every defaults-level sub-validator AND every endpoint-level
// sub-validator emit at least one message, driving the accumulation loop
// bodies in validateDefaults / validateService / validateEndpoint.
func TestValidate_EveryDefaultsAndEndpointSubValidatorFires(t *testing.T) {
	c := &Config{
		Defaults: Defaults{
			Retry:          &RetryConfig{MaxAttempts: -1},
			Cache:          &CacheDefaults{DefaultTTL: Duration(-1)},
			CircuitBreaker: &CircuitBreakerConfig{FailureThreshold: -1},
			TLS:            &TLSConfig{MinVersion: "9.9"},
			Pool:           &PoolConfig{MaxConnsPerHost: -1},
			Redaction:      &RedactionConfig{BodyJSONPath: []string{"no-dollar-prefix"}},
		},
		Services: map[string]ServiceConfig{
			"s": {
				BaseURL:   "https://s.example.com",
				Timeout:   Duration(-1),
				Pool:      &PoolConfig{MaxConnsPerHost: -1},
				Redaction: &RedactionConfig{BodyJSONPath: []string{"bad"}},
				Endpoints: map[string]EndpointConfig{
					"e": {
						Method:        "GET",
						Path:          "/x",
						ResponseCodec: "yaml", // unsupported → emits
						Retry:         &RetryConfig{MaxAttempts: -1},
						Cache:         &EndpointCacheConfig{TTL: Duration(-1)},
						Idempotency:   &IdempotencyConfig{Source: "bogus"}, // empty header + bad source
					},
				},
			},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a multi-issue validation error")
	}
	for _, want := range []string{
		"defaults.retry", "defaults.cache", "defaults.circuitBreaker",
		"defaults.tls", "defaults.pool", "defaults.redaction",
		"services.s.timeout", "services.s.pool", "services.s.redaction",
		"responseCodec", "retry", "cache", "idempotency",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q; full: %v", want, err)
		}
	}
}
