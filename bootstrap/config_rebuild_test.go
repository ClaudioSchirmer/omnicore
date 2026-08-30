package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper — writes a temp yaml and returns the path. Local to this file to
// keep the test self-contained without colliding with writeTemp in
// load_test.go (same package).
func writeTempRebuildYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "microservice.test.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp yaml: %v", err)
	}
	return p
}

// validRebuildYAMLBase carries only the mandatory non-mongo / non-auth
// blocks so callers can append a `mongo:` block (with optional `rebuild:`
// sub-block) and an `auth:` block without YAML key collisions.
const validRebuildYAMLBase = `service: test
relational: { dialect: postgres, dsn: "postgres://x", clock: app }
transport:
  endpoints: ["k:1"]
  syncGroup: "g"
`

// mandatoryMongoMinimal is the URI/database pair every test needs in its
// mongo block. Tests appending a rebuild sub-block include this prefix.
const mandatoryMongoMinimal = `  uri: "mongodb://x"
  database: "v"
`

const prdAuthBlock = `auth:
  mode: jwt
  jwt:
    issuer: https://idp.example
    audience: svc
    publicKeyPem: |
      -----BEGIN PUBLIC KEY-----
      MFwwDQYJKoZIhvcNAQEBBQADSwAwSAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu
      KUpRKfFLfRYC9AIKjbJTWit+CqvjWYzvQwECAwEAAQ==
      -----END PUBLIC KEY-----
`

// applyProfileDefaults — profile-aware resolution of Migrations.AutoRun
// and Mongo.Rebuild.AutoRun under the AutoRunMode enum (check | true | false).

