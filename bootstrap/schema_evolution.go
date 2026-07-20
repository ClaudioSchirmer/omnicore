package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// applyMigrations runs the relational migration path. Behavior depends on
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

	// The migration runner is dialect-bound and resolved through the
	// engineBoots registry each engine_<dialect>.go binding populates: Postgres
	// runs over the live pgx pool; MySQL, SQL Server and Oracle open their own
	// *sql.DB from relational.dsn (the runner never owns the engine pool). A build
	// links exactly the bindings its tags select.
	mgr := newMigrator(deps, cfg)
	if mgr == nil {
		return fmt.Errorf("bootstrap: no migration runner registered for dialect %q (build with the engine's build tag)", cfg.Relational.Dialect)
	}

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
//     (None / FreshInit / MongoWiped / ArtifactOnly /
//     RebuildRequired / Downgrade-with-allowDowngrade).
//
// Returns nil only when the registry is fully reconciled to the declared
// shape (every plan either No-op'd, was Init'd, was Refreshed, or
// completed a rebuild).
// reconcileViewDrift runs the SYNCHRONOUS part of drift reconciliation at boot:
// detection, the unconditional / check-mode aborts (so a bad drift still crashes
// the boot before the HTTP server comes up), and the fast registry-only paths
// (InitRegistryOnly / RefreshRegistryArtifactOnly). It returns the plans that
// need a full ExecuteRebuild — the SLOW blue-green rebuild the caller runs in the
// background so the HTTP probes come up immediately (livez 200, readyz 503 until
// the rebuilds finish). rebuildCfg is returned alongside for the caller's loop.
func reconcileViewDrift(ctx context.Context, cfg *Config, deps Deps, sync *query.SyncEngine, views []*query.ViewDefinition) (rebuildPlans []query.DriftPlan, rebuildCfg query.RebuildConfig, err error) {
	// The Mongo-view drift/rebuild control plane is backend-neutral: it reads the
	// omnicore_mongo_views registry through the engine's Querier/Dialect and
	// serializes rebuilds on the engine's AcquireRebuildLock (PG pg_advisory_lock,
	// MySQL GET_LOCK), so it runs on any relational backend.

	// autoRun=false — skip every branch, including drift detection itself.
	// Operator opted out of every framework-side check; runtime errors on
	// shape mismatch are their responsibility.
	if cfg.Mongo.Rebuild.AutoRun.IsFalse() {
		deps.Logger.Info("view drift reconciliation skipped (autoRun=false)")
		return nil, rebuildCfg, nil
	}

	report, err := query.DetectViewDrift(ctx, deps.Mongo, deps.DB, views, deps.Resolver)
	if err != nil {
		return nil, rebuildCfg, fmt.Errorf("bootstrap: mongo drift detect: %w", err)
	}

	// Unconditional aborts under autoRun ∈ {check, true} — no escape.
	if report.HasAny(query.DriftAlienData) {
		return nil, rebuildCfg, errors.New(query.FormatAlienDataDiagnostic(cfg.Relational.Dialect, report.PlansBy(query.DriftAlienData)))
	}
	if report.HasAny(query.DriftForgotToBump) {
		return nil, rebuildCfg, errors.New(query.FormatForgotToBumpDiagnostic(report.PlansBy(query.DriftForgotToBump)))
	}

	// autoRun=check — abort on any non-None decision.
	if cfg.Mongo.Rebuild.AutoRun.IsCheck() {
		if !report.NeedsAction() {
			deps.Logger.Info("view drift up to date (check mode)", "views", len(views))
			return nil, rebuildCfg, nil
		}
		// Aggregate all check-mode diagnostics into one message naming
		// every offending view. Each diagnostic carries the specific
		// recovery SQL; concatenating them preserves all operator
		// instructions in a single boot-fatal payload.
		var diags []string
		if plans := report.PlansBy(query.DriftFreshInit); len(plans) > 0 {
			diags = append(diags, query.FormatFreshInitDiagnostic(cfg.Relational.Dialect, plans))
		}
		if plans := report.PlansBy(query.DriftMongoWiped); len(plans) > 0 {
			diags = append(diags, query.FormatMongoWipedDiagnostic(plans))
		}
		if plans := report.PlansBy(query.DriftArtifactOnly); len(plans) > 0 {
			diags = append(diags, query.FormatArtifactOnlyDiagnostic(plans))
		}
		if plans := report.PlansBy(query.DriftRebuildRequired); len(plans) > 0 {
			diags = append(diags, query.FormatRebuildRequiredDiagnostic(plans))
		}
		if plans := report.PlansBy(query.DriftDowngrade); len(plans) > 0 {
			diags = append(diags, query.FormatDowngradeDiagnostic(plans))
		}
		return nil, rebuildCfg, errors.New(strings.Join(diags, "\n"))
	}

	// autoRun=true — under allowDowngrade=false, downgrades still abort.
	if !cfg.Mongo.Rebuild.AllowDowngrade && report.HasAny(query.DriftDowngrade) {
		return nil, rebuildCfg, errors.New(query.FormatDowngradeDiagnostic(report.PlansBy(query.DriftDowngrade)))
	}

	rebuildCfg = query.RebuildConfig{
		Orphan:      cfg.Mongo.Rebuild.Orphan,
		ServiceName: cfg.Service,
		Workers:     cfg.Mongo.Rebuild.Workers,
		BatchSize:   cfg.Mongo.Rebuild.BatchSize,
	}

	for _, plan := range report.Plans {
		switch plan.Decision {
		case query.DriftNone:
			continue
		case query.DriftFreshInit:
			if err := sync.InitRegistryOnly(ctx, plan, cfg.Service); err != nil {
				return nil, rebuildCfg, fmt.Errorf("bootstrap: init registry on view %q: %w", plan.View.Name(), err)
			}
			deps.Logger.Info("view registry initialized",
				"view", plan.View.Name(),
				"version", plan.CurrentVersion)
		case query.DriftArtifactOnly:
			if err := sync.RefreshRegistryArtifactOnly(ctx, plan, cfg.Service); err != nil {
				return nil, rebuildCfg, fmt.Errorf("bootstrap: refresh registry artifact on view %q: %w", plan.View.Name(), err)
			}
			deps.Logger.Info("view registry artifact refreshed",
				"view", plan.View.Name(),
				"version", plan.CurrentVersion)
		case query.DriftMongoWiped, query.DriftRebuildRequired, query.DriftDowngrade:
			// All three drive a full blue-green rebuild under autoRun=true
			// (Downgrade only when allowDowngrade gated above). These are the
			// SLOW paths — collected and returned so the caller runs them in the
			// background while the HTTP probes are already up (readyz 503 until
			// they finish).
			rebuildPlans = append(rebuildPlans, plan)
		}
	}
	return rebuildPlans, rebuildCfg, nil
}
