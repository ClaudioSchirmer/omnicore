package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "microservice.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func mustInterpolate(t *testing.T, input string) string {
	t.Helper()
	got, err := interpolate(input)
	if err != nil {
		t.Fatalf("interpolate(%q) error: %v", input, err)
	}
	return got
}

func TestInterpolate_EnvWins(t *testing.T) {
	t.Setenv("FOO", "bar")
	if got := mustInterpolate(t, "${FOO:default}"); got != "bar" {
		t.Fatalf("want %q, got %q", "bar", got)
	}
}

func TestInterpolate_DefaultWhenUnset(t *testing.T) {
	os.Unsetenv("UNSET_FOO")
	if got := mustInterpolate(t, "${UNSET_FOO:default}"); got != "default" {
		t.Fatalf("want %q, got %q", "default", got)
	}
}

func TestInterpolate_EnvSetEmptyFallsBackToDefault(t *testing.T) {
	t.Setenv("EMPTY_FOO", "")
	if got := mustInterpolate(t, "${EMPTY_FOO:default}"); got != "default" {
		t.Fatalf("want %q, got %q", "default", got)
	}
}

func TestInterpolate_NoDefault_Empty(t *testing.T) {
	os.Unsetenv("FOO_NO_DEF")
	if got := mustInterpolate(t, "${FOO_NO_DEF}"); got != "" {
		t.Fatalf("want empty string, got %q", got)
	}
}

func TestInterpolate_DefaultWithColons(t *testing.T) {
	os.Unsetenv("DSN")
	want := "postgres://u:p@h/d"
	if got := mustInterpolate(t, "${DSN:postgres://u:p@h/d}"); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

const validYAMLAllRequired = `service: test
relational:
  dialect: postgres
  dsn: "${DB:postgres://localhost}"
  clock: app
mongo:
  uri: "${MURI:mongodb://localhost}"
  database: "views"
transport:
  endpoints: ["${KB:localhost:9092}"]
  syncGroup: "g1"
`

func TestLoadConfigFrom_HappyPath_AppliesDefaults(t *testing.T) {
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	path := writeTemp(t, validYAMLAllRequired)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Service != "test" {
		t.Errorf("Service = %q, want %q", cfg.Service, "test")
	}
	if cfg.HTTP.Addr != ":8080" {
		t.Errorf("HTTP.Addr default = %q, want %q", cfg.HTTP.Addr, ":8080")
	}
	if cfg.Relational.DSN != "postgres://localhost" {
		t.Errorf("Relational.DSN = %q", cfg.Relational.DSN)
	}
	if cfg.Mongo.URI != "mongodb://localhost" {
		t.Errorf("Mongo.URI = %q", cfg.Mongo.URI)
	}
	if cfg.Mongo.Database != "views" {
		t.Errorf("Mongo.Database = %q", cfg.Mongo.Database)
	}
	if len(cfg.Transport.Endpoints) != 1 || cfg.Transport.Endpoints[0] != "localhost:9092" {
		t.Errorf("Transport.Endpoints = %#v", cfg.Transport.Endpoints)
	}
	if cfg.Transport.SyncGroup != "g1" {
		t.Errorf("Transport.SyncGroup = %q", cfg.Transport.SyncGroup)
	}
	if cfg.Migrations.Dir != "./migrations" {
		t.Errorf("Migrations.Dir default = %q", cfg.Migrations.Dir)
	}
	// LoadConfigFrom is profile-agnostic: AutoRun stays the empty zero
	// value because the profile-aware default (dev=true / non-dev=check)
	// is resolved by LoadConfig only.
	if cfg.Migrations.AutoRun != "" {
		t.Errorf("Migrations.AutoRun after LoadConfigFrom = %q, want empty (profile-agnostic)", cfg.Migrations.AutoRun)
	}
	if cfg.Transport.SyncWorkers != runtime.NumCPU() {
		t.Errorf("Transport.SyncWorkers default = %d, want runtime.NumCPU()=%d", cfg.Transport.SyncWorkers, runtime.NumCPU())
	}
}

func TestLoadConfigFrom_KafkaSyncWorkers_ExplicitOverride(t *testing.T) {
	yaml := `service: test
relational: { dialect: postgres, dsn: "postgres://x", clock: app }
mongo: { uri: "mongodb://x", database: "v" }
transport:
  endpoints: ["k:1"]
  syncGroup: "g"
  syncWorkers: 2
`
	path := writeTemp(t, yaml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Transport.SyncWorkers != 2 {
		t.Errorf("Transport.SyncWorkers = %d, want 2 (explicit override)", cfg.Transport.SyncWorkers)
	}
}

func TestLoadConfigFrom_EnvOverridesDefault(t *testing.T) {
	t.Setenv("DB", "postgres://prod")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	path := writeTemp(t, validYAMLAllRequired)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Relational.DSN != "postgres://prod" {
		t.Errorf("Relational.DSN = %q, want %q", cfg.Relational.DSN, "postgres://prod")
	}
}

func TestLoadConfigFrom_AutoRunFalseExplicit(t *testing.T) {
	yml := validYAMLAllRequired + "migrations:\n  autoRun: false\n"
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunFalse {
		t.Errorf("Migrations.AutoRun explicit false not respected: %q", cfg.Migrations.AutoRun)
	}
}

func TestLoadConfigFrom_MissingRequired(t *testing.T) {
	path := writeTemp(t, "service: test\n")
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	// Only relational.* is unconditionally required now; mongo.* and transport.*
	// are optional (opt-out infrastructure — see the infra-free posture).
	for _, field := range []string{"relational.dialect", "relational.dsn"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error %q does not list missing field %q", err, field)
		}
	}
	for _, field := range []string{"mongo.uri", "transport.endpoints", "transport.syncGroup"} {
		if strings.Contains(err.Error(), field) {
			t.Errorf("error %q must not report optional field %q as missing", err, field)
		}
	}
}

func TestLoadConfigFrom_FileNotFound(t *testing.T) {
	_, err := LoadConfigFrom("/no/such/file.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "/no/such/file.yaml") {
		t.Errorf("error does not mention file path: %v", err)
	}
}

func TestLoadConfigFrom_InvalidYaml(t *testing.T) {
	path := writeTemp(t, "service: test\n\tbroken-tab-indent: 1\n")
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not mention file path: %v", err)
	}
}

