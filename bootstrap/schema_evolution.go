package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra"
	"github.com/ClaudioSchirmer/omnicore/infra/migration"
)

// applyMigrations runs the PostgreSQL migration path. Behavior depends on
// cfg.Migrations.AutoRun (AutoRunMode):
//
//   - AutoRunTrue  → ValidateDownExists + Up (legacy behavior).
//   - AutoRunCheck → read mgr.Status + mgr.Pending; if dirty or pending,
//     abort with diagnostic listing recovery options.
//   - AutoRunFalse → skip entirely. Service boots whatever schema is in
//     PG; runtime errors on missing columns are the operator's
//     responsibility.
//
// The default value of AutoRun is profile-aware
// (dev=AutoRunTrue, else=AutoRunCheck; see applyProfileDefaults), so
// non-dev profiles default to check mode. Operator override via explicit
// yaml is honored.
func applyMigrations(ctx context.Context, cfg *Config, deps Deps) error {
	switch cfg.Migrations.AutoRun {
	case AutoRunFalse:
		deps.Logger.Info("migrations skipped (autoRun=false)")
		return nil
	}

	mgr := migration.New(deps.Postgres.Pool(), cfg.Migrations.Dir)

	if cfg.Migrations.AutoRun.IsTrue() {
		if err := mgr.ValidateDownExists(); err != nil {
			return fmt.Errorf("bootstrap: migration validate: %w", err)
		}
		if err := mgr.Up(ctx); err != nil {
			return fmt.Errorf("bootstrap: migration up: %w", err)
		}
		deps.Logger.Info("migrations applied", "dir", cfg.Migrations.Dir)
		return nil
	}

	// AutoRunCheck — read status and decide.
	current, dirty, err := mgr.Status(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: migration status: %w", err)
	}
	if dirty {
		return errors.New(formatMigrationDirtyDiagnostic(current))
	}
	pending, err := mgr.Pending(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap: migration pending: %w", err)
	}
	if len(pending) > 0 {
		return errors.New(formatMigrationPendingDiagnostic(current, pending))
	}
	deps.Logger.Info("migrations up to date (check mode)", "current", current)
	return nil
}

// formatMigrationPendingDiagnostic builds the §14.1 boot-abort message
// for pending migrations under autoRun=check. Names current + required +
// the operator's recovery options.
func formatMigrationPendingDiagnostic(current uint, pending []uint) string {
	required := pending[len(pending)-1]
	var sb strings.Builder
	sb.WriteString("[migrations] pending migration(s) detected:\n")
	for _, v := range pending {
		fmt.Fprintf(&sb, "  - version %d\n", v)
	}
	fmt.Fprintf(&sb, "\ncurrent DB version: %d. required: %d.\n", current, required)
	sb.WriteString("migrations.autoRun=check (profile-aware default in non-dev profile) —\n")
	sb.WriteString("the framework will NOT apply migrations in check mode.\n\n")
	sb.WriteString("To proceed, choose one:\n")
	sb.WriteString("  A. Let the framework apply pending migrations on next boot:\n")
	sb.WriteString("       set migrations.autoRun: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart\n")
	sb.WriteString("  B. Apply migrations manually now, then mark them in the control table:\n")
	for _, v := range pending {
		fmt.Fprintf(&sb, "       psql ... -f migrations/%04d_*.up.sql\n", v)
	}
	sb.WriteString("       psql ... -c \"INSERT INTO omnicore_migrations (version, dirty) VALUES")
	for i, v := range pending {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, " (%d, false)", v)
	}
	sb.WriteString(";\"\n       restart\n")
	sb.WriteString("  C. Skip the framework's migration check:\n")
	sb.WriteString("       set migrations.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

// formatMigrationDirtyDiagnostic builds the §14.2 boot-abort message
// when a previous migration left the tracking table in dirty state.
func formatMigrationDirtyDiagnostic(version uint) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[migrations] previous migration %d left the database in dirty state.\n", version)
	sb.WriteString("investigate the failure, then either:\n")
	fmt.Fprintf(&sb, "  - call migration.Manager.Force(ctx, %d) to mark a clean version, OR\n", version)
	sb.WriteString("  - restore from backup and redeploy.\n\n")
	fmt.Fprintf(&sb, "Manual SQL equivalent of Force:\n  psql ... -c \"UPDATE omnicore_migrations SET dirty = false WHERE version = %d;\"\n", version)
	return sb.String()
}

