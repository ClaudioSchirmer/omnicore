package mongo

import (
	"strings"
	"testing"
)

// The drift detection's pure decision logic (decideDrift) and the
// diagnostic formatters are covered here. The PG + Mongo round-trip
// (DetectViewDrift, hasUserDocuments, ReadViewRegistry) is exercised by
// the omnicore-example-users E2E suite (qa/schema_evolution.sh).

// ─── decideDrift — the 8 branches of §9.1 ────────────────────────────────────

func TestDecideDrift_FreshInit_RegistryAbsentMongoEmpty(t *testing.T) {
	got := decideDrift(nil, false, 1, "rh", "ch")
	if got != DriftFreshInit {
		t.Errorf("decideDrift(registry=nil, populated=false) = %v, want DriftFreshInit", got)
	}
}

func TestDecideDrift_AlienData_RegistryAbsentMongoPopulated(t *testing.T) {
	got := decideDrift(nil, true, 1, "rh", "ch")
	if got != DriftAlienData {
		t.Errorf("decideDrift(registry=nil, populated=true) = %v, want DriftAlienData", got)
	}
}

func TestDecideDrift_None_RegistryMatchesAndMongoPopulated(t *testing.T) {
	reg := &ViewRegistryRow{Version: 1, RebuildHash: "rh", CombinedHash: "ch"}
	got := decideDrift(reg, true, 1, "rh", "ch")
	if got != DriftNone {
		t.Errorf("decideDrift(combined matches, populated) = %v, want DriftNone", got)
	}
}

func TestDecideDrift_MongoWiped_RegistryMatchesButMongoEmpty(t *testing.T) {
	reg := &ViewRegistryRow{Version: 1, RebuildHash: "rh", CombinedHash: "ch"}
	got := decideDrift(reg, false, 1, "rh", "ch")
	if got != DriftMongoWiped {
		t.Errorf("decideDrift(combined matches, populated=false) = %v, want DriftMongoWiped", got)
	}
}

func TestDecideDrift_ArtifactOnly_SameVersionSameRebuildDifferentCombined(t *testing.T) {
	reg := &ViewRegistryRow{Version: 1, RebuildHash: "rh", CombinedHash: "ch_old"}
	got := decideDrift(reg, true, 1, "rh", "ch_new")
	if got != DriftArtifactOnly {
		t.Errorf("decideDrift(same version, same rebuild, combined differs) = %v, want DriftArtifactOnly", got)
	}
}

func TestDecideDrift_ForgotToBump_SameVersionDifferentRebuild(t *testing.T) {
	reg := &ViewRegistryRow{Version: 1, RebuildHash: "rh_old", CombinedHash: "ch_old"}
	got := decideDrift(reg, true, 1, "rh_new", "ch_new")
	if got != DriftForgotToBump {
		t.Errorf("decideDrift(same version, rebuild differs) = %v, want DriftForgotToBump", got)
	}
}

func TestDecideDrift_RebuildRequired_RegistryVersionLowerThanSpec(t *testing.T) {
	reg := &ViewRegistryRow{Version: 1, RebuildHash: "rh_old", CombinedHash: "ch_old"}
	got := decideDrift(reg, true, 2, "rh_new", "ch_new")
	if got != DriftRebuildRequired {
		t.Errorf("decideDrift(registry v1, spec v2) = %v, want DriftRebuildRequired", got)
	}
}

func TestDecideDrift_Downgrade_RegistryVersionHigherThanSpec(t *testing.T) {
	reg := &ViewRegistryRow{Version: 5, RebuildHash: "rh_new", CombinedHash: "ch_new"}
	got := decideDrift(reg, true, 3, "rh_old", "ch_old")
	if got != DriftDowngrade {
		t.Errorf("decideDrift(registry v5, spec v3) = %v, want DriftDowngrade", got)
	}
}

// ─── DriftReport aggregations ────────────────────────────────────────────────

func TestDriftReport_HasAny_TrueWhenAnyMatches(t *testing.T) {
	r := &DriftReport{Plans: []DriftPlan{
		{Decision: DriftNone},
		{Decision: DriftAlienData},
	}}
	if !r.HasAny(DriftAlienData) {
		t.Error("HasAny(DriftAlienData) should be true")
	}
	if !r.HasAny(DriftNone, DriftAlienData) {
		t.Error("HasAny with multiple targets should match either")
	}
}

