package mongo

import (
	"context"
	"strings"
	"testing"

	"github.com/ClaudioSchirmer/omnicore/infra/db/query"
)

// The DB-per-service guard's pure decision logic (which collections are
// foreign + what the framework does about them in each profile) is
// covered without an active Mongo connection. The Mongo round-trips
// (upsertServiceMarker, listCollectionNames, listOtherServices) are
// exercised by the omnicore-example-users E2E suite once Phase D wires
// CheckServiceRegistry into bootstrap.Run.

// ─── filterForeignCollections ────────────────────────────────────────────────

func TestFilterForeignCollections_AllDeclared_Empty(t *testing.T) {
	views := []*query.ViewDefinition{
		query.View("users"),
		query.View("orders"),
	}
	got := filterForeignCollections([]string{"users", "orders"}, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want [] when every observed collection is declared", got)
	}
}

// A view's own blue-green slots (<view>__0 / <view>__1) are NOT foreign — the
// guard must recognize a view living in a slot, or it aborts a non-dev boot on the
// view's own active/shadow collection.
func TestFilterForeignCollections_SlotsAreNotForeign(t *testing.T) {
	views := []*query.ViewDefinition{query.View("gadgets")}
	got := filterForeignCollections([]string{"gadgets", "gadgets__0", "gadgets__1"}, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want [] — a view's own slots must not be flagged foreign", got)
	}
	// A genuinely foreign slot (of an undeclared view) is still flagged.
	got = filterForeignCollections([]string{"gadgets__0", "orders__0"}, views, nil)
	if len(got) != 1 || got[0] != "orders__0" {
		t.Errorf("got %v, want [orders__0] (an undeclared view's slot is still foreign)", got)
	}
}

func TestFilterForeignCollections_RegistryIsFrameworkOwned(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{"users", RegistryCollectionName}, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want [] (registry collection must be framework-owned)", got)
	}
}

// The projection-state registry (base-revision records + tombstones) is materialized
// by the SyncEngine itself on its first start — the guard must recognize it,
// or every boot AFTER the first against the same database aborts on the
// framework's own collection.
func TestFilterForeignCollections_ProjectionStateIsFrameworkOwned(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{"users", query.ProjectionStateCollectionName}, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want [] (projection-state registry must be framework-owned)", got)
	}
}

func TestFilterForeignCollections_SystemNamespaceIgnored(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{
		"users", "system.profile", "system.namespaces", "system.indexes",
	}, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want [] (system.* must never be flagged)", got)
	}
}

func TestFilterForeignCollections_OneForeign(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{"users", "legacy_audit"}, views, nil)
	if len(got) != 1 || got[0] != "legacy_audit" {
		t.Errorf("got %v, want [legacy_audit]", got)
	}
}

func TestFilterForeignCollections_MultipleForeignSortedDeterministic(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections(
		[]string{"zeta", "alpha", "users", "mike"}, views, nil)
	want := []string{"alpha", "mike", "zeta"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v (must be deterministic / sorted)", got, want)
	}
}

func TestFilterForeignCollections_NoDeclaredViews_AllObservedAreForeign(t *testing.T) {
	// A write-only service that ships zero ViewDefinitions but somehow
	// has Mongo collections present (residue, manual creation): every
	// non-system / non-framework collection is foreign.
	got := filterForeignCollections([]string{"a", "b", "system.x", RegistryCollectionName}, nil, nil)
	if !equalSlices(got, []string{"a", "b"}) {
		t.Errorf("got %v, want [a b]", got)
	}
}

func TestFilterForeignCollections_EmptyObserved(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections(nil, views, nil)
	if len(got) != 0 {
		t.Errorf("got %v, want []", got)
	}
}

// ─── decideForeignResponse ───────────────────────────────────────────────────

func TestDecideForeignResponse_NoneToReport(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", "prd", nil)
	if err != nil {
		t.Errorf("got %v, want nil when foreign list is empty", err)
	}
}

func TestDecideForeignResponse_DevDowngradesToWarn(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", DevProfile, []string{"legacy"})
	if err != nil {
		t.Errorf("got %v, want nil under dev profile (warn-only)", err)
	}
}

func TestDecideForeignResponse_NonDevAborts(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", "prd", []string{"legacy"})
	if err == nil {
		t.Fatal("got nil, want abort under non-dev profile")
	}
	msg := err.Error()
	if !strings.Contains(msg, "svc") {
		t.Errorf("err does not name service: %q", msg)
	}
	if !strings.Contains(msg, "legacy") {
		t.Errorf("err does not name foreign collection: %q", msg)
	}
}

func TestDecideForeignResponse_AbortListsEveryForeign(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", "prd",
		[]string{"alpha", "mike", "zeta"})
	if err == nil {
		t.Fatal("expected error")
	}
	for _, name := range []string{"alpha", "mike", "zeta"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("err missing %q: %s", name, err.Error())
		}
	}
}