func TestLoadConfig_UsesEnvPath(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired)
	t.Setenv(profileEnv, profileDev)
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Service != "test" {
		t.Errorf("Service = %q", cfg.Service)
	}
	if cfg.Profile != profileDev {
		t.Errorf("Profile = %q, want %q", cfg.Profile, profileDev)
	}
}

func TestLoadConfig_DefaultsToProfileFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "microservice.dev.yaml")
	if err := os.WriteFile(path, []byte(validYAMLAllRequired), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Setenv(profileEnv, profileDev)
	os.Unsetenv(configPathEnv)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Service != "test" {
		t.Errorf("Service = %q", cfg.Service)
	}
	if cfg.Profile != profileDev {
		t.Errorf("Profile = %q, want %q", cfg.Profile, profileDev)
	}
}

func TestLoadConfig_MissingProfile(t *testing.T) {
	os.Unsetenv(profileEnv)
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error when APP_PROFILE is unset")
	}
	if !strings.Contains(err.Error(), profileEnv) {
		t.Errorf("error %q does not mention %s", err, profileEnv)
	}
}

func TestLoadConfig_ArbitraryProfileAccepted(t *testing.T) {
	// "dev" and "prd" are the canonical pair, but any non-empty string is a
	// valid APP_PROFILE — QA suites and ops setups exercise auth variants by
	// shipping extra microservice.<variant>.yaml files (e.g., prd-pem,
	// prd-external) and swapping APP_PROFILE. The framework treats every
	// non-"dev" profile the same way; only "dev" unlocks auth.mode=disabled.
	yml := validYAMLAllRequired + `
auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: omnicore-users
    jwksUrl: https://idp.example.com/.well-known/jwks.json
`
	path := writeTemp(t, yml)
	t.Setenv(profileEnv, "prd-pem")
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig with arbitrary profile name: %v", err)
	}
	if cfg.Profile != "prd-pem" {
		t.Errorf("Profile = %q, want %q (free-form profile name should round-trip)", cfg.Profile, "prd-pem")
	}
}

func TestLoadConfig_DisabledRejectedInArbitraryProfile(t *testing.T) {
	// The disabled-mode guard applies to every profile that is not "dev",
	// including QA variants. validYAMLAllRequired has no auth: block, so
	// auth.mode defaults to "disabled".
	path := writeTemp(t, validYAMLAllRequired)
	t.Setenv(profileEnv, "prd-external")
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error: auth.mode=disabled must be rejected under any non-dev profile")
	}
	if !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "prd-external") {
		t.Errorf("error %q does not name both the mode and the profile", err)
	}
}

