package query

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/db/core"
)

// An empty cache (or nil resolver) resolves every name to its bare collection.
func TestViewResolver_Active_BareWhenEmpty(t *testing.T) {
	r := NewViewResolver(nil)
	if got := r.Active("users_view").String(); got != "users_view" {
		t.Errorf("Active = %q, want users_view (bare)", got)
	}
}

// Refresh loads the registry pointers; Active then returns the loaded active
// slot, while a name absent from the registry still resolves to bare.
func TestViewResolver_Refresh_LoadsActivePointer(t *testing.T) {
	active := "users_view__1"
	r := NewViewResolver(newFakeEngine(&fakeQuerier{queryFn: pointerRows("users_view", &active, nil)}))
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := r.Active("users_view").String(); got != "users_view__1" {
		t.Errorf("Active after refresh = %q, want users_view__1", got)
	}
	if got := r.Active("other").String(); got != "other" {
		t.Errorf("Active(unknown) = %q, want other (bare)", got)
	}
}

// Shadow is the inactive slot: __0 from bare, then alternating off the loaded
// active pointer.
func TestViewResolver_Shadow_ThreeState(t *testing.T) {
	if got := NewViewResolver(nil).Shadow("v").String(); got != "v__0" {
		t.Errorf("Shadow(bare) = %q, want v__0", got)
	}
	for _, tc := range []struct{ active, wantShadow string }{
		{"v__0", "v__1"},
		{"v__1", "v__0"},
	} {
		active := tc.active
		r := NewViewResolver(newFakeEngine(&fakeQuerier{queryFn: pointerRows("v", &active, nil)}))
		if err := r.Refresh(context.Background()); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if got := r.Shadow("v").String(); got != tc.wantShadow {
			t.Errorf("active %q → Shadow = %q, want %q", tc.active, got, tc.wantShadow)
		}
	}
}

func TestViewResolver_Refresh_NilEngIsNoop(t *testing.T) {
	if err := NewViewResolver(nil).Refresh(context.Background()); err != nil {
		t.Fatalf("nil-eng Refresh must be a no-op, got %v", err)
	}
}

func TestViewResolver_Refresh_QueryError(t *testing.T) {
	r := NewViewResolver(newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (core.Rows, error) { return nil, errFake },
	}))
	if err := r.Refresh(context.Background()); err == nil {
		t.Fatal("expected Refresh to surface the query error")
	}
}

// A component wired without a resolver (nil receiver) resolves to identity for
// Active and to __0 for Shadow, so pre-blue-green paths behave unchanged.
func TestViewResolver_NilSafe(t *testing.T) {
	var r *ViewResolver
	if got := r.Active("x").String(); got != "x" {
		t.Errorf("nil Active = %q, want x", got)
	}
	if got := r.Shadow("x").String(); got != "x__0" {
		t.Errorf("nil Shadow = %q, want x__0", got)
	}
}

// NewViewResolverWithLease honours a positive lease and falls back to the
// default for a non-positive one (the mongo.rebuild.pointerLeaseSeconds knob).
func TestNewViewResolverWithLease(t *testing.T) {
	if got := NewViewResolverWithLease(nil, 3*time.Second).lease; got != 3*time.Second {
		t.Errorf("lease = %v, want 3s", got)
	}
	if got := NewViewResolverWithLease(nil, 0).lease; got != defaultResolverLease {
		t.Errorf("zero lease must fall back to the default, got %v", got)
	}
	if got := NewViewResolverWithLease(nil, -time.Second).lease; got != defaultResolverLease {
		t.Errorf("negative lease must fall back to the default, got %v", got)
	}
}

// ShadowActive reports (shadow, true) only while a rebuild is recorded.
func TestViewResolver_ShadowActive(t *testing.T) {
	if _, on := NewViewResolver(nil).ShadowActive("v"); on {
		t.Error("empty resolver must report no active rebuild")
	}
	shadow := "v__0"
	r := NewViewResolver(newFakeEngine(&fakeQuerier{queryFn: pointerRows("v", nil, &shadow)}))
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	got, on := r.ShadowActive("v")
	if !on || got.String() != "v__0" {
		t.Errorf("ShadowActive = (%q, %v), want (v__0, true)", got.String(), on)
	}
	// nil receiver → no active rebuild.
	var nilr *ViewResolver
	if _, on := nilr.ShadowActive("v"); on {
		t.Error("nil resolver must report no active rebuild")
	}
}