func TestDriftReport_HasAny_FalseWhenNoneMatches(t *testing.T) {
	r := &DriftReport{Plans: []DriftPlan{
		{Decision: DriftNone},
		{Decision: DriftArtifactOnly},
	}}
	if r.HasAny(DriftRebuildRequired) {
		t.Error("HasAny should be false when no plan matches")
	}
}

func TestDriftReport_NeedsAction_TrueOnAnyNonNone(t *testing.T) {
	r := &DriftReport{Plans: []DriftPlan{
		{Decision: DriftNone},
		{Decision: DriftRebuildRequired},
	}}
	if !r.NeedsAction() {
		t.Error("NeedsAction should be true when any plan is not None")
	}
}

func TestDriftReport_NeedsAction_FalseWhenAllNone(t *testing.T) {
	r := &DriftReport{Plans: []DriftPlan{
		{Decision: DriftNone},
		{Decision: DriftNone},
	}}
	if r.NeedsAction() {
		t.Error("NeedsAction should be false when all plans are None")
	}
}

func TestDriftReport_PlansBy_FiltersCorrectly(t *testing.T) {
	r := &DriftReport{Plans: []DriftPlan{
		{Decision: DriftNone},
		{Decision: DriftRebuildRequired},
		{Decision: DriftRebuildRequired},
		{Decision: DriftArtifactOnly},
	}}
	got := r.PlansBy(DriftRebuildRequired)
	if len(got) != 2 {
		t.Errorf("PlansBy(RebuildRequired) returned %d, want 2", len(got))
	}
	for _, p := range got {
		if p.Decision != DriftRebuildRequired {
			t.Errorf("PlansBy returned a plan with Decision=%v, want DriftRebuildRequired", p.Decision)
		}
	}
}

// ─── Diagnostic formatters — coverage of the §14 family ──────────────────────

func TestFormatAlienDataDiagnostic_NamesEveryViewAndSQL(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftAlienData,
			CurrentVersion:      1,
			CurrentRebuildHash:  "rh",
			CurrentArtifactHash: "ah",
			CurrentCombinedHash: "ch",
		},
		{
			View:                View("orders").Version(1).Root("orders"),
			Decision:            DriftAlienData,
			CurrentVersion:      1,
			CurrentRebuildHash:  "rh2",
			CurrentArtifactHash: "ah2",
			CurrentCombinedHash: "ch2",
		},
	}
	out := FormatAlienDataDiagnostic(plans)
	for _, expected := range []string{"users", "orders", "INSERT INTO omnicore_mongo_views", "manual-reconcile-tofu", "db.users.drop()", "mongo.rebuild.autoRun"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatAlienDataDiagnostic missing %q", expected)
		}
	}
}

func TestFormatAlienDataDiagnostic_EmptyOnNoPlans(t *testing.T) {
	if got := FormatAlienDataDiagnostic(nil); got != "" {
		t.Errorf("formatter for empty input = %q, want empty", got)
	}
}

func TestFormatForgotToBumpDiagnostic_NamesViewAndHashes(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftForgotToBump,
			CurrentVersion:      3,
			CurrentCombinedHash: "spec_hash_12345678",
			Registry: &ViewRegistryRow{
				Version:      3,
				CombinedHash: "registry_hash_12345678",
			},
		},
	}
	out := FormatForgotToBumpDiagnostic(plans)
	for _, expected := range []string{"users", "v3", "Version(N+1)", "without bumping", "spec_hash_12345"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatForgotToBumpDiagnostic missing %q", expected)
		}
	}
}

func TestFormatDowngradeDiagnostic_OffersAllowDowngradeFlag(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftDowngrade,
			CurrentVersion:      3,
			CurrentRebuildHash:  "rh",
			CurrentArtifactHash: "ah",
			CurrentCombinedHash: "ch_v3",
			Registry: &ViewRegistryRow{
				Version:      5,
				CombinedHash: "ch_v5",
			},
		},
	}
	out := FormatDowngradeDiagnostic(plans)
	for _, expected := range []string{"users", "v3", "v5", "mongo.rebuild.allowDowngrade: true", "manual-reconcile-downgrade"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatDowngradeDiagnostic missing %q", expected)
		}
	}
}