func TestLoadConfig_DisabledRejectedInPrd(t *testing.T) {
	// validYAMLAllRequired has no auth: block, so auth.mode defaults to "disabled".
	path := writeTemp(t, validYAMLAllRequired)
	t.Setenv(profileEnv, profilePrd)
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error: auth.mode=disabled must be rejected under prd")
	}
	if !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), profilePrd) {
		t.Errorf("error %q does not name both the mode and the profile", err)
	}
}

func TestLoadConfig_DisabledAllowedInDev(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired)
	t.Setenv(profileEnv, profileDev)
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Auth.Mode != AuthModeDisabled {
		t.Errorf("Auth.Mode default = %q, want %q", cfg.Auth.Mode, AuthModeDisabled)
	}
}

func TestLoadConfig_JWTAllowedInPrd(t *testing.T) {
	yml := validYAMLAllRequired + `
auth:
  mode: jwt
  jwt:
    issuer: https://idp.example.com
    audience: omnicore-users
    jwksUrl: https://idp.example.com/.well-known/jwks.json
`
	path := writeTemp(t, yml)
	t.Setenv(profileEnv, profilePrd)
	t.Setenv(configPathEnv, path)
	os.Unsetenv("DB")
	os.Unsetenv("MURI")
	os.Unsetenv("KB")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Auth.Mode != AuthModeJWT {
		t.Errorf("Auth.Mode = %q, want %q", cfg.Auth.Mode, AuthModeJWT)
	}
	if cfg.Profile != profilePrd {
		t.Errorf("Profile = %q, want %q", cfg.Profile, profilePrd)
	}
}

// --- file: prefix ---

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestInterpolate_FilePrefix_ReadsContents(t *testing.T) {
	path := writeFile(t, "secret", "hello world")
	got, err := interpolate("${file:" + path + "}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got != "hello world" {
		t.Fatalf("want %q, got %q", "hello world", got)
	}
}

func TestInterpolate_FilePrefix_TrimsTrailingLF(t *testing.T) {
	path := writeFile(t, "secret", "hello\n")
	got, err := interpolate("${file:" + path + "}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got != "hello" {
		t.Fatalf("want trailing LF trimmed; got %q", got)
	}
}

func TestInterpolate_FilePrefix_TrimsTrailingCRLF(t *testing.T) {
	path := writeFile(t, "secret", "hello\r\n")
	got, err := interpolate("${file:" + path + "}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got != "hello" {
		t.Fatalf("want trailing CRLF trimmed; got %q", got)
	}
}

