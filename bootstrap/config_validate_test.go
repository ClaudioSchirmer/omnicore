package bootstrap

import (
	"strings"
	"testing"
)

// validBaseConfig returns a Config carrying every mandatory field plus the
// profile-agnostic defaults, so Config.Validate() passes. Individual tests
// mutate one knob to drive a specific validation branch.
func validBaseConfig() *Config {
	c := &Config{Service: "svc"}
	c.Relational.Dialect = "postgres"
	c.Relational.DSN = "postgres://localhost/db"
	c.Relational.Clock = "app"
	c.Mongo.URI = "mongodb://localhost:27017"
	c.Mongo.Database = "views"
	c.Transport.Endpoints = []string{"localhost:9092"}
	c.Transport.SyncGroup = "svc-sync"
	c.applyDefaults()
	return c
}

func TestQueryConfig_Validate(t *testing.T) {
	if err := (&QueryConfig{MaxLimit: 100, MaxExportRows: 5000}).validate(); err != nil {
		t.Errorf("valid query config rejected: %v", err)
	}
	if err := (&QueryConfig{MaxLimit: -1}).validate(); err == nil || !strings.Contains(err.Error(), "maxLimit") {
		t.Errorf("expected maxLimit error, got %v", err)
	}
	if err := (&QueryConfig{MaxExportRows: -1}).validate(); err == nil || !strings.Contains(err.Error(), "maxExportRows") {
		t.Errorf("expected maxExportRows error, got %v", err)
	}
}

func TestConfig_Validate_PassesOnCompleteConfig(t *testing.T) {
	if err := validBaseConfig().Validate(); err != nil {
		t.Fatalf("complete config must validate, got %v", err)
	}
}

func TestConfig_Validate_ReportsAllMissingRequired(t *testing.T) {
	err := (&Config{}).Validate()
	if err == nil {
		t.Fatal("empty config must fail validation")
	}
	// Only service + relational.* are unconditionally required. mongo.* and
	// transport.* are OPTIONAL (each infrastructure is opt-out by its own config
	// block — the infra-free posture); a service that needs them but omits them is
	// caught by a coherence guard at boot, not by this base validation.
	for _, field := range []string{"service", "relational.dialect", "relational.dsn"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing-field error must name %q, got %v", field, err)
		}
	}
	for _, field := range []string{"mongo.uri", "transport.endpoints", "transport.syncGroup"} {
		if strings.Contains(err.Error(), field) {
			t.Errorf("optional field %q must NOT be reported missing, got %v", field, err)
		}
	}
}

// mongo.database is the one CONDITIONAL requirement: mandatory when mongo.uri is
// set (a uri with no database is a real mistake), absent otherwise.
func TestConfig_Validate_MongoDatabaseRequiredOnlyWithURI(t *testing.T) {
	c := validBaseConfig()
	c.Mongo.URI = "mongodb://localhost"
	c.Mongo.Database = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mongo.database") {
		t.Errorf("mongo.database must be required when mongo.uri is set, got %v", err)
	}

	c2 := validBaseConfig()
	c2.Mongo.URI = ""
	c2.Mongo.Database = ""
	if err := c2.Validate(); err != nil {
		t.Errorf("mongo.database must be optional when mongo.uri is empty, got %v", err)
	}
}

func TestConfig_Validate_NegativeShutdownDrainRejected(t *testing.T) {
	c := validBaseConfig()
	c.Shutdown.DrainTimeoutSeconds = -1
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "drainTimeoutSeconds") {
		t.Errorf("expected drainTimeoutSeconds error, got %v", err)
	}
}

func TestConfig_Validate_CacheErrorPropagates(t *testing.T) {
	c := validBaseConfig()
	c.Cache = &CacheConfig{Store: "memcached"} // invalid backend
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "not a valid backend") {
		t.Errorf("expected cache validation error to propagate, got %v", err)
	}
}
