package query

import (
	"context"
	"testing"

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
