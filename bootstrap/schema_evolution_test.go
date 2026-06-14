package bootstrap

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// The pure decision functions in schema_evolution.go are covered here
// (diagnostic formatters for the migration paths). The Mongo and
// Postgres round-trips (applyMigrations strict path, reconcileViewDrift
// dispatch over the 8-case matrix) are exercised by the
// omnicore-example-users E2E suite once qa/schema_evolution.sh lands.

// ─── formatMigrationPendingDiagnostic ────────────────────────────────────────

func TestFormatMigrationPendingDiagnostic_NamesEveryPendingVersion(t *testing.T) {
	out := formatMigrationPendingDiagnostic(2, []uint{3, 4, 5})
	for _, want := range []string{"version 3", "version 4", "version 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("diagnostic missing %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "current DB version: 2") {
		t.Errorf("diagnostic should name current version 2:\n%s", out)
	}
	if !strings.Contains(out, "required: 5") {
		t.Errorf("diagnostic should name highest required version (5):\n%s", out)
	}
	if !strings.Contains(out, "migrations.autoRun") {
		t.Errorf("diagnostic should instruct on autoRun flip:\n%s", out)
	}
}

func TestFormatMigrationPendingDiagnostic_OffersManualSQL(t *testing.T) {
	// §14.1 — the diagnostic must include the INSERT INTO omnicore_migrations
	// SQL so an operator can mark migrations as applied after running
	// them manually.
	out := formatMigrationPendingDiagnostic(2, []uint{3})
	if !strings.Contains(out, "INSERT INTO omnicore_migrations") {
		t.Errorf("diagnostic should include manual reconcile SQL:\n%s", out)
	}
	if !strings.Contains(out, "(3, false)") {
		t.Errorf("diagnostic SQL should name the pending version:\n%s", out)
	}
}

func TestFormatMigrationPendingDiagnostic_SinglePending(t *testing.T) {
	out := formatMigrationPendingDiagnostic(0, []uint{2})
	if !strings.Contains(out, "version 2") {
		t.Errorf("diagnostic should name version 2:\n%s", out)
	}
	if !strings.Contains(out, "current DB version: 0") {
		t.Errorf("diagnostic should name current version 0 (fresh DB):\n%s", out)
	}
}

// ─── formatMigrationDirtyDiagnostic ──────────────────────────────────────────

func TestFormatMigrationDirtyDiagnostic_NamesVersionAndForceCall(t *testing.T) {
	out := formatMigrationDirtyDiagnostic(3)
	if !strings.Contains(out, "previous migration 3") {
		t.Errorf("diagnostic should name version 3:\n%s", out)
	}
	if !strings.Contains(out, "Force(ctx, 3)") {
		t.Errorf("diagnostic should suggest Force(ctx, 3) as recovery:\n%s", out)
	}
	if !strings.Contains(out, "dirty state") {
		t.Errorf("diagnostic should mention 'dirty state':\n%s", out)
	}
}

func TestFormatMigrationDirtyDiagnostic_OffersManualSQL(t *testing.T) {
	// §14.2 — diagnostic must include the equivalent UPDATE SQL operators
	// can run via psql to mark the version as not-dirty.
	out := formatMigrationDirtyDiagnostic(3)
	if !strings.Contains(out, "UPDATE omnicore_migrations SET dirty = false WHERE version = 3") {
		t.Errorf("diagnostic should include manual UPDATE SQL:\n%s", out)
	}
}

// ─── reconcileViewDrift — autoRun=false short-circuit ────────────────────────

// autoRun=false must skip every check, including drift detection itself.
// Operator opted out — the framework must not touch PG or Mongo. Passing nil
// deps.Postgres / deps.Mongo (and nil views) proves the early-return: if the
// short-circuit regressed, DetectViewDrift would dereference one of them and
// panic. A nil sync engine + nil views ensures no dispatch path is reachable
// either.
func TestReconcileViewDrift_AutoRunFalse_SkipsDetection(t *testing.T) {
	cfg := &Config{}
	cfg.Mongo.Rebuild.AutoRun = AutoRunFalse

	deps := Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Postgres + Mongo intentionally nil — must NOT be accessed.
	}

	if err := reconcileViewDrift(context.Background(), cfg, deps, nil, nil); err != nil {
		t.Fatalf("autoRun=false must return nil without touching PG/Mongo; got: %v", err)
	}
}
