package bootstrap

import (
	"runtime"
	"testing"
)

// TestApplyDefaults_Pool_UnsetGetsFrameworkDefault asserts an omitted
// relational.pool resolves to the framework default: maxOpenConns = max(4,
// NumCPU) (mirrors pgxpool, bounds MySQL), maxIdleConns = the same (keep the pool
// warm), connMaxLifetimeSeconds = 0 (engine default).
func TestApplyDefaults_Pool_UnsetGetsFrameworkDefault(t *testing.T) {
	var c Config
	c.applyDefaults()

	want := FrameworkDefaultMaxOpenConns()
	if c.Relational.Pool.MaxOpenConns == nil || *c.Relational.Pool.MaxOpenConns != want {
		t.Errorf("MaxOpenConns = %v, want %d", c.Relational.Pool.MaxOpenConns, want)
	}
	if c.Relational.Pool.MaxIdleConns == nil || *c.Relational.Pool.MaxIdleConns != want {
		t.Errorf("MaxIdleConns = %v, want %d (defaults to MaxOpenConns)", c.Relational.Pool.MaxIdleConns, want)
	}
	if c.Relational.Pool.ConnMaxLifetimeSeconds == nil || *c.Relational.Pool.ConnMaxLifetimeSeconds != 0 {
		t.Errorf("ConnMaxLifetimeSeconds = %v, want 0", c.Relational.Pool.ConnMaxLifetimeSeconds)
	}
}

// TestFrameworkDefaultMaxOpenConns asserts the floor of 4 and that it never
// undershoots NumCPU — the invariant the default relies on.
func TestFrameworkDefaultMaxOpenConns(t *testing.T) {
	got := FrameworkDefaultMaxOpenConns()
	if got < 4 {
		t.Errorf("FrameworkDefaultMaxOpenConns() = %d, want >= 4", got)
	}
	if n := runtime.NumCPU(); n > 4 && got != n {
		t.Errorf("FrameworkDefaultMaxOpenConns() = %d, want %d (NumCPU)", got, n)
	}
}

// TestApplyDefaults_Pool_ExplicitValuesPreserved asserts operator-set values
// survive applyDefaults untouched.
func TestApplyDefaults_Pool_ExplicitValuesPreserved(t *testing.T) {
	open, idle, life := 25, 10, 300
	var c Config
	c.Relational.Pool.MaxOpenConns = &open
	c.Relational.Pool.MaxIdleConns = &idle
	c.Relational.Pool.ConnMaxLifetimeSeconds = &life
	c.applyDefaults()

	if *c.Relational.Pool.MaxOpenConns != 25 {
		t.Errorf("MaxOpenConns = %d, want 25", *c.Relational.Pool.MaxOpenConns)
	}
	if *c.Relational.Pool.MaxIdleConns != 10 {
		t.Errorf("MaxIdleConns = %d, want 10", *c.Relational.Pool.MaxIdleConns)
	}
	if *c.Relational.Pool.ConnMaxLifetimeSeconds != 300 {
		t.Errorf("ConnMaxLifetimeSeconds = %d, want 300", *c.Relational.Pool.ConnMaxLifetimeSeconds)
	}
}

// TestApplyDefaults_Pool_ExplicitUnlimitedOpen asserts an explicit maxOpenConns=0
// (opt into an unlimited pool) is preserved, and that maxIdleConns then falls back
// to the framework default rather than 0 (which would force zero idle churn).
func TestApplyDefaults_Pool_ExplicitUnlimitedOpen(t *testing.T) {
	zero := 0
	var c Config
	c.Relational.Pool.MaxOpenConns = &zero
	c.applyDefaults()

	if *c.Relational.Pool.MaxOpenConns != 0 {
		t.Errorf("MaxOpenConns = %d, want 0 (unlimited opt-in preserved)", *c.Relational.Pool.MaxOpenConns)
	}
	if got := *c.Relational.Pool.MaxIdleConns; got != FrameworkDefaultMaxOpenConns() {
		t.Errorf("MaxIdleConns = %d, want %d (fallback when open is unlimited)", got, FrameworkDefaultMaxOpenConns())
	}
}
