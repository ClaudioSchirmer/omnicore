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
	for _, field := range []string{"service", "relational.dialect", "relational.dsn", "mongo.uri", "mongo.database", "transport.endpoints", "transport.syncGroup"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("missing-field error must name %q, got %v", field, err)
		}
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
