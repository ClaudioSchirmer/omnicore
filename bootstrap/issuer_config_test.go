package bootstrap

import (
	"strings"
	"testing"
)

const fakePEM = "x"

// baseYAMLNoAuth is the minimal service scaffold every issuer test builds
// on top of — no `auth:` block, so Mode defaults to disabled and each test
// controls exactly the auth shape it needs.
const baseYAMLNoAuth = `service: test
relational: { dialect: postgres, dsn: "postgres://x" }
mongo: { uri: "mongodb://x", database: "v" }
transport:
  endpoints: ["k:1"]
  syncGroup: "g"
`

const (
	defaultIssuerSelfURL     = "http://auth"
	defaultIssuerAudience    = `["users-api"]`
	defaultIssuerTokenTTL    = "900"
	defaultIssuerMaxTokenTTL = "3600"
	defaultIssuerKeysBlock   = "      - kid: \"k1\"\n        algorithm: RS256\n        state: current\n        privateKeyPem: \"" + fakePEM + "\"\n"
)

// issuerYAML assembles a full microservice.yaml with an `auth.issuer:`
// block from explicit field values — empty string omits that yaml line
// entirely, so a guard test can target exactly one missing/invalid field
// without duplicating a key already written by a "valid" baseline.
func issuerYAML(selfURL, audience, tokenTTL, maxTokenTTL, refreshTTL, keysBlock, jwksBlock string) string {
	b := baseYAMLNoAuth + "auth:\n  issuer:\n    enabled: true\n"
	if selfURL != "" {
		b += "    selfUrl: \"" + selfURL + "\"\n"
	}
	if audience != "" {
		b += "    audience: " + audience + "\n"
	}
	if tokenTTL != "" {
		b += "    tokenTtlSeconds: " + tokenTTL + "\n"
	}
	if maxTokenTTL != "" {
		b += "    maxTokenTtlSeconds: " + maxTokenTTL + "\n"
	}
	if refreshTTL != "" {
		b += "    refreshTokenTtlSeconds: " + refreshTTL + "\n"
	}
	if keysBlock != "" {
		b += "    keys:\n" + keysBlock
	}
	if jwksBlock != "" {
		b += "    jwks:\n" + jwksBlock
	}
	return b
}

// validIssuerYAML is the fully-valid baseline every guard test starts from,
// overriding exactly one field via issuerYAML's parameters.
func validIssuerYAML() string {
	return issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL,
		defaultIssuerMaxTokenTTL, "", defaultIssuerKeysBlock, "")
}

// --- closed key set ----------------------------------------------------------