func TestApplyProfileDefaults_DevAutoRunTrue(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal
	t.Setenv(profileEnv, profileDev)
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunTrue {
		t.Errorf("Migrations.AutoRun in dev = %q, want %q", cfg.Migrations.AutoRun, AutoRunTrue)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunTrue {
		t.Errorf("Mongo.Rebuild.AutoRun in dev = %q, want %q", cfg.Mongo.Rebuild.AutoRun, AutoRunTrue)
	}
}

func TestApplyProfileDefaults_PrdAutoRunCheck(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal + prdAuthBlock
	t.Setenv(profileEnv, profilePrd)
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunCheck {
		t.Errorf("Migrations.AutoRun in prd = %q, want %q", cfg.Migrations.AutoRun, AutoRunCheck)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunCheck {
		t.Errorf("Mongo.Rebuild.AutoRun in prd = %q, want %q", cfg.Mongo.Rebuild.AutoRun, AutoRunCheck)
	}
}

func TestApplyProfileDefaults_QAVariantAutoRunCheck(t *testing.T) {
	// Any non-dev profile resolves AutoRun to check — covers prd, qa-canary,
	// prd-pem, etc. without listing them.
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal + prdAuthBlock
	t.Setenv(profileEnv, "qa-canary")
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunCheck {
		t.Errorf("Migrations.AutoRun in qa-canary = %q, want %q", cfg.Migrations.AutoRun, AutoRunCheck)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunCheck {
		t.Errorf("Mongo.Rebuild.AutoRun in qa-canary = %q, want %q", cfg.Mongo.Rebuild.AutoRun, AutoRunCheck)
	}
}

func TestApplyProfileDefaults_ExplicitYAMLOverridesProfile(t *testing.T) {
	// Explicit autoRun: true in yaml wins even under non-dev profile.
	yml := validRebuildYAMLBase +
		"migrations:\n  autoRun: true\n" +
		"mongo:\n" + mandatoryMongoMinimal + "  rebuild:\n    autoRun: true\n" +
		prdAuthBlock
	t.Setenv(profileEnv, profilePrd)
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunTrue {
		t.Errorf("Explicit autoRun=true overridden by profile: got %q", cfg.Migrations.AutoRun)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunTrue {
		t.Errorf("Explicit mongo.rebuild.autoRun=true overridden by profile: got %q", cfg.Mongo.Rebuild.AutoRun)
	}
}

func TestApplyProfileDefaults_ExplicitFalseHonoredInDev(t *testing.T) {
	// Symmetric: dev profile but explicit autoRun=false should be honored.
	yml := validRebuildYAMLBase +
		"migrations:\n  autoRun: false\n" +
		"mongo:\n" + mandatoryMongoMinimal + "  rebuild:\n    autoRun: false\n"
	t.Setenv(profileEnv, profileDev)
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunFalse {
		t.Errorf("Explicit autoRun=false overridden in dev: got %q", cfg.Migrations.AutoRun)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunFalse {
		t.Errorf("Explicit mongo.rebuild.autoRun=false overridden in dev: got %q", cfg.Mongo.Rebuild.AutoRun)
	}
}

func TestApplyProfileDefaults_ExplicitCheckString(t *testing.T) {
	// "check" mode is the new third value the enum carries.
	yml := validRebuildYAMLBase +
		"migrations:\n  autoRun: check\n" +
		"mongo:\n" + mandatoryMongoMinimal + "  rebuild:\n    autoRun: check\n"
	t.Setenv(profileEnv, profileDev)
	path := writeTempRebuildYAML(t, yml)
	t.Setenv(configPathEnv, path)
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Migrations.AutoRun != AutoRunCheck {
		t.Errorf("Migrations.AutoRun = %q, want %q", cfg.Migrations.AutoRun, AutoRunCheck)
	}
	if cfg.Mongo.Rebuild.AutoRun != AutoRunCheck {
		t.Errorf("Mongo.Rebuild.AutoRun = %q, want %q", cfg.Mongo.Rebuild.AutoRun, AutoRunCheck)
	}
}

func TestAutoRunMode_InvalidValueRejected(t *testing.T) {
	yml := validRebuildYAMLBase + "migrations:\n  autoRun: maybe\n" + "mongo:\n" + mandatoryMongoMinimal
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for invalid autoRun value, got nil")
	}
	if !strings.Contains(err.Error(), "autoRun") || !strings.Contains(err.Error(), "maybe") {
		t.Errorf("error should mention autoRun + offending value: %v", err)
	}
}

// MongoRebuildConfig — orphan + allowDowngrade defaults and validation.

func TestMongoRebuildConfig_DefaultsAppliedByLoadConfigFrom(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal
	path := writeTempRebuildYAML(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Mongo.Rebuild.Orphan != MongoRebuildOrphanDelete {
		t.Errorf("Orphan default = %q, want %q", cfg.Mongo.Rebuild.Orphan, MongoRebuildOrphanDelete)
	}
	if cfg.Mongo.Rebuild.AllowDowngrade {
		t.Error("AllowDowngrade default should be false")
	}
}

func TestMongoRebuildConfig_OrphanExplicitWarn(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    orphan: warn\n"
	path := writeTempRebuildYAML(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Mongo.Rebuild.Orphan != MongoRebuildOrphanWarn {
		t.Errorf("Orphan = %q, want %q", cfg.Mongo.Rebuild.Orphan, MongoRebuildOrphanWarn)
	}
}

func TestMongoRebuildConfig_OrphanInvalidValueRejected(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    orphan: skip\n"
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for invalid orphan value, got nil")
	}
	if !strings.Contains(err.Error(), "orphan") {
		t.Errorf("error should mention orphan: %v", err)
	}
}

func TestMongoRebuildConfig_AllowDowngradeExplicitTrue(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    allowDowngrade: true\n"
	path := writeTempRebuildYAML(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if !cfg.Mongo.Rebuild.AllowDowngrade {
		t.Error("AllowDowngrade = false, want true (explicit yaml)")
	}
}

func TestMongoRebuildConfig_WorkersAndBatchSizeDefaultZero(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal
	path := writeTempRebuildYAML(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Mongo.Rebuild.Workers != 0 {
		t.Errorf("Workers default = %d, want 0 (framework default)", cfg.Mongo.Rebuild.Workers)
	}
	if cfg.Mongo.Rebuild.BatchSize != 0 {
		t.Errorf("BatchSize default = %d, want 0 (framework default)", cfg.Mongo.Rebuild.BatchSize)
	}
}

func TestMongoRebuildConfig_WorkersAndBatchSizeExplicit(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    workers: 8\n    batchSize: 5000\n"
	path := writeTempRebuildYAML(t, yml)
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Mongo.Rebuild.Workers != 8 {
		t.Errorf("Workers = %d, want 8", cfg.Mongo.Rebuild.Workers)
	}
	if cfg.Mongo.Rebuild.BatchSize != 5000 {
		t.Errorf("BatchSize = %d, want 5000", cfg.Mongo.Rebuild.BatchSize)
	}
}

func TestMongoRebuildConfig_NegativeWorkersRejected(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    workers: -1\n"
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for negative workers, got nil")
	}
	if !strings.Contains(err.Error(), "workers") {
		t.Errorf("error should mention workers: %v", err)
	}
}

func TestMongoRebuildConfig_NegativeBatchSizeRejected(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    batchSize: -1\n"
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for negative batchSize, got nil")
	}
	if !strings.Contains(err.Error(), "batchSize") {
		t.Errorf("error should mention batchSize: %v", err)
	}
}

// Strict yaml decoding on mongo.rebuild — unknown keys (notably the
// removed lockTTL) abort boot.

func TestMongoRebuildConfig_StrictDecoding_RejectsLockTTLRemoved(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    lockTTL: 1h\n"
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for removed lockTTL field, got nil")
	}
	if !strings.Contains(err.Error(), "lockTTL") {
		t.Errorf("error should mention lockTTL: %v", err)
	}
}

func TestMongoRebuildConfig_StrictDecoding_RejectsTypo(t *testing.T) {
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal +
		"  rebuild:\n    ophan: delete\n"
	path := writeTempRebuildYAML(t, yml)
	_, err := LoadConfigFrom(path)
	if err == nil {
		t.Fatal("LoadConfigFrom: want error for typo in field name, got nil")
	}
	if !strings.Contains(err.Error(), "ophan") {
		t.Errorf("error should mention offending field name: %v", err)
	}
}

// AutoRunMode helpers — Is* discriminators.

func TestAutoRunMode_Discriminators(t *testing.T) {
	if !AutoRunTrue.IsTrue() || AutoRunCheck.IsTrue() || AutoRunFalse.IsTrue() {
		t.Error("IsTrue() should match only AutoRunTrue")
	}
	if !AutoRunCheck.IsCheck() || AutoRunTrue.IsCheck() || AutoRunFalse.IsCheck() {
		t.Error("IsCheck() should match only AutoRunCheck")
	}
	if !AutoRunFalse.IsFalse() || AutoRunTrue.IsFalse() || AutoRunCheck.IsFalse() {
		t.Error("IsFalse() should match only AutoRunFalse")
	}
}
