package httpclient

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestConfig_ApplyDefaults_FillsBlanks(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Defaults.Timeout.ToTime() != 30*time.Second {
		t.Errorf("Timeout default = %v, want 30s", c.Defaults.Timeout.ToTime())
	}
	if c.Defaults.ThreadIDHeader != "X-Thread-Id" {
		t.Errorf("ThreadIDHeader default = %q", c.Defaults.ThreadIDHeader)
	}
	if c.Defaults.RequestIDHeader != "X-Request-ID" {
		t.Errorf("RequestIDHeader default = %q", c.Defaults.RequestIDHeader)
	}
	if c.Defaults.LogBodies == nil || *c.Defaults.LogBodies != true {
		t.Errorf("LogBodies default not true: %v", c.Defaults.LogBodies)
	}
}

func TestConfig_ApplyDefaults_PreservesExplicit(t *testing.T) {
	f := false
	c := &Config{
		Defaults: Defaults{
			Timeout:         Duration(5 * time.Second),
			ThreadIDHeader:  "X-Custom-Thread",
			RequestIDHeader: "X-Custom-Req",
			LogBodies:       &f,
		},
	}
	c.applyDefaults()
	if c.Defaults.Timeout.ToTime() != 5*time.Second {
		t.Errorf("Timeout overwritten: got %v", c.Defaults.Timeout.ToTime())
	}
	if c.Defaults.ThreadIDHeader != "X-Custom-Thread" {
		t.Errorf("ThreadIDHeader overwritten: %q", c.Defaults.ThreadIDHeader)
	}
	if c.Defaults.RequestIDHeader != "X-Custom-Req" {
		t.Errorf("RequestIDHeader overwritten: %q", c.Defaults.RequestIDHeader)
	}
	if *c.Defaults.LogBodies != false {
		t.Errorf("LogBodies false overwritten: %v", *c.Defaults.LogBodies)
	}
}

func TestConfig_ApplyDefaults_DefaultsEndpointCodecs(t *testing.T) {
	c := &Config{
		Services: map[string]ServiceConfig{
			"s": {
				BaseURL: "https://s.example.com",
				Endpoints: map[string]EndpointConfig{
					"e": {Method: "GET", Path: "/x"},
				},
			},
		},
	}
	c.applyDefaults()
	ep := c.Services["s"].Endpoints["e"]
	if ep.RequestCodec != "json" {
		t.Errorf("RequestCodec default = %q, want json", ep.RequestCodec)
	}
	if ep.ResponseCodec != "json" {
		t.Errorf("ResponseCodec default = %q, want json", ep.ResponseCodec)
	}
}

func TestConfig_Validate_NilOK(t *testing.T) {
	var c *Config
	if err := c.Validate(); err != nil {
		t.Errorf("nil config Validate: %v", err)
	}
}

func TestConfig_Validate_EmptyOK(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("empty config Validate: %v", err)
	}
}

func TestConfig_Validate_AcceptsEmptyBaseURL(t *testing.T) {
	// Phase 9: baseURL is optional. Services that defer routing to a
	// runtime BaseURLResolver leave it empty. Validate must accept the
	// shape; the call-time error surfaces only when both the resolver and
	// the YAML are empty.
	c := &Config{Services: map[string]ServiceConfig{
		"s": {Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}}},
	}}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Errorf("empty baseURL should validate (resolver path): %v", err)
	}
}

func TestConfig_Validate_RejectsBadURL(t *testing.T) {
	c := &Config{Services: map[string]ServiceConfig{
		"s": {BaseURL: "not a url", Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}}},
	}}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid absolute URL") {
		t.Errorf("expected URL parse error, got: %v", err)
	}
}

func TestConfig_Validate_RequiresEndpoint(t *testing.T) {
	c := &Config{Services: map[string]ServiceConfig{
		"s": {BaseURL: "https://s.example.com"},
	}}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "endpoints") {
		t.Errorf("expected endpoints requirement error, got: %v", err)
	}
}

func TestConfig_Validate_RequiresMethodAndPath(t *testing.T) {
	cases := []struct {
		name string
		ec   EndpointConfig
		want string
	}{
		{"empty method", EndpointConfig{Path: "/x"}, "method: required"},
		{"empty path", EndpointConfig{Method: "GET"}, "path: required"},
		{"bad method", EndpointConfig{Method: "SEND", Path: "/x"}, "not a supported HTTP method"},
		{"bad path", EndpointConfig{Method: "GET", Path: "users"}, "must start with '/'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Services: map[string]ServiceConfig{
				"s": {BaseURL: "https://s.example.com", Endpoints: map[string]EndpointConfig{"e": tc.ec}},
			}}
			c.applyDefaults()
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestConfig_Validate_RejectsUnsupportedCodec(t *testing.T) {
	c := &Config{Services: map[string]ServiceConfig{
		"s": {BaseURL: "https://s.example.com", Endpoints: map[string]EndpointConfig{
			"e": {Method: "GET", Path: "/x", RequestCodec: "yaml"},
		}},
	}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not one of json|xml|form") {
		t.Errorf("expected unsupported codec error, got: %v", err)
	}
}

func TestConfig_Validate_RejectsBadStatusCode(t *testing.T) {
	c := &Config{Services: map[string]ServiceConfig{
		"s": {BaseURL: "https://s.example.com", Endpoints: map[string]EndpointConfig{
			"e": {Method: "GET", Path: "/x", AcceptableStatus: []int{42}},
		}},
	}}
	c.applyDefaults()
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid HTTP status code") {
		t.Errorf("expected status code error, got: %v", err)
	}
}

