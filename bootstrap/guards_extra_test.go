package bootstrap

import (
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/read/mongo"
	"github.com/ClaudioSchirmer/omnicore/infra/db"
	"github.com/ClaudioSchirmer/omnicore/web/openapi"
)

// ─── describeFilter ──────────────────────────────────────────────────────────

func TestDescribeFilter(t *testing.T) {
	if got := describeFilter(nil); got != "(empty)" {
		t.Errorf("nil → %q, want (empty)", got)
	}
	if got := describeFilter([]string{}); got != "(empty)" {
		t.Errorf("empty slice → %q, want (empty)", got)
	}
	if got := describeFilter([]string{"name", "email"}); got != "[name, email]" {
		t.Errorf("populated → %q, want [name, email]", got)
	}
}

// ─── applyUpstreamSubscriptionDefaults ───────────────────────────────────────

func TestApplyUpstreamSubscriptionDefaults_FillsPerEntry(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users"},
		{Topic: "orders.events", Collection: "orders", Workers: 4, ConsumerGroup: "explicit"},
	}
	out := applyUpstreamSubscriptionDefaults(subs, "billing")
	if out[0].ConsumerGroup != "billing-upstream-users.events" {
		t.Errorf("entry[0] ConsumerGroup default = %q", out[0].ConsumerGroup)
	}
	if out[0].Workers != 1 || out[0].StartFrom != StartFromLatest || out[0].OnUpstreamDelete != UpstreamDeleteCascade {
		t.Errorf("entry[0] defaults not applied: %+v", out[0])
	}
	if out[1].ConsumerGroup != "explicit" || out[1].Workers != 4 {
		t.Errorf("entry[1] explicit values overwritten: %+v", out[1])
	}
}

// ─── guardJoinFieldIndex — missing join field branch ─────────────────────────

func TestGuardJoinFieldIndex_RejectsMissingJoinField(t *testing.T) {
	// External embed declared without a join field (no .On / no FK) — the
	// guard reports the missing-join-field diagnostic rather than the
	// missing-index one.
	v := mongo.View("orders").Root("orders").
		Embed("buyer", mongo.FromSchema(db.NewExternalSchema("users").PK("id")).As("Buyer")).
		Version(1)
	errs := guardJoinFieldIndex([]*mongo.ViewDefinition{v})
	if len(errs) != 1 || !strings.Contains(errs[0], "§8.1") ||
		!strings.Contains(errs[0], "no join field declared") {
		t.Errorf("expected missing-join-field diagnostic, got %v", errs)
	}
}

// ─── validateUpstreamSubscriptions — clean pass ──────────────────────────────

func TestValidateUpstreamSubscriptions_PassesWhenClean(t *testing.T) {
	subs := []UpstreamSubscription{
		{Topic: "users.events", Collection: "users",
			StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteCascade},
	}
	views := []*mongo.ViewDefinition{
		mongo.View("orders").Root("orders").
			Embed("buyer", extEmbed("users", "buyer_id", "Buyer")).
			Indexes(mongo.Index("buyer_id")).
			Version(1),
	}
	if err := validateUpstreamSubscriptions(subs, views, profileDev); err != nil {
		t.Fatalf("clean subscriptions must validate, got %v", err)
	}
}

func TestValidateUpstreamSubscriptions_SurfacesShapeViolation(t *testing.T) {
	// A per-entry shape violation (missing topic) is reported with the
	// entry index prefix.
	subs := []UpstreamSubscription{
		{Collection: "users", StartFrom: StartFromLatest, OnUpstreamDelete: UpstreamDeleteCascade},
	}
	err := validateUpstreamSubscriptions(subs, nil, profileDev)
	if err == nil || !strings.Contains(err.Error(), "entry[0]") {
		t.Errorf("expected entry-indexed shape violation, got %v", err)
	}
}

// ─── requiredPermissionOf / buildPublicRouteSet ──────────────────────────────

func TestRequiredPermissionOf_RawAndSpecBranches(t *testing.T) {
	raw := openapi.Operation{Raw: &openapi.RawSpec{RequiredPermission: "users:write"}}
	if got := requiredPermissionOf(raw); got != "users:write" {
		t.Errorf("raw branch = %q, want users:write", got)
	}
	spec := openapi.Operation{Spec: openapi.RouteSpec{RequiredPermission: "orders:read"}}
	if got := requiredPermissionOf(spec); got != "orders:read" {
		t.Errorf("spec branch = %q, want orders:read", got)
	}
}

func TestBuildPublicRouteSet_NormalizesAndSkipsMalformed(t *testing.T) {
	set := buildPublicRouteSet([]string{
		"get /health",     // lowercase method normalized to upper
		"POST /login",     // already upper
		"/missing-method", // malformed (single token) → skipped
		"a b c",           // malformed (three tokens) → skipped
	})
	if _, ok := set["GET /health"]; !ok {
		t.Error("GET /health must be present (method upper-cased)")
	}
	if _, ok := set["POST /login"]; !ok {
		t.Error("POST /login must be present")
	}
	if len(set) != 2 {
		t.Errorf("malformed entries must be skipped, set = %v", set)
	}
}
