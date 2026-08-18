package bootstrap

import (
	"strings"
	"testing"
)

// loadAudit parses a yaml carrying the supplied `audit:` block on top of the
// minimal valid config, returning the loaded config or the boot error.
func loadAudit(t *testing.T, auditBlock string) (*Config, error) {
	t.Helper()
	return LoadConfigFrom(writeTemp(t, validYAMLAllRequired+auditBlock))
}

// mustLoadAudit is loadAudit for the cases that must boot.
func mustLoadAudit(t *testing.T, auditBlock string) *Config {
	t.Helper()
	cfg, err := loadAudit(t, auditBlock)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	return cfg
}

// ─── presence is the switch ──────────────────────────────────────────────────

// The endpoint is opt-in: a service that never heard of it keeps the exact
// posture it had before the block existed.
func TestAuditEndpoint_AbsentMountsNothing(t *testing.T) {
	cfg := mustLoadAudit(t, "audit:\n  destinations: [slog, database]\n")
	if cfg.Audit.Endpoint != nil {
		t.Errorf("Endpoint = %+v, want nil when the block is absent", cfg.Audit.Endpoint)
	}
	if cfg.Audit.auditRESTEnabled() {
		t.Error("no endpoint block must mean no REST connector")
	}
}

// The endpoint block alone turns on nothing: each connector is its own switch,
// so a future connector cannot be enabled by accident.
func TestAuditEndpoint_WithoutAConnectorMountsNothing(t *testing.T) {
	cfg := mustLoadAudit(t, "audit:\n  destinations: [database]\n  endpoint: {}\n")
	if cfg.Audit.Endpoint == nil {
		t.Fatal("the endpoint block must survive parsing")
	}
	if cfg.Audit.auditRESTEnabled() {
		t.Error("no rest sub-block must mean no REST connector")
	}
}

func TestAuditEndpoint_EmptyRESTBlockMountsWithDefaults(t *testing.T) {
	cfg := mustLoadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    rest: {}\n")
	if !cfg.Audit.auditRESTEnabled() {
		t.Fatal("an empty rest block must still mount the connector")
	}
	rest := cfg.Audit.Endpoint.REST
	if rest.Path != defaultAuditRESTPath {
		t.Errorf("Path = %q, want the default %q", rest.Path, defaultAuditRESTPath)
	}
	if rest.PermissionValue() != defaultAuditRESTPermission {
		t.Errorf("Permission = %q, want the default %q", rest.PermissionValue(), defaultAuditRESTPermission)
	}
	if cfg.Audit.Endpoint.MaxLimit != FrameworkDefaultAuditMaxLimit {
		t.Errorf("MaxLimit = %d, want the default %d", cfg.Audit.Endpoint.MaxLimit, FrameworkDefaultAuditMaxLimit)
	}
	if !cfg.Audit.Endpoint.RenderLabelsEnabled() {
		t.Error("renderLabels must default to on")
	}
}

func TestAuditEndpoint_ExplicitValuesWin(t *testing.T) {
	cfg := mustLoadAudit(t, `audit:
  destinations: [database]
  endpoint:
    renderLabels: false
    maxLimit: 5
    rest:
      path: /trail
      permission: trail:read
`)
	ep := cfg.Audit.Endpoint
	if ep.MaxLimit != 5 || ep.RenderLabelsEnabled() {
		t.Errorf("shared knobs drifted: maxLimit=%d renderLabels=%v", ep.MaxLimit, ep.RenderLabelsEnabled())
	}
	if ep.REST.Path != "/trail" || ep.REST.PermissionValue() != "trail:read" {
		t.Errorf("rest knobs drifted: path=%q permission=%q", ep.REST.Path, ep.REST.PermissionValue())
	}
}

// The write half keeps working through the embedding — the read block is
// additive, not a replacement for how destinations are read.
func TestAuditEndpoint_DestinationsStillReachableThroughTheWrapper(t *testing.T) {
	cfg := mustLoadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    rest: {}\n")
	if len(cfg.Audit.Destinations) != 1 || !cfg.Audit.Includes("database") {
		t.Errorf("the embedded write config must stay reachable, got %#v", cfg.Audit.Destinations)
	}
}

// ─── the database precondition ───────────────────────────────────────────────

// The endpoint answers from audit_events. Routing audit to slog alone would
// mount a surface that can only ever answer an empty timeline, so the
// combination is refused — and the message says where to go instead.
func TestAuditEndpoint_RequiresTheDatabaseDestination(t *testing.T) {
	for _, dest := range []string{"[slog]", "[]"} {
		_, err := loadAudit(t, "audit:\n  destinations: "+dest+"\n  endpoint:\n    rest: {}\n")
		if err == nil {
			t.Fatalf("destinations=%s: expected the boot to refuse the endpoint", dest)
		}
		for _, want := range []string{"audit.endpoint", "database", "audit_events", "RenderLabelsInJSON"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("destinations=%s: diagnostic must mention %q, got: %v", dest, want, err)
			}
		}
	}
}

