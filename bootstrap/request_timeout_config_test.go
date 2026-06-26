package bootstrap

import "testing"

// TestApplyDefaults_RequestTimeout_UnsetGetsFrameworkDefault asserts an unset
// http.requestTimeoutSeconds resolves to the framework default (30s), so a
// service that declares nothing still runs with an inbound deadline.
func TestApplyDefaults_RequestTimeout_UnsetGetsFrameworkDefault(t *testing.T) {
	var c Config
	c.applyDefaults()

	if c.HTTP.RequestTimeoutSeconds == nil {
		t.Fatal("expected RequestTimeoutSeconds to be defaulted, got nil")
	}
	if got := *c.HTTP.RequestTimeoutSeconds; got != FrameworkDefaultRequestTimeoutSeconds {
		t.Errorf("RequestTimeoutSeconds = %d, want %d", got, FrameworkDefaultRequestTimeoutSeconds)
	}
}

// TestApplyDefaults_RequestTimeout_ExplicitZeroPreserved asserts an explicit 0
// (the opt-out that disables the deadline) survives applyDefaults — the tri-state
// pointer is what distinguishes "unset" (→ default) from "disabled" (→ 0).
func TestApplyDefaults_RequestTimeout_ExplicitZeroPreserved(t *testing.T) {
	zero := 0
	var c Config
	c.HTTP.RequestTimeoutSeconds = &zero
	c.applyDefaults()

	if c.HTTP.RequestTimeoutSeconds == nil {
		t.Fatal("expected explicit RequestTimeoutSeconds to survive, got nil")
	}
	if got := *c.HTTP.RequestTimeoutSeconds; got != 0 {
		t.Errorf("RequestTimeoutSeconds = %d, want 0 (disabled)", got)
	}
}

// TestApplyDefaults_RequestTimeout_ExplicitValuePreserved asserts a positive
// explicit value is left untouched.
func TestApplyDefaults_RequestTimeout_ExplicitValuePreserved(t *testing.T) {
	v := 5
	var c Config
	c.HTTP.RequestTimeoutSeconds = &v
	c.applyDefaults()

	if got := *c.HTTP.RequestTimeoutSeconds; got != 5 {
		t.Errorf("RequestTimeoutSeconds = %d, want 5", got)
	}
}