func TestDecideForeignResponse_CustomProfileTreatedAsNonDev(t *testing.T) {
	// Any profile other than "dev" must abort — even "qa", "stg",
	// "prd-pem", etc. The canonical posture mirrors auth.mode=disabled
	// (only dev is loose).
	for _, profile := range []string{"prd", "qa", "stg", "prd-pem", "prd-external"} {
		err := decideForeignResponse(context.Background(), "svc", "svcdb", profile, []string{"orphan"})
		if err == nil {
			t.Errorf("profile %q: got nil, want abort", profile)
		}
	}
}

// The abort is the last thing an operator reads before the process exits, and
// the cause it names is one they cannot see from the database: the view's
// DECLARATION left the build. It has to carry the database, every collection,
// that explanation, and both statements to run — the drop and the relational
// bookkeeping row, which is keyed by the VIEW name and is the half that gets
// forgotten.
func TestDecideForeignResponse_AbortIsActionable(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", "prd", []string{"users_rel__1"})
	if err == nil {
		t.Fatal("expected an abort")
	}
	msg := err.Error()
	for _, want := range []string{
		`database "svcdb"`, // WHERE the collection lives
		"use svcdb",        // the mongosh context to run in
		`db.getCollection("users_rel__1").drop()`,                         // the exact drop
		"DECLARATION IS NO LONGER IN THIS BUILD",                          // WHY it is orphaned
		"query.RelationalView(...)",                                       // the conversion that causes it
		"DELETE FROM omnicore_mongo_views WHERE view_name = 'users_rel';", // the other half
		"mongo.database", // the give-it-its-own-DB escape
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("abort message is missing %q:\n%s", want, msg)
		}
	}
}

// The bookkeeping row is keyed by the VIEW, and a view owns two blue-green
// slots. Emitting the same DELETE twice would read as two separate problems.
func TestDecideForeignResponse_OneDeletePerView(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "svcdb", "prd",
		[]string{"users_rel", "users_rel__0", "users_rel__1"})
	if err == nil {
		t.Fatal("expected an abort")
	}
	if n := strings.Count(err.Error(), "DELETE FROM omnicore_mongo_views"); n != 1 {
		t.Errorf("three slots of ONE view must yield one DELETE, got %d:\n%s", n, err.Error())
	}
	// All three collections still have to be dropped individually.
	if n := strings.Count(err.Error(), ".drop()"); n != 3 {
		t.Errorf("every collection must be dropped, got %d drops", n)
	}
}

// ─── frameworkOwnedCollections ───────────────────────────────────────────────

func TestFrameworkOwnedCollections_IncludesRegistry(t *testing.T) {
	got := frameworkOwnedCollections()
	found := false
	for _, n := range got {
		if n == RegistryCollectionName {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("frameworkOwnedCollections = %v, must include RegistryCollectionName (%q)", got, RegistryCollectionName)
	}
}

// ─── helper ──────────────────────────────────────────────────────────────────

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// BulkApplyProjection's empty-batch guard needs no driver: the bulk write API
// rejects a zero-length model slice, so the no-op return is load-bearing.
func TestBulkApplyProjection_EmptyBatchIsNoOp(t *testing.T) {
	m := &MongoDB{}
	if err := m.BulkApplyProjection(context.Background(), query.PhysicalCollection{}, nil); err != nil {
		t.Fatalf("empty batch must be a no-op, got %v", err)
	}
}

// ─── upstream-subscription mirrors are claimed, not foreign ──────────────────

// The local collection an upstreamSubscriptions entry materializes is written by
// the framework into THIS service's database, on this service's behalf. Before it
// was passed to the guard, it was reported as another tenant's residue: a warn in
// dev and an ABORT everywhere else, so a service declaring a subscription could
// not boot outside dev.
func TestFilterForeignCollections_UpstreamMirrorIsClaimed(t *testing.T) {
	views := []*query.ViewDefinition{query.View("orders")}
	got := filterForeignCollections(
		[]string{"orders", "upstream_gadgets"}, views, []string{"upstream_gadgets"})
	if len(got) != 0 {
		t.Errorf("got %v, want [] — a declared upstream mirror is this service's own collection", got)
	}
}

// An upstream mirror has no omnicore_mongo_views row, so the resolver never points
// it at a blue-green slot and the subscriber only ever writes the bare name.
// Whitelisting slots it cannot own would hide real residue.
func TestFilterForeignCollections_UpstreamMirrorClaimsNoSlots(t *testing.T) {
	got := filterForeignCollections(
		[]string{"upstream_gadgets", "upstream_gadgets__0"}, nil, []string{"upstream_gadgets"})
	if len(got) != 1 || got[0] != "upstream_gadgets__0" {
		t.Errorf("got %v, want [upstream_gadgets__0] — a mirror claims its bare name only", got)
	}
}

// A subscription whose collection is empty claims nothing (the boot guard rejects
// the declaration itself; this only proves the filter never whitelists "").
func TestFilterForeignCollections_UpstreamEmptyNameClaimsNothing(t *testing.T) {
	got := filterForeignCollections([]string{"residue"}, nil, []string{""})
	if len(got) != 1 || got[0] != "residue" {
		t.Errorf("got %v, want [residue]", got)
	}
}