// EnsureFresh re-reads only when the cache is older than the lease, and surfaces
// a re-read failure so the caller can stop consuming.
func TestViewResolver_EnsureFresh(t *testing.T) {
	if err := NewViewResolver(nil).EnsureFresh(context.Background()); err != nil {
		t.Fatalf("nil-eng EnsureFresh must be a no-op, got %v", err)
	}

	var calls int
	q := &fakeQuerier{queryFn: func(string, []any) (core.Rows, error) { calls++; return &fakeRows{}, nil }}
	r := NewViewResolver(newFakeEngine(q))
	if err := r.Refresh(context.Background()); err != nil { // calls == 1, lastRefresh = now
		t.Fatalf("Refresh: %v", err)
	}
	if err := r.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("fresh EnsureFresh: %v", err)
	}
	if calls != 1 {
		t.Errorf("fresh cache must not re-read, calls = %d", calls)
	}
	r.lease = -time.Second // force staleness
	if err := r.EnsureFresh(context.Background()); err != nil {
		t.Fatalf("stale EnsureFresh: %v", err)
	}
	if calls != 2 {
		t.Errorf("stale cache must re-read, calls = %d", calls)
	}
}

func TestViewResolver_EnsureFresh_RefreshErrorSurfaces(t *testing.T) {
	r := NewViewResolver(newFakeEngine(&fakeQuerier{
		queryFn: func(string, []any) (core.Rows, error) { return nil, errFake },
	}))
	r.lease = -time.Second // stale → forces the failing re-read
	if err := r.EnsureFresh(context.Background()); err == nil {
		t.Fatal("stale EnsureFresh with a failing re-read must surface the error")
	}
}

// pointerRows returns a queryFn yielding one omnicore_mongo_views row with the
// given name and active/shadow pointers (nil = SQL NULL).
func pointerRows(name string, active, shadow *string) func(string, []any) (core.Rows, error) {
	return func(string, []any) (core.Rows, error) {
		return &fakeRows{rows: 1, scan: func(_ int, dest []any) error {
			*dest[0].(*string) = name
			*dest[1].(**string) = active
			*dest[2].(**string) = shadow
			return nil
		}}, nil
	}
}

// ViewNameOf inverts PhysicalCollectionNames for every view name that does not
// itself end in a slot suffix — which the naming scheme cannot disambiguate, and
// which ViewNameOf documents as its limit. The DB-per-service abort tells an
// operator which omnicore_mongo_views row to delete from a collection name alone,
// and that row is keyed by the view.
func TestViewNameOf_InvertsEveryPhysicalName(t *testing.T) {
	for _, view := range []string{"users", "users_rel", "a__b", "users_v2"} {
		for _, physical := range PhysicalCollectionNames(view) {
			if got := ViewNameOf(physical); got != view {
				t.Errorf("ViewNameOf(%q) = %q, want %q", physical, got, view)
			}
		}
	}
}

// A collection the framework never named comes back unchanged — there is no
// suffix to strip, and inventing one would send the operator to a row that does
// not exist.
func TestViewNameOf_LeavesAnUnslottedNameAlone(t *testing.T) {
	if got := ViewNameOf("legacy_report"); got != "legacy_report" {
		t.Errorf("ViewNameOf(%q) = %q, want it unchanged", "legacy_report", got)
	}
}

// The scheme is not injective, and the helper says so rather than guessing well:
// a view NAMED "x__0" is indistinguishable from view "x"'s first slot. Pinned so
// the limitation is a decision on record, not a surprise — the answer only ever
// becomes a suggestion in a message a human reads.
func TestViewNameOf_IsNotInjectiveOnASlotShapedName(t *testing.T) {
	if got := ViewNameOf("x__0"); got != "x" {
		t.Errorf("ViewNameOf(%q) = %q — the documented limit is that it strips the suffix", "x__0", got)
	}
}

// ─── the reserved slot suffixes ──────────────────────────────────────────────

// The suffixes are how the framework ADDRESSES a view's two physical
// collections, so a name ending in one is a collision waiting to happen: view
// "users__0" would own a bare collection indistinguishable from view "users"'s
// first slot. Every consequence of that is silent — the DB-per-service guard
// whitelists both, a rebuild of "users" provisions over it, and ViewNameOf points
// an operator at the wrong registry row — so the name is refused at declaration.
func TestReservedNameSuffixProblem_RefusesASlotSuffix(t *testing.T) {
	for _, name := range []string{"users__0", "users__1", "a__0", "shared_base_view__1"} {
		why := ReservedNameSuffixProblem(name)
		if why == "" {
			t.Errorf("%q ends in a reserved slot suffix and must be refused", name)
			continue
		}
		// The message has to teach the rule, not just deny: it names both
		// suffixes, shows what the base name would have owned, and suggests a fix.
		for _, want := range []string{"__0", "__1", "reserves", "Choose a name"} {
			if !strings.Contains(why, want) {
				t.Errorf("%q: message is missing %q — got %q", name, want, why)
			}
		}
	}
}

// Everything else passes, including names that merely CONTAIN the suffix or end
// in a single underscore-zero — the rule is about the trailing slot form only.
func TestReservedNameSuffixProblem_AllowsEverythingElse(t *testing.T) {
	for _, name := range []string{"users", "users_rel", "a__b", "users__0_archive", "users_0", "users__2"} {
		if why := ReservedNameSuffixProblem(name); why != "" {
			t.Errorf("%q is a legal name, got refused: %s", name, why)
		}
	}
}
