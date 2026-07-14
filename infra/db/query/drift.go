package query

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// DriftDecision is what the framework should do for one view, computed
// from the PG registry state vs the declared spec hash + the Mongo
// collection's population. The eight branches map 1:1 to
// tasks/mongo_schema_evolution_2.md §9.1.
type DriftDecision int

const (
	// DriftNone — registry combined hash matches spec; no-op.
	DriftNone DriftDecision = iota

	// DriftFreshInit — no registry row AND Mongo collection absent or
	// empty. Under autoRun=true: INSERT a registry row at spec version
	// (no rebuild needed; SyncEngine will populate organically). Under
	// autoRun=check: ABORT with §14.9 diagnostic. Under autoRun=false:
	// skip.
	DriftFreshInit

	// DriftAlienData — no registry row AND Mongo collection populated.
	// The framework cannot certify whether the docs are legitimate or
	// alien. Aborts under autoRun ∈ {check, true} (operator must manually
	// INSERT the registry row OR drop the collection and let the framework
	// rebuild). Under autoRun=false, drift detection itself does not run,
	// so this decision is never produced and boot proceeds.
	DriftAlienData

	// DriftMongoWiped — registry row present (and matches spec) BUT
	// Mongo collection is absent or empty. Operator dropped the
	// collection to force a rebuild. Under autoRun=true: REBUILD from
	// PG. Under autoRun=check: ABORT with §14.7 diagnostic.
	DriftMongoWiped

	// DriftArtifactOnly — registry present; only artifact hash differs.
	// ApplyMongoSpecs already brought indexes to the declared shape;
	// only the registry row needs refreshing. Under autoRun=true:
	// UPDATE the row. Under autoRun=check: ABORT with §14.8 diagnostic.
	DriftArtifactOnly

	// DriftForgotToBump — registry version matches spec version but
	// hashes differ. Developer changed shape without bumping Version(N).
	// Aborts under autoRun ∈ {check, true} — no escape there, the version
	// field IS the intent signal. Under autoRun=false, drift detection
	// itself does not run, so this decision is never produced and boot
	// proceeds.
	DriftForgotToBump

	// DriftRebuildRequired — registry version is older than spec
	// version (linear upgrade). Under autoRun=true: REBUILD. Under
	// autoRun=check: ABORT with §14.3 diagnostic.
	DriftRebuildRequired

	// DriftDowngrade — registry version is newer than spec version.
	// Under autoRun=true + allowDowngrade=false: ABORT with §14.6. Under
	// autoRun=true + allowDowngrade=true: REBUILD at the lower shape
	// (loses v(spec+1)..v(registry) audit trail). Under autoRun=check:
	// ABORT.
	DriftDowngrade
)

// driftDecisionString returns the lowercase snake case identifier for
// each decision. Used by slog logs and unit-test friendly diffing.
func (d DriftDecision) String() string {
	switch d {
	case DriftNone:
		return "none"
	case DriftFreshInit:
		return "fresh_init"
	case DriftAlienData:
		return "alien_data"
	case DriftMongoWiped:
		return "mongo_wiped"
	case DriftArtifactOnly:
		return "artifact_only"
	case DriftForgotToBump:
		return "forgot_to_bump"
	case DriftRebuildRequired:
		return "rebuild_required"
	case DriftDowngrade:
		return "downgrade"
	default:
		return "unknown"
	}
}

// DriftPlan carries one view's decision plus the inputs the diagnostic
// formatter + rebuild orchestrator need.
type DriftPlan struct {
	View     *ViewDefinition
	Decision DriftDecision

	// Registry is the row currently in omnicore_mongo_views. Nil when
	// none was found.
	Registry *ViewRegistryRow

	// CurrentVersion, CurrentRebuildHash, CurrentArtifactHash,
	// CurrentCombinedHash are computed from the View's declarative
	// state. Carried so the rebuild path + diagnostic formatter consume
	// them without recomputing.
	CurrentVersion      int
	CurrentRebuildHash  string
	CurrentArtifactHash string
	CurrentCombinedHash string
}

// DriftReport is the aggregate per-boot view of drift across every
// declared view.
type DriftReport struct {
	Plans []DriftPlan
}