func TestIssuerConfig_RejectsUnknownField(t *testing.T) {
	yml := baseYAMLNoAuth + `auth:
  issuer:
    enabled: true
    selfUrl: "http://auth"
    bogus: true
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "auth.issuer") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected auth.issuer unknown-field error, got %v", err)
	}
}

func TestIssuerKeyConfig_RejectsUnknownField(t *testing.T) {
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
		"      - kid: \"k1\"\n        algorithm: RS256\n        state: current\n        privateKeyPem: \"x\"\n        bogus: true\n", "")
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "auth.issuer.keys") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected auth.issuer.keys unknown-field error, got %v", err)
	}
}

func TestIssuerKeyConfig_RejectsMalformedNode(t *testing.T) {
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
		"      - \"just a string, not a mapping\"\n", "")
	if _, err := LoadConfigFrom(writeTemp(t, yml)); err == nil {
		t.Fatal("expected a yaml decode error for a scalar key entry")
	}
}

func TestIssuerJWKSConfig_RejectsMalformedNode(t *testing.T) {
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
		defaultIssuerKeysBlock, "")
	yml += "    jwks: \"just a string, not a mapping\"\n"
	if _, err := LoadConfigFrom(writeTemp(t, yml)); err == nil {
		t.Fatal("expected a yaml decode error for a scalar jwks value")
	}
}

func TestIssuerJWKSConfig_RejectsUnknownField(t *testing.T) {
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
		defaultIssuerKeysBlock, "      path: /.well-known/jwks.json\n      bogus: true\n")
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "auth.issuer.jwks") || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected auth.issuer.jwks unknown-field error, got %v", err)
	}
}

// --- applyDefaults -----------------------------------------------------------

func TestIssuerConfig_JWKSPathDefaults(t *testing.T) {
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
		defaultIssuerKeysBlock, "")
	yml += "    jwks: {}\n"
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.Issuer.JWKS.Path != defaultIssuerJWKSPath {
		t.Errorf("JWKS.Path default = %q, want %q", cfg.Auth.Issuer.JWKS.Path, defaultIssuerJWKSPath)
	}
}

func TestIssuerConfig_NoJWKSBlockMeansNoRoute(t *testing.T) {
	cfg, err := LoadConfigFrom(writeTemp(t, validIssuerYAML()))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.Issuer.JWKS != nil {
		t.Errorf("JWKS should stay nil when the block is absent, got %#v", cfg.Auth.Issuer.JWKS)
	}
}

func TestIssuerConfig_DisabledSkipsDefaultsAndValidation(t *testing.T) {
	yml := baseYAMLNoAuth + `auth:
  issuer:
    enabled: false
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.Issuer.SelfURL != "" {
		t.Errorf("disabled issuer should not be touched by defaults, got SelfURL=%q", cfg.Auth.Issuer.SelfURL)
	}
}

// --- successful full round trip ----------------------------------------------

func TestIssuerConfig_FullRoundTrip(t *testing.T) {
	keys := defaultIssuerKeysBlock +
		"      - kid: \"k0\"\n        algorithm: RS256\n        state: previous\n        privateKeyPem: \"x\"\n"
	yml := issuerYAML(defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL,
		"2592000", keys, "      path: /.well-known/jwks.json\n")
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	ic := cfg.Auth.Issuer
	if !ic.Enabled || ic.SelfURL != "http://auth" || len(ic.Audience) != 1 {
		t.Fatalf("unexpected IssuerConfig: %#v", ic)
	}
	if len(ic.Keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(ic.Keys))
	}
	if ic.RefreshTokenTTLSeconds != 2592000 {
		t.Errorf("RefreshTokenTTLSeconds = %d, want 2592000", ic.RefreshTokenTTLSeconds)
	}
}

// --- boot guards ---------------------------------------------------------

