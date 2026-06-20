package bootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/infra/integration"
)

// --- config.go: AutoRunMode.UnmarshalYAML edge branches ---------------------

func TestAutoRunMode_UnmarshalNonScalarRejected(t *testing.T) {
	var m AutoRunMode
	// A sequence node is not a scalar → the non-scalar guard fires.
	if err := yaml.Unmarshal([]byte("[a, b]"), &m); err == nil {
		t.Fatal("non-scalar autoRun node must be rejected")
	}
}

func TestAutoRunMode_UnmarshalEmptyLeavesZero(t *testing.T) {
	var m AutoRunMode
	if err := yaml.Unmarshal([]byte(`""`), &m); err != nil {
		t.Fatalf("empty scalar must decode to zero value, got %v", err)
	}
	if m != "" {
		t.Fatalf("empty autoRun must stay zero, got %q", m)
	}
}

// --- config.go: MongoRebuildConfig.UnmarshalYAML + validate -----------------

func TestMongoRebuildConfig_UnmarshalNonMappingRejected(t *testing.T) {
	var m MongoRebuildConfig
	if err := yaml.Unmarshal([]byte("just-a-scalar"), &m); err == nil {
		t.Fatal("non-mapping mongo.rebuild node must be rejected")
	}
}

func TestMongoRebuildConfig_UnmarshalDecodeError(t *testing.T) {
	var m MongoRebuildConfig
	// allowDowngrade is an allowed key but expects a bool — a string value
	// drives the inner value.Decode error branch (past the allowlist check).
	if err := yaml.Unmarshal([]byte("allowDowngrade: notabool"), &m); err == nil {
		t.Fatal("bad allowDowngrade type must surface a decode error")
	}
}

func TestMongoRebuildConfig_ValidateInvalidAutoRunDirect(t *testing.T) {
	// Direct call: an out-of-set AutoRun can only reach validate() when not
	// pre-rejected by UnmarshalYAML (e.g. set programmatically).
	m := MongoRebuildConfig{AutoRun: "bogus", Orphan: MongoRebuildOrphanDelete}
	if err := m.validate(); err == nil || !strings.Contains(err.Error(), "autoRun") {
		t.Fatalf("invalid autoRun must fail validate, got %v", err)
	}
}

// --- config.go: applyDefaults + Validate Integration branches ---------------

func TestConfig_ApplyDefaults_InvokesIntegrationDefaults(t *testing.T) {
	c := validBaseConfig()
	c.Integration = &integration.Config{
		Subscribes: map[string]integration.SubscribeEntry{
			"partners": {Topic: "partners.events"},
		},
	}
	c.applyDefaults()
	if c.Integration.Defaults.ConsumerGroup == "" {
		t.Fatal("applyDefaults must thread into Integration.ApplyDefaults")
	}
}

func TestConfig_Validate_IntegrationErrorPropagates(t *testing.T) {
	c := validBaseConfig()
	c.Integration = &integration.Config{
		Publishes: integration.PublishConfig{
			Events: map[string]integration.PublishEvent{
				"bad": {EventType: ""}, // missing eventType → Integration.Validate errors
			},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "eventType") {
		t.Fatalf("integration validation error must propagate, got %v", err)
	}
}

func TestConfig_Validate_OpenAPIErrorPropagates(t *testing.T) {
	c := validBaseConfig()
	c.OpenAPI.UIPath = "no-leading-slash"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "uiPath") {
		t.Fatalf("openapi validation error must propagate, got %v", err)
	}
}

// --- auth_config.go: UnmarshalYAML decode errors + reject non-mapping -------

func TestAuthorizationConfig_UnmarshalDecodeError(t *testing.T) {
	var a AuthorizationConfig
	if err := yaml.Unmarshal([]byte("enabled: notabool"), &a); err == nil {
		t.Fatal("bad enabled type must surface a decode error")
	}
}

func TestTenantConfig_UnmarshalDecodeError(t *testing.T) {
	var tc TenantConfig
	if err := yaml.Unmarshal([]byte("enabled: notabool"), &tc); err == nil {
		t.Fatal("bad tenant.enabled type must surface a decode error")
	}
}

func TestRejectUnknownYAMLFields_NonMappingIsNoError(t *testing.T) {
	node := &yaml.Node{Kind: yaml.ScalarNode, Value: "scalar"}
	if err := rejectUnknownYAMLFields(node, "auth.x", "a", "b"); err != nil {
		t.Fatalf("non-mapping node must be a no-op, got %v", err)
	}
}

// --- auth_config.go: applyDefaults + validate Authorization branches --------

func TestAuthConfig_ApplyDefaults_InvokesAuthorizationDefaults(t *testing.T) {
	a := &AuthConfig{
		Mode:          AuthModeJWT,
		JWT:           &JWTConfig{Issuer: "i", Audience: "aud", JWKSURL: "https://idp/jwks"},
		Authorization: &AuthorizationConfig{},
	}
	a.applyDefaults()
	if a.Authorization.PermissionsClaim != "permissions" {
		t.Fatalf("applyDefaults must thread into Authorization.applyDefaults, got %q",
			a.Authorization.PermissionsClaim)
	}
}

func TestAuthConfig_Validate_JWTWithAuthorization(t *testing.T) {
	a := &AuthConfig{
		Mode:          AuthModeJWT,
		JWT:           &JWTConfig{Issuer: "i", Audience: "aud", JWKSURL: "https://idp/jwks"},
		Authorization: &AuthorizationConfig{Enabled: true},
	}
	if err := a.validate(); err != nil {
		t.Fatalf("valid jwt + authorization must pass, got %v", err)
	}
}