// PlansBy returns the subset of plans matching the given decision. The
// caller iterates the returned slice; the original Plans slice is not
// mutated.
func (r *DriftReport) PlansBy(d DriftDecision) []DriftPlan {
	var out []DriftPlan
	for _, p := range r.Plans {
		if p.Decision == d {
			out = append(out, p)
		}
	}
	return out
}

// HasAny reports whether any plan matches one of the given decisions —
// helper for the dispatch code that wants to short-circuit on the
// unconditional-abort decisions (AlienData, ForgotToBump).
func (r *DriftReport) HasAny(decisions ...DriftDecision) bool {
	for _, p := range r.Plans {
		for _, d := range decisions {
			if p.Decision == d {
				return true
			}
		}
	}
	return false
}

// NeedsAction reports whether ANY plan requires an autoRun-gated action
// (a rebuild or a registry init/refresh). Used by reconcileViewDrift to
// decide whether to engage the rebuild engine at all.
func (r *DriftReport) NeedsAction() bool {
	for _, p := range r.Plans {
		switch p.Decision {
		case DriftNone:
			continue
		default:
			return true
		}
	}
	return false
}

// DetectViewDrift reads each view's relational registry row, computes the
// current hashes, and produces the per-view DriftPlan via the §9.1 decision
// matrix. Runs AFTER ApplyMongoSpecs in the boot sequence so the
// collection-level shape is already reconciled (case DriftFreshInit lands
// here when ApplyMongoSpecs just created the collection and it has no
// docs). The registry read goes through the engine's neutral Querier/Dialect,
// so it works on any relational backend (Postgres or MySQL).
//
// "Populated collection" is determined by a single ultra-fast query —
// CountDocuments with limit:1. O(1) regardless of collection size.
func DetectViewDrift(ctx context.Context, mongo ReadModelStore, eng core.RelationalEngine, views []*ViewDefinition) (*DriftReport, error) {
	report := &DriftReport{Plans: make([]DriftPlan, 0, len(views))}
	q := eng.Querier()
	d := eng.Dialect()
	for _, v := range views {
		registry, err := ReadViewRegistry(ctx, q, d, v.Name())
		if err != nil {
			return nil, err
		}
		populated, err := mongo.HasDocuments(ctx, v.Name())
		if err != nil {
			return nil, err
		}
		sorPopulated, err := sorHasRows(ctx, q, d, v.RootTable())
		if err != nil {
			return nil, err
		}
		plan := DriftPlan{
			View:                v,
			Registry:            registry,
			CurrentVersion:      v.VersionNumber(),
			CurrentRebuildHash:  v.RebuildHash(),
			CurrentArtifactHash: v.ArtifactHash(),
			CurrentCombinedHash: v.Hash(),
		}
		plan.Decision = decideDrift(registry, populated, sorPopulated, plan.CurrentVersion, plan.CurrentRebuildHash, plan.CurrentCombinedHash)
		report.Plans = append(report.Plans, plan)
	}
	return report, nil
}

// sorHasRows reports whether the view's ROOT table holds at least one row in
// the relational store — the disambiguator between "the operator wiped Mongo"
// (SoR has rows, collection empty → rebuild) and "the aggregate simply has no
// data yet" (both sides empty → the empty collection IS the correct mirror, no
// drift). O(1) on any backend: SELECT 1 … LIMIT 1 through the neutral
// Querier/Dialect.
func sorHasRows(ctx context.Context, q core.Querier, d core.Dialect, table string) (bool, error) {
	rows, err := q.Query(ctx, d.ApplyLimit("SELECT 1 FROM "+d.QuoteIdent(table), 1))
	if err != nil {
		return false, fmt.Errorf("probe root table %q: %w", table, err)
	}
	defer rows.Close()
	if rows.Next() {
		return true, nil
	}
	return false, rows.Err()
}

