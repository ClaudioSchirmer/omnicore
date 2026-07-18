package query

import "testing"

// Phase 1 resolution is identity: every logical name resolves to the bare name,
// via both an engine-backed resolver and a nil (unwired) one.
func TestViewResolver_Active_Identity(t *testing.T) {
	r := NewViewResolver(nil)
	if got := r.Active("users_view").String(); got != "users_view" {
		t.Errorf("Active = %q, want users_view", got)
	}
}

func TestViewResolver_Shadow_Identity(t *testing.T) {
	r := NewViewResolver(nil)
	if got := r.Shadow("users_view").String(); got != "users_view" {
		t.Errorf("Shadow = %q, want users_view", got)
	}
}

// A component wired without a resolver (nil receiver) must resolve to identity,
// so pre-blue-green paths behave exactly as before.
func TestViewResolver_NilSafe(t *testing.T) {
	var r *ViewResolver
	if got := r.Active("x").String(); got != "x" {
		t.Errorf("nil Active = %q, want x", got)
	}
	if got := r.Shadow("x").String(); got != "x" {
		t.Errorf("nil Shadow = %q, want x", got)
	}
}
