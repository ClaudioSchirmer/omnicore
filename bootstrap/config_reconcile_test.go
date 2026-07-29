package bootstrap

import (
	"strings"
	"testing"
)

// The mongo.reconcile block — the scheduled revision-parity sweep's knob.
// Off by default (the operator turns it on seeing the pass-duration trade),
// strict key allowlist like mongo.rebuild, interval defaulted to 60 minutes.

func loadReconcileYAML(t *testing.T, reconcileBlock string) (*Config, error) {
	t.Helper()
	yml := validRebuildYAMLBase + "mongo:\n" + mandatoryMongoMinimal + reconcileBlock
	t.Setenv(profileEnv, profileDev)
	t.Setenv(configPathEnv, writeTempRebuildYAML(t, yml))
	return LoadConfig()
}

func TestReconcileConfig_DefaultsOffWithHourlyInterval(t *testing.T) {
	cfg, err := loadReconcileYAML(t, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Mongo.Reconcile.Enabled {
		t.Error("the sweep must be OFF unless the operator turns it on")
	}
	if cfg.Mongo.Reconcile.IntervalMinutes != 60 {
		t.Errorf("IntervalMinutes default = %d, want 60", cfg.Mongo.Reconcile.IntervalMinutes)
	}
	if cfg.Mongo.Reconcile.RowsPerSecond != 0 {
		t.Errorf("RowsPerSecond must stay 0 (framework default applies downstream), got %d", cfg.Mongo.Reconcile.RowsPerSecond)
	}
}

func TestReconcileConfig_ExplicitValues(t *testing.T) {
	cfg, err := loadReconcileYAML(t, "  reconcile:\n    enabled: true\n    intervalMinutes: 15\n    rowsPerSecond: 20000\n    batchSize: 500\n")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	r := cfg.Mongo.Reconcile
	if !r.Enabled || r.IntervalMinutes != 15 || r.RowsPerSecond != 20000 || r.BatchSize != 500 {
		t.Errorf("explicit values not honored: %+v", r)
	}
}

// The mongo.parkedRetry block — the parked-events replay driver's knob.
// ON by default (pointer-bool so absent ≠ false), cadence defaulted to 1 min.

func TestParkedRetryConfig_DefaultsOnEveryMinute(t *testing.T) {
	cfg, err := loadReconcileYAML(t, "")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Mongo.ParkedRetry.Enabled == nil || !*cfg.Mongo.ParkedRetry.Enabled {
		t.Error("the replay driver must be ON unless the operator turns it off")
	}
	if cfg.Mongo.ParkedRetry.IntervalMinutes != 0 {
		t.Errorf("IntervalMinutes must stay 0 (framework default 1min applies downstream), got %d", cfg.Mongo.ParkedRetry.IntervalMinutes)
	}
}

func TestParkedRetryConfig_ExplicitOffAndCadence(t *testing.T) {
	cfg, err := loadReconcileYAML(t, "  parkedRetry:\n    enabled: false\n    intervalMinutes: 10\n")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if *cfg.Mongo.ParkedRetry.Enabled {
		t.Error("explicit enabled: false must be honored — absent and false are different values")
	}
	if cfg.Mongo.ParkedRetry.IntervalMinutes != 10 {
		t.Errorf("IntervalMinutes = %d, want 10", cfg.Mongo.ParkedRetry.IntervalMinutes)
	}
}

func TestParkedRetryConfig_UnknownKeyAborts(t *testing.T) {
	_, err := loadReconcileYAML(t, "  parkedRetry:\n    intervalSeconds: 30\n")
	if err == nil {
		t.Fatal("an unknown mongo.parkedRetry key must abort the boot")
	}
	if !strings.Contains(err.Error(), "intervalSeconds") {
		t.Errorf("the abort must name the offending key, got: %v", err)
	}
}

func TestReconcileConfig_UnknownKeyAborts(t *testing.T) {
	_, err := loadReconcileYAML(t, "  reconcile:\n    enabled: true\n    deleteOrphans: true\n")
	if err == nil {
		t.Fatal("an unknown mongo.reconcile key must abort the boot, not be ignored")
	}
	if !strings.Contains(err.Error(), "deleteOrphans") {
		t.Errorf("the abort must name the offending key, got: %v", err)
	}
}