// decideDrift is the pure decision function — extracted so the §9.1 case
// table is testable in isolation, without an active PG or Mongo.
func decideDrift(registry *ViewRegistryRow, populated, sorPopulated bool, specVersion int, specRebuildHash, specCombinedHash string) DriftDecision {
	if registry == nil {
		if populated {
			return DriftAlienData
		}
		return DriftFreshInit
	}
	if registry.CombinedHash == specCombinedHash {
		// Combined hash matches → no shape drift. An empty collection is only
		// "wiped" when the source of record actually HAS rows to mirror — a
		// view whose aggregate holds no data yet is correctly empty on both
		// sides and must not rebuild on every boot.
		if !populated && sorPopulated {
			return DriftMongoWiped
		}
		return DriftNone
	}
	// Combined hash differs — disambiguate by version.
	switch {
	case registry.Version < specVersion:
		return DriftRebuildRequired
	case registry.Version > specVersion:
		return DriftDowngrade
	default:
		// Versions equal but hashes differ.
		if registry.RebuildHash == specRebuildHash {
			// Only artifact (index) hash differs.
			return DriftArtifactOnly
		}
		// Rebuild hash differs without a version bump — developer error.
		return DriftForgotToBump
	}
}

// FormatRebuildRequiredDiagnostic builds the §14.3 abort message under
// autoRun=check: registry says vN, spec says vM (M > N), framework will
// not rebuild in check mode.
func FormatRebuildRequiredDiagnostic(plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] view shape drift detected (rebuild required) on the following view(s):\n")
	for _, p := range plans {
		regV, regH := "<none>", "<none>"
		if p.Registry != nil {
			regV = fmt.Sprintf("v%d", p.Registry.Version)
			regH = shortHash(p.Registry.CombinedHash)
		}
		fmt.Fprintf(&sb, "  - %q  registry=%s hash=%s  spec=v%d hash=%s\n",
			p.View.Name(),
			regV,
			regH,
			p.CurrentVersion,
			shortHash(p.CurrentCombinedHash))
	}
	sb.WriteString("\nmongo.rebuild.autoRun=check (profile-aware default in non-dev) —\n")
	sb.WriteString("the framework will NOT rebuild in check mode.\n\n")
	sb.WriteString("To proceed, choose one per view:\n")
	sb.WriteString("  A. Let the framework rebuild on next boot:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart\n")
	sb.WriteString("  B. Apply the manual SQL reconcile (after rebuilding Mongo out of band):\n")
	sb.WriteString("       UPDATE omnicore_mongo_views\n")
	sb.WriteString("          SET previous_version = version, previous_combined_hash = combined_hash,\n")
	sb.WriteString("              previous_applied_at = applied_at,\n")
	sb.WriteString("              version = <spec.version>, rebuild_hash = '<spec.rebuild>',\n")
	sb.WriteString("              artifact_hash = '<spec.artifact>', combined_hash = '<spec.combined>',\n")
	sb.WriteString("              status = 'done', started_at = NULL,\n")
	sb.WriteString("              applied_at = CURRENT_TIMESTAMP, applied_by = 'manual-reconcile-rebuild'\n")
	sb.WriteString("        WHERE view_name = '<view>';\n")
	sb.WriteString("  C. Skip the framework's check entirely:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

// registryIDLiteral mints a fresh UUID v7 and renders it as the given
// dialect's SQL literal for the registry's native id column, so the generated
// operator scripts carry a ready-to-run id: 'uuid' on postgres (uuid column),
// X'hex' on mysql and 0xHEX on sqlserver (BINARY(16) columns). Diagnostics
// text only — never executed by the framework itself.
func registryIDLiteral(dialect string) string {
	u, err := uuid.NewV7()
	if err != nil {
		u = uuid.New() // best-effort: the literal only needs uniqueness
	}
	h := hex.EncodeToString(u[:])
	switch dialect {
	case "mysql":
		return "X'" + h + "'"
	case "sqlserver":
		return "0x" + h
	default:
		return "'" + u.String() + "'"
	}
}

// FormatAlienDataDiagnostic builds the §14.4 abort message: populated
// Mongo collection without a registry row. ABORTS regardless of autoRun.
func FormatAlienDataDiagnostic(dialect string, plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] view(s) carry data the framework cannot certify:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  (Mongo collection populated but no registry row)\n", p.View.Name())
	}
	sb.WriteString("\nautoRun cannot escape this case — proceeding would risk overwriting data the operator wants preserved.\n\n")
	sb.WriteString("To proceed, choose one per view:\n")
	sb.WriteString("  A. Acknowledge the state and let the framework take ownership WITHOUT rebuilding:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb,
			"       INSERT INTO omnicore_mongo_views (id, view_name, version, rebuild_hash, artifact_hash,\n"+
				"                                          combined_hash, status, applied_at, applied_by)\n"+
				"       VALUES (%s, '%s', %d, '%s', '%s', '%s', 'done', CURRENT_TIMESTAMP, 'manual-reconcile-tofu');\n",
			registryIDLiteral(dialect), p.View.Name(), p.CurrentVersion, p.CurrentRebuildHash, p.CurrentArtifactHash, p.CurrentCombinedHash)
	}
	sb.WriteString("  B. Drop the Mongo collection and let the framework rebuild from PG:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "       db.%s.drop()\n", p.View.Name())
	}
	sb.WriteString("       set mongo.rebuild.autoRun: true   (rebuilds from PG on boot)\n")
	sb.WriteString("       restart  (after success, revert autoRun to the previous value)\n")
	sb.WriteString("  C. Skip the framework's check:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

// FormatForgotToBumpDiagnostic builds the §14.5 abort message: registry
// version matches spec version but hashes differ. ABORTS regardless of
// autoRun.
func FormatForgotToBumpDiagnostic(plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] view shape changed in code without bumping Version():\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  version=v%d  registry_hash=%s  spec_hash=%s\n",
			p.View.Name(), p.CurrentVersion,
			shortHash(p.Registry.CombinedHash),
			shortHash(p.CurrentCombinedHash))
	}
	sb.WriteString("\nautoRun cannot resolve this — the Version(N) integer is the framework's only signal of developer intent.\n\n")
	sb.WriteString("To proceed, choose one:\n")
	sb.WriteString("  A. The shape change was intentional — bump the Version and redeploy:\n")
	sb.WriteString("       fwinfra.View(\"<view>\").Version(N+1).Root(...).<...>\n")
	sb.WriteString("  B. The shape change was accidental — revert your code to the previous shape and redeploy.\n")
	return sb.String()
}

