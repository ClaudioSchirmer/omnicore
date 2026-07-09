package bootstrap

import (
	"strings"
	"testing"
)

const validYAMLWithJWT = `service: test
relational: { dialect: postgres, dsn: "postgres://x" }
mongo: { uri: "mongodb://x", database: "v" }
transport:
  endpoints: ["k:1"]
  syncGroup: "g"
auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: omnicore-users
    jwksUrl: https://idp.example.com/.well-known/jwks.json
`

func TestAuthConfig_DefaultsModeDisabledWhenAbsent(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired) // no `auth:` block
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.Mode != AuthModeDisabled {
		t.Errorf("Auth.Mode default = %q, want %q", cfg.Auth.Mode, AuthModeDisabled)
	}
	if cfg.Auth.JWT != nil {
		t.Errorf("JWT block should be nil when auth block absent, got %#v", cfg.Auth.JWT)
	}
}

func TestAuthConfig_JWTDefaultsAlgorithms(t *testing.T) {
	path := writeTemp(t, validYAMLWithJWT)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	got := cfg.Auth.JWT.Algorithms
	if len(got) != len(defaultJWTAlgorithms) {
		t.Fatalf("Algorithms default len = %d, want %d", len(got), len(defaultJWTAlgorithms))
	}
	for i, alg := range defaultJWTAlgorithms {
		if got[i] != alg {
			t.Errorf("Algorithms[%d] = %q, want %q", i, got[i], alg)
		}
	}
}

func TestAuthConfig_ExternalValidatorDefaults(t *testing.T) {
	yml := validYAMLWithJWT + `  externalValidator:
    url: https://idp.example.com/introspect
    tokenPlacement: form_field
    tokenField: token
    success:
      jsonPath: $.active
      expectedValue: true
`
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	ev := cfg.Auth.ExternalValidator
	if ev == nil {
		t.Fatal("ExternalValidator should be parsed, got nil")
	}
	if ev.Method != "POST" {
		t.Errorf("Method default = %q, want POST", ev.Method)
	}
	if ev.FailMode != FailModeClosed {
		t.Errorf("FailMode default = %q, want %q", ev.FailMode, FailModeClosed)
	}
	if ev.TimeoutMS != 2000 {
		t.Errorf("TimeoutMS default = %d, want 2000", ev.TimeoutMS)
	}
}

func TestAuthConfig_FullRoundTrip(t *testing.T) {
	yml := `service: test
relational: { dialect: postgres, dsn: "postgres://x" }
mongo: { uri: "mongodb://x", database: "v" }
transport:
  endpoints: ["k:1"]
  syncGroup: "g"
auth:
  mode: jwt
  jwt:
    algorithms: [RS256]
    issuer: https://idp.example.com
    audience: omnicore-users
    leewaySeconds: 60
    publicKeyPem: |
      -----BEGIN PUBLIC KEY-----
      AAAA
      -----END PUBLIC KEY-----
  externalValidator:
    method: POST
    url: https://idp.example.com/introspect
    tokenPlacement: form_field
    tokenField: token
    extraHeaders:
      Authorization: "Basic abc"
    success:
      jsonPath: $.active
      expectedValue: true
    timeoutMs: 1500
    failMode: open
  publicRoutes:
    - GET /health
    - GET /ready
`
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.Mode != AuthModeJWT {
		t.Errorf("Mode = %q", cfg.Auth.Mode)
	}
	if len(cfg.Auth.JWT.Algorithms) != 1 || cfg.Auth.JWT.Algorithms[0] != "RS256" {
		t.Errorf("Algorithms = %#v", cfg.Auth.JWT.Algorithms)
	}
	if cfg.Auth.JWT.LeewaySeconds != 60 {
		t.Errorf("LeewaySeconds = %d", cfg.Auth.JWT.LeewaySeconds)
	}
	if !strings.Contains(cfg.Auth.JWT.PublicKeyPEM, "BEGIN PUBLIC KEY") {
		t.Errorf("PublicKeyPEM not parsed: %q", cfg.Auth.JWT.PublicKeyPEM)
	}
	ev := cfg.Auth.ExternalValidator
	if ev == nil {
		t.Fatal("ExternalValidator nil")
	}
	if ev.FailMode != FailModeOpen {
		t.Errorf("FailMode = %q, want %q (explicit override)", ev.FailMode, FailModeOpen)
	}
	if ev.TimeoutMS != 1500 {
		t.Errorf("TimeoutMS = %d", ev.TimeoutMS)
	}
	if got := ev.ExtraHeaders["Authorization"]; got != "Basic abc" {
		t.Errorf("ExtraHeaders[Authorization] = %q", got)
	}
	if len(cfg.Auth.PublicRoutes) != 2 {
		t.Errorf("PublicRoutes = %#v", cfg.Auth.PublicRoutes)
	}
}

func TestAuthConfig_InvalidMode(t *testing.T) {
	yml := validYAMLAllRequired + "auth:\n  mode: oauth\n"
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "oauth") {
		t.Fatalf("expected error citing the invalid mode, got %v", err)
	}
}

func TestAuthConfig_JWTRequiresJWTBlock(t *testing.T) {
	yml := validYAMLAllRequired + "auth:\n  mode: jwt\n"
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "auth.jwt block") {
		t.Fatalf("expected error about missing jwt block, got %v", err)
	}
}

func TestAuthConfig_JWTRequiresIssuer(t *testing.T) {
	yml := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    audience: omnicore-users
    jwksUrl: https://idp/jwks
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("expected error about missing issuer, got %v", err)
	}
}