func TestAuthConfig_Validate_JWTAuthorizationTenantRuleFails(t *testing.T) {
	a := &AuthConfig{
		Mode: AuthModeJWT,
		JWT:  &JWTConfig{Issuer: "i", Audience: "aud", JWKSURL: "https://idp/jwks"},
		Authorization: &AuthorizationConfig{
			Tenant: TenantConfig{Required: true, Enabled: false}, // incoherent
		},
	}
	if err := a.validate(); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Fatalf("required-without-enabled tenant must fail, got %v", err)
	}
}

// --- cache_config.go: remaining resolve + validate branches -----------------

func TestValidateCache_RedisSubBlockValidationError(t *testing.T) {
	// store=redis with a structurally invalid Redis sub-block (empty Addr)
	// drives the cfg.Redis.Validate() error branch.
	errs := validateCache(&CacheConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: ""}})
	if !containsSubstr(errs, "addr") {
		t.Fatalf("expected redis sub-validation error, got %v", errs)
	}
}

func TestValidateCacheShared_RedisSubBlockValidationError(t *testing.T) {
	errs := validateCacheShared(&CacheSharedConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: ""}})
	if !containsSubstr(errs, "addr") {
		t.Fatalf("expected shared redis sub-validation error, got %v", errs)
	}
}

func TestResolveCache_EmptyStoreFallsBackToMemory(t *testing.T) {
	c, err := resolveCache(&CacheConfig{Store: ""}, nil)
	if err != nil || c == nil {
		t.Fatalf("empty store must fall back to memory, got (%v, %v)", c, err)
	}
}

func TestResolveCache_RedisNoInjectionBuilds(t *testing.T) {
	// NewRedis is lazy (no dial at construction), so a valid Addr yields a
	// cache without contacting Redis.
	c, err := resolveCache(&CacheConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: "localhost:6379"}}, nil)
	if err != nil || c == nil {
		t.Fatalf("redis store must build a cache, got (%v, %v)", c, err)
	}
}

func TestResolveCache_UnknownStoreDefensiveError(t *testing.T) {
	_, err := resolveCache(&CacheConfig{Store: "etcd"}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a valid backend") {
		t.Fatalf("unknown store must hit the defensive default, got %v", err)
	}
}

func TestResolveSharedCache_RedisNoInjectionBuilds(t *testing.T) {
	cfg := &CacheConfig{Store: "memory", Shared: &CacheSharedConfig{
		Store: "redis", Redis: &cache.RedisConfig{Addr: "localhost:6379"},
	}}
	c, err := resolveSharedCache(cfg, nil)
	if err != nil || c == nil {
		t.Fatalf("shared redis store must build a cache, got (%v, %v)", c, err)
	}
}

// --- load.go: interpolate short-circuit after first error -------------------

func TestInterpolate_SecondMatchShortCircuitsAfterError(t *testing.T) {
	// Two ${file:...} forms: the first read fails, setting firstErr; the
	// closure for the second match takes the early `if firstErr != nil`
	// return before attempting a second read.
	_, err := interpolate("${file:/no/such/path/aaa}${file:/no/such/path/bbb}")
	if err == nil {
		t.Fatal("a failing file interpolation must surface an error")
	}
}

// --- upstream_subscription.go: UnmarshalYAML + validateShape branches -------

func TestUpstreamSubscription_UnmarshalNonMappingRejected(t *testing.T) {
	var s UpstreamSubscription
	if err := yaml.Unmarshal([]byte("scalar-not-a-map"), &s); err == nil {
		t.Fatal("non-mapping upstreamSubscription must be rejected")
	}
}

func TestUpstreamSubscription_UnmarshalDecodeError(t *testing.T) {
	var s UpstreamSubscription
	// workers is an int field; a non-numeric value drives the inner decode
	// error branch past the allowlist check.
	if err := yaml.Unmarshal([]byte("workers: notanint"), &s); err == nil {
		t.Fatal("bad workers type must surface a decode error")
	}
}

func TestUpstreamSubscription_ValidateShape_OffsetNonDevWithoutAck(t *testing.T) {
	s := UpstreamSubscription{
		Topic:            "users.events",
		Collection:       "users",
		OnUpstreamDelete: UpstreamDeleteCascade,
		StartFrom:        "offset:42",
		// AcknowledgeOffsetReset left false under a non-dev profile → rejected.
	}
	if err := s.validateShape("prd"); err == nil || !strings.Contains(err.Error(), "acknowledgeOffsetReset") {
		t.Fatalf("offset:N under prd without ack must fail, got %v", err)
	}
}

func TestUpstreamSubscription_ValidateShape_NegativeWorkersRejected(t *testing.T) {
	s := UpstreamSubscription{
		Topic:            "users.events",
		Collection:       "users",
		OnUpstreamDelete: UpstreamDeleteCascade,
		StartFrom:        StartFromLatest,
		Workers:          -1,
	}
	if err := s.validateShape("dev"); err == nil || !strings.Contains(err.Error(), "workers") {
		t.Fatalf("negative workers must fail validateShape, got %v", err)
	}
}

// --- upstream_guards.go: resolveUpstreamSubscriptions empty-wiring path ------

func TestResolveUpstreamSubscriptions_CfgOnlyEmptyWiring(t *testing.T) {
	cfg := &Config{UpstreamSubscriptions: []UpstreamSubscription{{Topic: "t", Collection: "c"}}}
	out, err := resolveUpstreamSubscriptions(cfg, Wiring{})
	if err != nil {
		t.Fatalf("cfg-only resolve must succeed, got %v", err)
	}
	if len(out) != 1 || out[0].Topic != "t" {
		t.Fatalf("cfg subscriptions must pass through, got %+v", out)
	}
}