func TestIssuerConfig_BootGuards(t *testing.T) {
	cases := []struct {
		name          string
		selfURL       string
		audience      string
		tokenTTL      string
		maxTokenTTL   string
		refreshTTL    string
		keysBlock     string
		jwksBlock     string
		wantErrSubstr string
	}{
		{"selfUrl required", "", defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "", defaultIssuerKeysBlock, "",
			"selfUrl is required"},
		{"audience required", defaultIssuerSelfURL, "[]", defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "", defaultIssuerKeysBlock, "",
			"audience is required"},
		{"tokenTtlSeconds must be positive", defaultIssuerSelfURL, defaultIssuerAudience, "0", defaultIssuerMaxTokenTTL, "", defaultIssuerKeysBlock, "",
			"tokenTtlSeconds must be > 0"},
		{"maxTokenTtlSeconds must be positive", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, "0", "", defaultIssuerKeysBlock, "",
			"maxTokenTtlSeconds must be > 0"},
		{"maxTokenTtlSeconds must be >= tokenTtlSeconds", defaultIssuerSelfURL, defaultIssuerAudience, "3600", "900", "", defaultIssuerKeysBlock, "",
			"must be >= tokenTtlSeconds"},
		{"refreshTokenTtlSeconds rejects negative", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "-1", defaultIssuerKeysBlock, "",
			"refreshTokenTtlSeconds must be >= 0"},
		{"keys required", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "", "", "",
			"keys must declare at least one key"},
		{"kid required", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - algorithm: RS256\n        state: current\n        privateKeyPem: \"x\"\n", "",
			"kid is required"},
		{"algorithm must be asymmetric", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - kid: \"k1\"\n        algorithm: HS256\n        state: current\n        privateKeyPem: \"x\"\n", "",
			`algorithm "HS256" is invalid`},
		{"state must be valid", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - kid: \"k1\"\n        algorithm: RS256\n        state: bogus\n        privateKeyPem: \"x\"\n", "",
			`state "bogus" is invalid`},
		{"privateKeyPem required", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - kid: \"k1\"\n        algorithm: RS256\n        state: current\n        privateKeyPem: \"\"\n", "",
			"privateKeyPem is required"},
		{"exactly one current — zero", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - kid: \"k1\"\n        algorithm: RS256\n        state: next\n        privateKeyPem: \"x\"\n", "",
			`exactly one key with state="current"`},
		{"exactly one current — two", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			"      - kid: \"k1\"\n        algorithm: RS256\n        state: current\n        privateKeyPem: \"x\"\n" +
				"      - kid: \"k2\"\n        algorithm: RS256\n        state: current\n        privateKeyPem: \"x\"\n", "",
			`exactly one key with state="current"`},
		{"jwks path must start with slash", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			defaultIssuerKeysBlock, "      path: \"no-leading-slash\"\n",
			"must start with"},
		{"jwks path collision with framework route", defaultIssuerSelfURL, defaultIssuerAudience, defaultIssuerTokenTTL, defaultIssuerMaxTokenTTL, "",
			defaultIssuerKeysBlock, "      path: \"/livez\"\n",
			"collides with a framework route"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yml := issuerYAML(tc.selfURL, tc.audience, tc.tokenTTL, tc.maxTokenTTL, tc.refreshTTL, tc.keysBlock, tc.jwksBlock)
			_, err := LoadConfigFrom(writeTemp(t, yml))
			if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErrSubstr, err)
			}
		})
	}
}

// --- selfUrl vs auth.jwt.issuer agreement ------------------------------------

func TestIssuerConfig_SelfURLMustMatchJWTIssuerWhenBothConfigured(t *testing.T) {
	yml := baseYAMLNoAuth + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: omnicore-users
    jwksUrl: https://idp.example.com/.well-known/jwks.json
  issuer:
    enabled: true
    selfUrl: "http://auth"
    audience: ["users-api"]
    tokenTtlSeconds: 900
    maxTokenTtlSeconds: 3600
    keys:
      - kid: "k1"
        algorithm: RS256
        state: current
        privateKeyPem: "x"
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "must equal auth.jwt.issuer") {
		t.Fatalf("expected selfUrl/jwt.issuer mismatch error, got %v", err)
	}
}

func TestIssuerConfig_SelfURLMatchingJWTIssuerPasses(t *testing.T) {
	yml := baseYAMLNoAuth + `auth:
  mode: jwt
  jwt:
    issuer: "http://auth"
    audience: omnicore-users
    jwksUrl: https://idp.example.com/.well-known/jwks.json
  issuer:
    enabled: true
    selfUrl: "http://auth"
    audience: ["users-api"]
    tokenTtlSeconds: 900
    maxTokenTtlSeconds: 3600
    keys:
      - kid: "k1"
        algorithm: RS256
        state: current
        privateKeyPem: "x"
`
	if _, err := LoadConfigFrom(writeTemp(t, yml)); err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
}

// Issuer is orthogonal to Mode: a disabled-mode service (dev, no inbound
// auth) can still validly declare an issuer, and no cross-check against
// auth.jwt.issuer applies since there is no JWT block to disagree with.
func TestIssuerConfig_ValidatesIndependentlyOfDisabledMode(t *testing.T) {
	if _, err := LoadConfigFrom(writeTemp(t, validIssuerYAML())); err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
}