func TestAuthConfig_JWTRequiresAudience(t *testing.T) {
	yml := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    jwksUrl: https://idp/jwks
`
	path := writeTemp(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil || !strings.Contains(err.Error(), "audience") {
		t.Fatalf("expected error about missing audience, got %v", err)
	}
}

func TestAuthConfig_JWTRequiresExactlyOneKeySource(t *testing.T) {
	// neither
	ymlNone := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: aud
`
	if _, err := LoadConfigFrom(writeTemp(t, ymlNone)); err == nil || !strings.Contains(err.Error(), "jwksUrl") {
		t.Fatalf("expected error when neither jwksUrl nor publicKeyPem set, got %v", err)
	}
	// both
	ymlBoth := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: aud
    jwksUrl: https://idp/jwks
    publicKeyPem: "abc"
`
	if _, err := LoadConfigFrom(writeTemp(t, ymlBoth)); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected error when both jwksUrl and publicKeyPem set, got %v", err)
	}
}

func TestAuthConfig_JWTRejectsUnsupportedAlgorithm(t *testing.T) {
	yml := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    algorithms: [HS256]
    issuer: https://idp.example.com
    audience: aud
    jwksUrl: https://idp/jwks
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "HS256") {
		t.Fatalf("expected error citing HS256, got %v", err)
	}
}

func TestAuthConfig_ExternalValidatorValidations(t *testing.T) {
	base := `service: test
relational: { dialect: postgres, dsn: "postgres://x" }
mongo: { uri: "mongodb://x", database: "v" }
transport: { endpoints: ["k:1"], syncGroup: "g" }
auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: aud
    jwksUrl: https://idp/jwks
  externalValidator:
`
	cases := []struct {
		name string
		ext  string
		want string // substring expected in error
	}{
		{
			name: "missing URL",
			ext: `    method: POST
    tokenPlacement: form_field
    tokenField: token
    success: { jsonPath: $.active, expectedValue: true }
`,
			want: "url is required",
		},
		{
			name: "invalid method",
			ext: `    method: PATCH
    url: https://idp/introspect
    tokenPlacement: form_field
    tokenField: token
    success: { jsonPath: $.active, expectedValue: true }
`,
			want: "method",
		},
		{
			name: "invalid token placement",
			ext: `    method: POST
    url: https://idp/introspect
    tokenPlacement: cookie
    tokenField: token
    success: { jsonPath: $.active, expectedValue: true }
`,
			want: "tokenPlacement",
		},
		{
			name: "form placement without token field",
			ext: `    method: POST
    url: https://idp/introspect
    tokenPlacement: form_field
    success: { jsonPath: $.active, expectedValue: true }
`,
			want: "tokenField",
		},
		{
			name: "invalid fail mode",
			ext: `    method: POST
    url: https://idp/introspect
    tokenPlacement: bearer_header
    failMode: silent
    success: { jsonPath: $.active, expectedValue: true }
`,
			want: "failMode",
		},
		{
			name: "missing json path",
			ext: `    method: POST
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success: { expectedValue: true }
`,
			want: "jsonPath",
		},
		{
			name: "missing expected value",
			ext: `    method: POST
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success: { jsonPath: $.active }
`,
			want: "expectedValue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yml := base + tc.ext
			_, err := LoadConfigFrom(writeTemp(t, yml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestAuthConfig_CacheTTLDefaultIsZero(t *testing.T) {
	yml := validYAMLWithJWT + `  externalValidator:
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success:
      jsonPath: $.active
      expectedValue: true
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.ExternalValidator.CacheTTLSeconds != 0 {
		t.Errorf("CacheTTLSeconds default = %d, want 0 (cache disabled)", cfg.Auth.ExternalValidator.CacheTTLSeconds)
	}
}

func TestAuthConfig_CacheTTLExplicit(t *testing.T) {
	yml := validYAMLWithJWT + `  externalValidator:
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success:
      jsonPath: $.active
      expectedValue: true
    cacheTtlSeconds: 90
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.ExternalValidator.CacheTTLSeconds != 90 {
		t.Errorf("CacheTTLSeconds = %d, want 90", cfg.Auth.ExternalValidator.CacheTTLSeconds)
	}
}

func TestAuthConfig_AuditClaimsAllowlist(t *testing.T) {
	yml := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: aud
    jwksUrl: https://idp/jwks
  auditClaims:
    - tenant_id
    - roles
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	got := cfg.Auth.AuditClaims
	if len(got) != 2 || got[0] != "tenant_id" || got[1] != "roles" {
		t.Errorf("AuditClaims = %#v, want [tenant_id, roles]", got)
	}
}

func TestAuthConfig_AuditClaimsDefaultsEmpty(t *testing.T) {
	cfg, err := LoadConfigFrom(writeTemp(t, validYAMLWithJWT))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(cfg.Auth.AuditClaims) != 0 {
		t.Errorf("AuditClaims default = %#v, want empty", cfg.Auth.AuditClaims)
	}
}

func TestAuthConfig_CacheTTLRejectsNegative(t *testing.T) {
	yml := validYAMLWithJWT + `  externalValidator:
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success:
      jsonPath: $.active
      expectedValue: true
    cacheTtlSeconds: -5
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "cacheTtlSeconds") {
		t.Fatalf("expected error citing cacheTtlSeconds, got %v", err)
	}
}

func TestAuthConfig_BearerPlacementAllowsEmptyTokenField(t *testing.T) {
	yml := validYAMLWithJWT + `  externalValidator:
    method: POST
    url: https://idp/introspect
    tokenPlacement: bearer_header
    success:
      jsonPath: $.active
      expectedValue: true
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Auth.ExternalValidator.TokenField != "" {
		t.Errorf("TokenField = %q, want empty for bearer_header placement", cfg.Auth.ExternalValidator.TokenField)
	}
}