func TestConfig_Validate_NegativeTimeout(t *testing.T) {
	c := &Config{
		Defaults: Defaults{Timeout: Duration(-1)},
		Services: map[string]ServiceConfig{
			"s": {BaseURL: "https://s.example.com", Endpoints: map[string]EndpointConfig{
				"e": {Method: "GET", Path: "/x"},
			}},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "defaults.timeout") {
		t.Errorf("expected defaults.timeout error, got: %v", err)
	}
}

func TestConfig_Validate_AccumulatesAllIssues(t *testing.T) {
	// Validate must accumulate every issue across the YAML so the
	// operator sees the whole picture on one boot attempt. This test
	// stresses the accumulator with several distinct violations spread
	// across the auth provider, the signing block, and the endpoint
	// (responseStream + responseSSE mutually exclusive, cache + stream).
	yml := `
defaults:
  retry: {maxAttempts: 3}
  cache: {enabled: true}
  circuitBreaker: {enabled: true}
  pool: {maxConnsPerHost: 50}
  tls: {minVersion: "1.2"}
  redaction: {headers: [X]}
services:
  s:
    baseURL: https://s.example.com
    auth: {provider: x}
    tls: {minVersion: "1.0"}
    pool: {maxConnsPerHost: 10}
    signing: {type: hmac}
    redaction: {headers: [Y]}
    endpoints:
      e:
        method: GET
        path: /x
        cache: {ttl: 1m}
        retry: {maxAttempts: 1}
        idempotency: {header: X-Idempotency-Key}
        responseStream: true
        responseSSE: true
authProviders:
  x:
    type: oauth2-client-credentials
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	expected := []string{
		// signing block now real — invalid type + missing required fields
		`services.s.signing.type: "hmac" is not supported`,
		`services.s.signing.secret`,
		`services.s.signing.signedHeaders`,
		`services.s.signing.timestampHeader`,
		`services.s.signing.signatureHeader`,
		// streaming flags now real — mutual exclusivity + cache conflict
		"responseStream and responseSSE are mutually exclusive",
		"cache: cannot coexist with responseStream",
		"cache: cannot coexist with responseSSE",
		// oauth2-client-credentials missing required fields
		"authProviders.x.tokenEndpoint",
		"authProviders.x.clientId",
		"authProviders.x.clientSecret",
		"authProviders.x.tokenCache",
	}
	for _, want := range expected {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error should mention %q; got:\n%s", want, err)
		}
	}
}

// --- Duration ---

func TestDuration_UnmarshalYAML_ParsesStandardForms(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"5m", 5 * time.Minute},
		{"1h", time.Hour},
		{"100ms", 100 * time.Millisecond},
	}
	for _, tc := range cases {
		var d Duration
		if err := yaml.Unmarshal([]byte(tc.in), &d); err != nil {
			t.Errorf("Unmarshal %q: %v", tc.in, err)
			continue
		}
		if d.ToTime() != tc.want {
			t.Errorf("%q → %v, want %v", tc.in, d.ToTime(), tc.want)
		}
	}
}

func TestDuration_UnmarshalYAML_RejectsInvalid(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte("30x"), &d); err == nil {
		t.Fatal("expected error for invalid duration")
	}
}

func TestDuration_UnmarshalYAML_EmptyIsZero(t *testing.T) {
	var d Duration
	if err := yaml.Unmarshal([]byte(`""`), &d); err != nil {
		t.Fatalf("Unmarshal empty: %v", err)
	}
	if d != 0 {
		t.Errorf("empty string should yield 0; got %v", d)
	}
}

// --- Full YAML round-trip ---

func TestConfig_FullYAML_ParsesAndValidates(t *testing.T) {
	yml := `
defaults:
  timeout: 30s
  threadIdHeader: X-Thread-Id
  requestIdHeader: X-Request-ID
  logBodies: true
  headers:
    User-Agent: "omnicore-svc/1.0"
services:
  keycloak:
    baseURL: https://kc.example.com
    timeout: 10s
    headers:
      X-Tenant: acme
    endpoints:
      getUser:
        method: GET
        path: /users/{id}
        acceptableStatus: [404]
        headers:
          Accept: application/json
`
	var c Config
	if err := yaml.Unmarshal([]byte(yml), &c); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Defaults.Timeout.ToTime() != 30*time.Second {
		t.Errorf("Defaults.Timeout = %v", c.Defaults.Timeout.ToTime())
	}
	kc := c.Services["keycloak"]
	if kc.BaseURL != "https://kc.example.com" {
		t.Errorf("BaseURL = %q", kc.BaseURL)
	}
	if kc.Timeout.ToTime() != 10*time.Second {
		t.Errorf("service Timeout = %v", kc.Timeout.ToTime())
	}
	ep := kc.Endpoints["getUser"]
	if ep.Method != "GET" || ep.Path != "/users/{id}" {
		t.Errorf("endpoint = %+v", ep)
	}
	if len(ep.AcceptableStatus) != 1 || ep.AcceptableStatus[0] != 404 {
		t.Errorf("AcceptableStatus = %v", ep.AcceptableStatus)
	}
}