func TestInterpolate_FilePrefix_PreservesPEM(t *testing.T) {
	pem := "-----BEGIN PUBLIC KEY-----\nABCDEFG\nHIJKLMN\n-----END PUBLIC KEY-----\n"
	path := writeFile(t, "key.pem", pem)
	got, err := interpolate("${file:" + path + "}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	// Only the trailing newline is trimmed; internal LFs survive verbatim.
	want := strings.TrimSuffix(pem, "\n")
	if got != want {
		t.Fatalf("PEM block round-trip failed\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestInterpolate_FilePrefix_NotFound_Errors(t *testing.T) {
	_, err := interpolate("${file:/no/such/file/at/all}")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "/no/such/file/at/all") {
		t.Errorf("error does not mention the path: %v", err)
	}
}

// --- vault: prefix ---

type stubResolver struct {
	value string
	err   error
	last  string
}

func (s *stubResolver) ResolveSecret(path string) (string, error) {
	s.last = path
	return s.value, s.err
}

func TestInterpolate_VaultPrefix_NoResolver_Errors(t *testing.T) {
	// Default resolver returns ErrUnsupportedResolver.
	RegisterSecretResolver(nil)
	defer RegisterSecretResolver(nil)
	_, err := interpolate("${vault:secret/db#dsn}")
	if err == nil {
		t.Fatal("expected error without a registered resolver")
	}
	if !errors.Is(err, ErrUnsupportedResolver) {
		t.Errorf("expected ErrUnsupportedResolver, got %v", err)
	}
}

func TestInterpolate_VaultPrefix_WithResolver(t *testing.T) {
	stub := &stubResolver{value: "s3cr3t"}
	RegisterSecretResolver(stub)
	defer RegisterSecretResolver(nil)

	got, err := interpolate("${vault:secret/db#dsn}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	if got != "s3cr3t" {
		t.Fatalf("want resolver value, got %q", got)
	}
	if stub.last != "secret/db#dsn" {
		t.Errorf("resolver received %q; want %q", stub.last, "secret/db#dsn")
	}
}

func TestInterpolate_VaultPrefix_ResolverError(t *testing.T) {
	custom := errors.New("vault unreachable")
	RegisterSecretResolver(&stubResolver{err: custom})
	defer RegisterSecretResolver(nil)

	_, err := interpolate("${vault:secret/x#y}")
	if err == nil {
		t.Fatal("expected error from resolver")
	}
	if !errors.Is(err, custom) {
		t.Errorf("expected wrapped resolver error, got %v", err)
	}
}

// --- mixed forms ---

func TestInterpolate_MixedFileAndEnv(t *testing.T) {
	path := writeFile(t, "secret", "abc")
	t.Setenv("USER_PART", "xyz")
	got, err := interpolate("file=${file:" + path + "} env=${USER_PART}")
	if err != nil {
		t.Fatalf("interpolate: %v", err)
	}
	want := "file=abc env=xyz"
	if got != want {
		t.Fatalf("mixed substitution failed\nwant %q\ngot  %q", want, got)
	}
}

// --- LoadConfigFrom integration ---

func TestLoadConfigFrom_FilePrefix_AppliesToValue(t *testing.T) {
	dsn := "postgres://user:pwd@host:5432/db?sslmode=disable"
	path := writeFile(t, "dsn", dsn+"\n") // trailing LF will be trimmed
	yml := `service: test
relational:
  dialect: postgres
  dsn: "${file:` + path + `}"
  clock: app
mongo:
  uri: "mongodb://localhost"
  database: "views"
transport:
  endpoints: ["localhost:9092"]
  syncGroup: "g"
`
	cfgPath := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Relational.DSN != dsn {
		t.Errorf("Relational.DSN = %q, want %q", cfg.Relational.DSN, dsn)
	}
}

func TestLoadConfigFrom_FilePrefix_NotFound_FailsBoot(t *testing.T) {
	yml := `service: test
relational:
  dialect: postgres
  dsn: "${file:/no/such/file}"
  clock: app
mongo:
  uri: "mongodb://localhost"
  database: "views"
transport:
  endpoints: ["localhost:9092"]
  syncGroup: "g"
`
	cfgPath := writeTemp(t, yml)
	_, err := LoadConfigFrom(cfgPath)
	if err == nil {
		t.Fatal("expected boot failure for missing file")
	}
	if !strings.Contains(err.Error(), "/no/such/file") {
		t.Errorf("error does not mention path: %v", err)
	}
}

// --- httpClient: block presence ---

func TestLoadConfigFrom_HttpClientAbsent_LeavesNil(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.HttpClient != nil {
		t.Errorf("HttpClient should be nil when block is absent, got %+v", cfg.HttpClient)
	}
}

func TestLoadConfigFrom_HttpClientPresent_Materializes(t *testing.T) {
	yml := validYAMLAllRequired + "httpClient: {}\n"
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.HttpClient == nil {
		t.Error("HttpClient should be non-nil when block is present (even empty)")
	}
}

func TestLoadConfigFrom_HttpClientWithChildren_ParsesOK(t *testing.T) {
	// Lookahead: arbitrary nested keys under httpClient: must parse without
	// error, even though the Config struct is currently empty. yaml.v3 ignores
	// unknown fields by default — Phase 1 introduces the schema.
	yml := validYAMLAllRequired + `httpClient:
  defaults:
    timeout: 30s
  services:
    keycloak:
      baseURL: https://kc.example.com
      endpoints:
        getUser:
          method: GET
          path: /users/{id}
`
	path := writeTemp(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.HttpClient == nil {
		t.Error("HttpClient should be non-nil when block is present with children")
	}
}

func TestLoadConfigFrom_VaultPrefix_FailsBoot_WithoutResolver(t *testing.T) {
	RegisterSecretResolver(nil)
	defer RegisterSecretResolver(nil)
	yml := `service: test
relational:
  dialect: postgres
  dsn: "${vault:secret/db#dsn}"
  clock: app
mongo:
  uri: "mongodb://localhost"
  database: "views"
transport:
  endpoints: ["localhost:9092"]
  syncGroup: "g"
`
	cfgPath := writeTemp(t, yml)
	_, err := LoadConfigFrom(cfgPath)
	if err == nil {
		t.Fatal("expected boot failure when no resolver is registered")
	}
	if !errors.Is(err, ErrUnsupportedResolver) {
		t.Errorf("expected ErrUnsupportedResolver in chain, got %v", err)
	}
}