// FormatDowngradeDiagnostic builds the §14.6 abort message: registry
// version is newer than spec version. ABORTS under autoRun=true unless
// mongo.rebuild.allowDowngrade is true.
func FormatDowngradeDiagnostic(plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] deployed code is older than the registry state for:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  registry=v%d hash=%s  spec=v%d hash=%s\n",
			p.View.Name(),
			p.Registry.Version, shortHash(p.Registry.CombinedHash),
			p.CurrentVersion, shortHash(p.CurrentCombinedHash))
	}
	sb.WriteString("\nThis typically means a rollback brought code back to an older version OR multiple services point at the same PG (DB-per-service violation).\n\n")
	sb.WriteString("To proceed, choose one:\n")
	sb.WriteString("  A. Verify the rollback is intentional and acknowledge by editing the control table:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb,
			"       UPDATE omnicore_mongo_views\n"+
				"          SET previous_version = version, previous_combined_hash = combined_hash,\n"+
				"              previous_applied_at = applied_at,\n"+
				"              version = %d, rebuild_hash = '%s', artifact_hash = '%s', combined_hash = '%s',\n"+
				"              applied_at = CURRENT_TIMESTAMP, applied_by = 'manual-reconcile-downgrade'\n"+
				"        WHERE view_name = '%s';\n",
			p.CurrentVersion, p.CurrentRebuildHash, p.CurrentArtifactHash, p.CurrentCombinedHash, p.View.Name())
	}
	sb.WriteString("  B. Opt in to automated rollback handling (canary / blue-green deploys):\n")
	sb.WriteString("       set mongo.rebuild.allowDowngrade: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart  (framework runs the downgrade rebuild automatically)\n")
	sb.WriteString("  C. Re-deploy with the latest code (matching the registry version).\n")
	return sb.String()
}