// The precondition is about the ENDPOINT, not about audit: a slog-only service
// that mounts nothing keeps booting.
func TestAuditEndpoint_SlogOnlyBootsWhenNoEndpointIsDeclared(t *testing.T) {
	if _, err := loadAudit(t, "audit:\n  destinations: [slog]\n"); err != nil {
		t.Fatalf("slog-only audit without an endpoint must boot: %v", err)
	}
}

// ─── window + path rules ─────────────────────────────────────────────────────

func TestAuditEndpoint_RejectsNegativeMaxLimit(t *testing.T) {
	_, err := loadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    maxLimit: -1\n    rest: {}\n")
	if err == nil || !strings.Contains(err.Error(), "maxLimit") {
		t.Errorf("expected a maxLimit rejection, got: %v", err)
	}
}

func TestAuditEndpoint_RejectsPathWithoutLeadingSlash(t *testing.T) {
	_, err := loadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    rest:\n      path: audit\n")
	if err == nil || !strings.Contains(err.Error(), "must start with") {
		t.Errorf("expected a path rejection, got: %v", err)
	}
}

func TestAuditEndpoint_RejectsFrameworkReservedPaths(t *testing.T) {
	for _, p := range []string{"/livez", "/readyz", "/openapi.json", "/docs"} {
		_, err := loadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    rest:\n      path: "+p+"\n")
		if err == nil || !strings.Contains(err.Error(), "framework route") {
			t.Errorf("path %q: expected a reserved-route rejection, got: %v", p, err)
		}
	}
}

// Two self-mounted surfaces on one path is a registration race whose winner
// depends on mount order — refused at boot rather than discovered in prod.
func TestAuditEndpoint_RejectsCollisionWithGraphQL(t *testing.T) {
	_, err := loadAudit(t, `audit:
  destinations: [database]
  endpoint:
    rest:
      path: /graphql
`)
	if err == nil || !strings.Contains(err.Error(), "graphql.path") {
		t.Errorf("expected a graphql.path collision, got: %v", err)
	}
}

func TestAuditEndpoint_RejectsCollisionWithGraphQLPlaygroundPath(t *testing.T) {
	yml := validYAMLAllRequired + `graphql:
  playground: true
  uiPath: /trail
audit:
  destinations: [database]
  endpoint:
    rest:
      path: /trail
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "graphql.uiPath") {
		t.Errorf("expected a graphql.uiPath collision, got: %v", err)
	}
}

func TestAuditEndpoint_RejectsCollisionWithOpenAPIUIPath(t *testing.T) {
	yml := validYAMLAllRequired + `openapi:
  uiPath: /trail
audit:
  destinations: [database]
  endpoint:
    rest:
      path: /trail
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "openapi.uiPath") {
		t.Errorf("expected an openapi.uiPath collision, got: %v", err)
	}
}

// ─── the ungated-audit guard ─────────────────────────────────────────────────

// A deliberately blank permission is honored while authorization is off (the
// gate would be inert anyway)...
func TestAuditEndpoint_BlankPermissionAllowedWhileAuthorizationIsOff(t *testing.T) {
	cfg := mustLoadAudit(t, "audit:\n  destinations: [database]\n  endpoint:\n    rest:\n      permission: \"\"\n")
	if cfg.Audit.Endpoint.REST.PermissionValue() != "" {
		t.Errorf("a deliberately blank permission must be preserved, got %q",
			cfg.Audit.Endpoint.REST.PermissionValue())
	}
}

// ...and refused once authorization is on, naming the yaml key rather than
// letting the route scan complain about code the maintainer does not own.
func TestAuditEndpoint_BlankPermissionRefusedUnderAuthorization(t *testing.T) {
	yml := validYAMLAllRequired + `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example
    audience: api
    jwksUrl: https://idp.example/jwks
  authorization:
    enabled: true
audit:
  destinations: [database]
  endpoint:
    rest:
      permission: ""
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil || !strings.Contains(err.Error(), "audit.endpoint.rest.permission") {
		t.Errorf("expected the ungated-audit guard to fire, got: %v", err)
	}
}

// ─── closed key sets ─────────────────────────────────────────────────────────

// A misspelled key that decodes to nothing is a silently wrong posture, so
// every level of the block names the offender at boot.
func TestAuditEndpoint_RejectsUnknownKeys(t *testing.T) {
	cases := []struct{ name, block, wantBlock string }{
		{
			name:      "audit",
			block:     "audit:\n  destinations: [database]\n  endpint:\n",
			wantBlock: "audit:",
		},
		{
			name:      "audit.endpoint",
			block:     "audit:\n  destinations: [database]\n  endpoint:\n    maxLimits: 5\n",
			wantBlock: "audit.endpoint:",
		},
		{
			name:      "audit.endpoint.rest",
			block:     "audit:\n  destinations: [database]\n  endpoint:\n    rest:\n      pathh: /trail\n",
			wantBlock: "audit.endpoint.rest:",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadAudit(t, c.block)
			if err == nil || !strings.Contains(err.Error(), "unknown key") {
				t.Fatalf("expected an unknown-key rejection, got: %v", err)
			}
			if !strings.Contains(err.Error(), strings.TrimSuffix(c.wantBlock, ":")) {
				t.Errorf("diagnostic must name the block %q, got: %v", c.wantBlock, err)
			}
		})
	}
}
