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
	got := filterForeignCollections([]string{"users", "orders"}, views)
	if len(got) != 0 {
		t.Errorf("got %v, want [] when every observed collection is declared", got)
	}
}

// A view's own blue-green slots (<view>__0 / <view>__1) are NOT foreign — the
// guard must recognize a view living in a slot, or it aborts a non-dev boot on the
// view's own active/shadow collection.
func TestFilterForeignCollections_SlotsAreNotForeign(t *testing.T) {
	views := []*query.ViewDefinition{query.View("gadgets")}
	got := filterForeignCollections([]string{"gadgets", "gadgets__0", "gadgets__1"}, views)
	if len(got) != 0 {
		t.Errorf("got %v, want [] — a view's own slots must not be flagged foreign", got)
	}
	// A genuinely foreign slot (of an undeclared view) is still flagged.
	got = filterForeignCollections([]string{"gadgets__0", "orders__0"}, views)
	if len(got) != 1 || got[0] != "orders__0" {
		t.Errorf("got %v, want [orders__0] (an undeclared view's slot is still foreign)", got)
	}
}

func TestFilterForeignCollections_RegistryIsFrameworkOwned(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{"users", RegistryCollectionName}, views)
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
	got := filterForeignCollections([]string{"users", query.ProjectionStateCollectionName}, views)
	if len(got) != 0 {
		t.Errorf("got %v, want [] (projection-state registry must be framework-owned)", got)
	}
}

func TestFilterForeignCollections_SystemNamespaceIgnored(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{
		"users", "system.profile", "system.namespaces", "system.indexes",
	}, views)
	if len(got) != 0 {
		t.Errorf("got %v, want [] (system.* must never be flagged)", got)
	}
}

func TestFilterForeignCollections_OneForeign(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections([]string{"users", "legacy_audit"}, views)
	if len(got) != 1 || got[0] != "legacy_audit" {
		t.Errorf("got %v, want [legacy_audit]", got)
	}
}

func TestFilterForeignCollections_MultipleForeignSortedDeterministic(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections(
		[]string{"zeta", "alpha", "users", "mike"}, views)
	want := []string{"alpha", "mike", "zeta"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v (must be deterministic / sorted)", got, want)
	}
}

func TestFilterForeignCollections_NoDeclaredViews_AllObservedAreForeign(t *testing.T) {
	// A write-only service that ships zero ViewDefinitions but somehow
	// has Mongo collections present (residue, manual creation): every
	// non-system / non-framework collection is foreign.
	got := filterForeignCollections([]string{"a", "b", "system.x", RegistryCollectionName}, nil)
	if !equalSlices(got, []string{"a", "b"}) {
		t.Errorf("got %v, want [a b]", got)
	}
}

func TestFilterForeignCollections_EmptyObserved(t *testing.T) {
	views := []*query.ViewDefinition{query.View("users")}
	got := filterForeignCollections(nil, views)
	if len(got) != 0 {
		t.Errorf("got %v, want []", got)
	}
}

// ─── decideForeignResponse ───────────────────────────────────────────────────

func TestDecideForeignResponse_NoneToReport(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "prd", nil)
	if err != nil {
		t.Errorf("got %v, want nil when foreign list is empty", err)
	}
}

func TestDecideForeignResponse_DevDowngradesToWarn(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", DevProfile, []string{"legacy"})
	if err != nil {
		t.Errorf("got %v, want nil under dev profile (warn-only)", err)
	}
}

func TestDecideForeignResponse_NonDevAborts(t *testing.T) {
	err := decideForeignResponse(context.Background(), "svc", "prd", []string{"legacy"})
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
	err := decideForeignResponse(context.Background(), "svc", "prd",
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
		err := decideForeignResponse(context.Background(), "svc", profile, []string{"orphan"})
		if err == nil {
			t.Errorf("profile %q: got nil, want abort", profile)
		}
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
