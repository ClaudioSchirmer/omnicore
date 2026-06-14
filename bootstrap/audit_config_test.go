package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/audit"
)

func TestAuditConfig_DefaultsToBothWhenAbsent(t *testing.T) {
	path := writeTemp(t, validYAMLAllRequired) // no `audit:` block
	cfg, err := LoadConfigFrom(path)
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	got := cfg.Audit.Destinations
	if len(got) != 2 {
		t.Fatalf("Destinations default len = %d, want 2", len(got))
	}
	if got[0] != audit.DestinationSlog || got[1] != audit.DestinationDatabase {
		t.Errorf("Destinations default = %#v, want [%q, %q]",
			got, audit.DestinationSlog, audit.DestinationDatabase)
	}
}

func TestAuditConfig_DefaultsAppliedWhenBlockAbsent(t *testing.T) {
	// A bare `audit:` block (no children) parses as a zero-value AuditConfig
	// with Destinations == nil — same as if the block were missing entirely.
	// applyDefaults must populate both destinations.
	yml := validYAMLAllRequired + "audit:\n"
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if len(cfg.Audit.Destinations) != 2 {
		t.Errorf("Destinations = %#v, want [slog, database]", cfg.Audit.Destinations)
	}
}

func TestAuditConfig_EmptySlicePreservedAsOff(t *testing.T) {
	// `destinations: []` is the explicit "audit off" shape — applyDefaults
	// must NOT overwrite it with the default. Without this distinction the
	// operator cannot turn audit off without a separate `disabled` enum.
	yml := validYAMLAllRequired + `audit:
  destinations: []
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Audit.Destinations == nil {
		t.Fatal("Destinations: nil after explicit empty slice — applyDefaults overwrote operator intent")
	}
	if len(cfg.Audit.Destinations) != 0 {
		t.Errorf("Destinations = %#v, want empty (off)", cfg.Audit.Destinations)
	}
}

func TestAuditConfig_SlogOnly(t *testing.T) {
	yml := validYAMLAllRequired + `audit:
  destinations:
    - slog
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if !cfg.Audit.Includes(audit.DestinationSlog) {
		t.Errorf("Includes(slog) = false, want true")
	}
	if cfg.Audit.Includes(audit.DestinationDatabase) {
		t.Errorf("Includes(database) = true, want false")
	}
}

func TestAuditConfig_DatabaseOnly(t *testing.T) {
	yml := validYAMLAllRequired + `audit:
  destinations:
    - database
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if cfg.Audit.Includes(audit.DestinationSlog) {
		t.Errorf("Includes(slog) = true, want false")
	}
	if !cfg.Audit.Includes(audit.DestinationDatabase) {
		t.Errorf("Includes(database) = false, want true")
	}
}

func TestAuditConfig_BothDestinationsExplicit(t *testing.T) {
	yml := validYAMLAllRequired + `audit:
  destinations:
    - slog
    - database
`
	cfg, err := LoadConfigFrom(writeTemp(t, yml))
	if err != nil {
		t.Fatalf("LoadConfigFrom: %v", err)
	}
	if !cfg.Audit.Includes(audit.DestinationSlog) || !cfg.Audit.Includes(audit.DestinationDatabase) {
		t.Errorf("Includes both = false (Destinations=%#v)", cfg.Audit.Destinations)
	}
}

func TestAuditConfig_RejectsUnknownDestination(t *testing.T) {
	yml := validYAMLAllRequired + `audit:
  destinations:
    - slog
    - kinesis
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil {
		t.Fatal("expected error citing unknown destination kinesis, got nil")
	}
	if !strings.Contains(err.Error(), "kinesis") {
		t.Errorf("error %q does not name the offending token", err)
	}
}

func TestAuditConfig_RejectsDuplicateDestination(t *testing.T) {
	yml := validYAMLAllRequired + `audit:
  destinations:
    - slog
    - database
    - slog
`
	_, err := LoadConfigFrom(writeTemp(t, yml))
	if err == nil {
		t.Fatal("expected error citing duplicate slog, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "slog") {
		t.Errorf("error %q does not name the duplicate or the token", err)
	}
}

func TestAuditConfig_IncludesNilSafe(t *testing.T) {
	// Includes on a zero-value (nil Destinations) returns false for every
	// destination — caller code can pre-applyDefaults call this without
	// guarding nil first.
	cfg := audit.Config{}
	if cfg.Includes(audit.DestinationSlog) {
		t.Errorf("zero-value Includes(slog) = true, want false")
	}
	if cfg.Includes(audit.DestinationDatabase) {
		t.Errorf("zero-value Includes(database) = true, want false")
	}
}