// reconcileViewDrift runs the Mongo drift detection against the PG
// registry and acts on each per-view DriftPlan. The 8-case matrix maps
// to the §9.1 table in tasks/mongo_schema_evolution_2.md.
//
// Order of evaluation:
//   - autoRun=false      — skip entirely (drift detection itself is not run).
//   - DriftAlienData     — abort under check/true (data we cannot certify).
//   - DriftForgotToBump  — abort under check/true (no escape; bump the version).
//   - autoRun=check      — any non-None decision aborts with §14 diagnostic.
//   - autoRun=true       — dispatch per-plan over the 5 actionable decisions
//                          (None / FreshInit / MongoWiped / ArtifactOnly /
//                           RebuildRequired / Downgrade-with-allowDowngrade).
//
// Returns nil only when the registry is fully reconciled to the declared
// shape (every plan either No-op'd, was Init'd, was Refreshed, or
// completed a rebuild).
func reconcileViewDrift(ctx context.Context, cfg *Config, deps Deps, sync *infra.SyncEngine, views []*infra.ViewDefinition) error {
	// autoRun=false — skip every branch, including drift detection itself.
	// Operator opted out of every framework-side check; runtime errors on
	// shape mismatch are their responsibility.
	if cfg.Mongo.Rebuild.AutoRun.IsFalse() {
		deps.Logger.Info("view drift reconciliation skipped (autoRun=false)")
		return nil
	}

	report, err := infra.DetectViewDrift(ctx, deps.Mongo, deps.Postgres, views)
	if err != nil {
		return fmt.Errorf("bootstrap: mongo drift detect: %w", err)
	}

	// Unconditional aborts under autoRun ∈ {check, true} — no escape.
	if report.HasAny(infra.DriftAlienData) {
		return errors.New(infra.FormatAlienDataDiagnostic(report.PlansBy(infra.DriftAlienData)))
	}
	if report.HasAny(infra.DriftForgotToBump) {
		return errors.New(infra.FormatForgotToBumpDiagnostic(report.PlansBy(infra.DriftForgotToBump)))
	}

	// autoRun=check — abort on any non-None decision.
	if cfg.Mongo.Rebuild.AutoRun.IsCheck() {
		if !report.NeedsAction() {
			deps.Logger.Info("view drift up to date (check mode)", "views", len(views))
			return nil
		}
		// Aggregate all check-mode diagnostics into one message naming
		// every offending view. Each diagnostic carries the specific
		// recovery SQL; concatenating them preserves all operator
		// instructions in a single boot-fatal payload.
		var diags []string
		if plans := report.PlansBy(infra.DriftFreshInit); len(plans) > 0 {
			diags = append(diags, infra.FormatFreshInitDiagnostic(plans))
		}
		if plans := report.PlansBy(infra.DriftMongoWiped); len(plans) > 0 {
			diags = append(diags, infra.FormatMongoWipedDiagnostic(plans))
		}
		if plans := report.PlansBy(infra.DriftArtifactOnly); len(plans) > 0 {
			diags = append(diags, infra.FormatArtifactOnlyDiagnostic(plans))
		}
		if plans := report.PlansBy(infra.DriftRebuildRequired); len(plans) > 0 {
			diags = append(diags, infra.FormatRebuildRequiredDiagnostic(plans))
		}
		if plans := report.PlansBy(infra.DriftDowngrade); len(plans) > 0 {
			diags = append(diags, infra.FormatDowngradeDiagnostic(plans))
		}
		return errors.New(strings.Join(diags, "\n"))
	}

	// autoRun=true — under allowDowngrade=false, downgrades still abort.
	if !cfg.Mongo.Rebuild.AllowDowngrade && report.HasAny(infra.DriftDowngrade) {
		return errors.New(infra.FormatDowngradeDiagnostic(report.PlansBy(infra.DriftDowngrade)))
	}

	rebuildCfg := infra.RebuildConfig{
		Orphan:      cfg.Mongo.Rebuild.Orphan,
		ServiceName: cfg.Service,
	}

	for _, plan := range report.Plans {
		switch plan.Decision {
		case infra.DriftNone:
			continue
		case infra.DriftFreshInit:
			if err := sync.InitRegistryOnly(ctx, plan, cfg.Service); err != nil {
				return fmt.Errorf("bootstrap: init registry on view %q: %w", plan.View.Name(), err)
			}
			deps.Logger.Info("view registry initialized",
				"view", plan.View.Name(),
				"version", plan.CurrentVersion)
		case infra.DriftArtifactOnly:
			if err := sync.RefreshRegistryArtifactOnly(ctx, plan, cfg.Service); err != nil {
				return fmt.Errorf("bootstrap: refresh registry artifact on view %q: %w", plan.View.Name(), err)
			}
			deps.Logger.Info("view registry artifact refreshed",
				"view", plan.View.Name(),
				"version", plan.CurrentVersion)
		case infra.DriftMongoWiped, infra.DriftRebuildRequired, infra.DriftDowngrade:
			// All three drive a rebuild under autoRun=true (Downgrade only
			// when allowDowngrade gated above).
			if err := sync.ExecuteRebuild(ctx, plan, rebuildCfg); err != nil {
				return fmt.Errorf("bootstrap: rebuild view %q: %w", plan.View.Name(), err)
			}
		}
	}
	return nil
}