// FormatMongoWipedDiagnostic builds the §14.7 abort message under
// autoRun=check: registry present, Mongo collection absent or empty.
func FormatMongoWipedDiagnostic(plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] view(s) wiped — registry present but Mongo collection is absent or empty:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  registry=v%d hash=%s\n",
			p.View.Name(), p.Registry.Version, shortHash(p.Registry.CombinedHash))
	}
	sb.WriteString("\nmongo.rebuild.autoRun=check (profile-aware default in non-dev) — the framework will NOT rebuild.\n\n")
	sb.WriteString("To proceed, choose one:\n")
	sb.WriteString("  A. Let the framework rebuild on next boot:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart  (framework rebuilds from PG, registry status cycles through 'processing')\n")
	sb.WriteString("  B. Rebuild manually now (operator tooling), then mark the rebuild as done:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb,
			"       UPDATE omnicore_mongo_views SET status = 'done', started_at = NULL,\n"+
				"              applied_at = CURRENT_TIMESTAMP, applied_by = 'manual-reconcile-rebuild'\n"+
				"        WHERE view_name = '%s';\n",
			p.View.Name())
	}
	sb.WriteString("  C. Skip the framework's check:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

// FormatArtifactOnlyDiagnostic builds the §14.8 abort message under
// autoRun=check: registry present, only the artifact hash differs.
func FormatArtifactOnlyDiagnostic(plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] index declaration changed but document shape is unchanged on:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  registry_artifact=%s  spec_artifact=%s\n",
			p.View.Name(),
			shortHash(p.Registry.ArtifactHash),
			shortHash(p.CurrentArtifactHash))
	}
	sb.WriteString("\nmongo.rebuild.autoRun=check — the framework will NOT refresh the registry in check mode.\n")
	sb.WriteString("ApplyMongoSpecs already brought the Mongo collection's indexes to the declared shape during this boot;\n")
	sb.WriteString("only the registry row is stale.\n\n")
	sb.WriteString("To proceed, choose one:\n")
	sb.WriteString("  A. Let the framework refresh the registry on next boot:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart  (metadata-only UPDATE, no rebuild)\n")
	sb.WriteString("  B. Refresh the registry row manually:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb,
			"       UPDATE omnicore_mongo_views\n"+
				"          SET previous_combined_hash = combined_hash, previous_applied_at = applied_at,\n"+
				"              artifact_hash = '%s', combined_hash = '%s',\n"+
				"              applied_at = CURRENT_TIMESTAMP, applied_by = 'manual-reconcile-artifact'\n"+
				"        WHERE view_name = '%s';\n",
			p.CurrentArtifactHash, p.CurrentCombinedHash, p.View.Name())
	}
	sb.WriteString("  C. Skip the framework's check:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

// FormatFreshInitDiagnostic builds the §14.9 abort message under
// autoRun=check: no registry row AND empty Mongo collection.
func FormatFreshInitDiagnostic(dialect string, plans []DriftPlan) string {
	if len(plans) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[mongo] fresh init detected (no registry row and empty Mongo collection):\n")
	for _, p := range plans {
		fmt.Fprintf(&sb, "  - %q  spec=v%d hash=%s\n",
			p.View.Name(), p.CurrentVersion, shortHash(p.CurrentCombinedHash))
	}
	sb.WriteString("\nmongo.rebuild.autoRun=check — the framework will NOT write the registry row in check mode.\n\n")
	sb.WriteString("To proceed, choose one per view:\n")
	sb.WriteString("  A. Acknowledge the fresh init by writing the registry row manually:\n")
	for _, p := range plans {
		fmt.Fprintf(&sb,
			"       INSERT INTO omnicore_mongo_views\n"+
				"         (id, view_name, version, rebuild_hash, artifact_hash, combined_hash,\n"+
				"          status, applied_at, applied_by)\n"+
				"       VALUES (%s, '%s', %d, '%s', '%s', '%s', 'done', CURRENT_TIMESTAMP, 'manual-reconcile-init');\n",
			registryIDLiteral(dialect), p.View.Name(), p.CurrentVersion, p.CurrentRebuildHash, p.CurrentArtifactHash, p.CurrentCombinedHash)
	}
	sb.WriteString("  B. Let the framework initialize on next boot:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: true in microservice.<profile>.yaml\n")
	sb.WriteString("       restart  (after success, revert autoRun to the previous value)\n")
	sb.WriteString("  C. Skip the framework's check:\n")
	sb.WriteString("       set mongo.rebuild.autoRun: false in microservice.<profile>.yaml\n")
	return sb.String()
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}