func TestFormatMongoWipedDiagnostic_OffersAutoRunFlip(t *testing.T) {
	plans := []DriftPlan{
		{
			View:     View("users").Version(1).Root("users"),
			Decision: DriftMongoWiped,
			Registry: &ViewRegistryRow{Version: 1, CombinedHash: "rh"},
		},
	}
	out := FormatMongoWipedDiagnostic(plans)
	for _, expected := range []string{"users", "wiped", "autoRun: true", "manual-reconcile-rebuild"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatMongoWipedDiagnostic missing %q", expected)
		}
	}
}

func TestFormatArtifactOnlyDiagnostic_NoRebuildMention(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftArtifactOnly,
			CurrentArtifactHash: "ah_new",
			CurrentCombinedHash: "ch_new",
			Registry: &ViewRegistryRow{
				ArtifactHash: "ah_old",
				CombinedHash: "ch_old",
			},
		},
	}
	out := FormatArtifactOnlyDiagnostic(plans)
	for _, expected := range []string{"users", "ah_new", "manual-reconcile-artifact", "no rebuild"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatArtifactOnlyDiagnostic missing %q", expected)
		}
	}
}

func TestFormatFreshInitDiagnostic_OffersInitSQL(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftFreshInit,
			CurrentVersion:      1,
			CurrentRebuildHash:  "rh",
			CurrentArtifactHash: "ah",
			CurrentCombinedHash: "ch",
		},
	}
	out := FormatFreshInitDiagnostic(plans)
	for _, expected := range []string{"users", "fresh init", "INSERT INTO omnicore_mongo_views", "'done'", "manual-reconcile-init"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatFreshInitDiagnostic missing %q", expected)
		}
	}
}

func TestFormatRebuildRequiredDiagnostic_ShowsVersionAndHashes(t *testing.T) {
	plans := []DriftPlan{
		{
			View:                View("users").Version(1).Root("users"),
			Decision:            DriftRebuildRequired,
			CurrentVersion:      2,
			CurrentCombinedHash: "spec_combined_0123456789abcdef",
			Registry: &ViewRegistryRow{
				Version:      1,
				CombinedHash: "reg_combined_aabbccddeeff0011",
			},
		},
	}
	out := FormatRebuildRequiredDiagnostic(plans)
	for _, expected := range []string{"users", "v1", "v2", "shape drift", "manual-reconcile-rebuild"} {
		if !strings.Contains(out, expected) {
			t.Errorf("FormatRebuildRequiredDiagnostic missing %q", expected)
		}
	}
}

func TestFormatRebuildRequired_EmptyOnNoPlans(t *testing.T) {
	if got := FormatRebuildRequiredDiagnostic(nil); got != "" {
		t.Errorf("formatter for empty input = %q, want empty", got)
	}
}

// ─── DriftDecision string roundtrip ──────────────────────────────────────────

func TestDriftDecision_String_CoversAll(t *testing.T) {
	cases := map[DriftDecision]string{
		DriftNone:            "none",
		DriftFreshInit:       "fresh_init",
		DriftAlienData:       "alien_data",
		DriftMongoWiped:      "mongo_wiped",
		DriftArtifactOnly:    "artifact_only",
		DriftForgotToBump:    "forgot_to_bump",
		DriftRebuildRequired: "rebuild_required",
		DriftDowngrade:       "downgrade",
	}
	for d, want := range cases {
		if got := d.String(); got != want {
			t.Errorf("DriftDecision(%d).String() = %q, want %q", d, got, want)
		}
	}
}

// ─── shortHash ───────────────────────────────────────────────────────────────

func TestShortHash_LongInput(t *testing.T) {
	full := "abcdef0123456789aaaaaaaaaaaaaaaa00000000000000000000000000000000"
	got := shortHash(full)
	if len(got) != 16 {
		t.Errorf("shortHash len = %d, want 16", len(got))
	}
	if got != "abcdef0123456789" {
		t.Errorf("shortHash = %q, want first 16 chars", got)
	}
}

func TestShortHash_ShortInput_PassesThrough(t *testing.T) {
	if got := shortHash("abc"); got != "abc" {
		t.Errorf("shortHash on short input = %q, want passthrough", got)
	}
}
