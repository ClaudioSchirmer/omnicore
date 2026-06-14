package httpclient

import (
	"testing"
	"time"
)

func TestApplyInvokeOptions_Defaults(t *testing.T) {
	cfg := applyInvokeOptions(nil)
	if len(cfg.extraHeaders) != 0 || len(cfg.extraQuery) != 0 {
		t.Errorf("defaults should have empty extras: %+v", cfg)
	}
	if len(cfg.acceptableStatus) != 0 {
		t.Errorf("defaults should have empty acceptableStatus")
	}
	if cfg.timeout != 0 {
		t.Errorf("defaults should have zero timeout")
	}
}

func TestWithExtraHeader_LastWins(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithExtraHeader("X-Tenant", "a"),
		WithExtraHeader("X-Tenant", "b"),
	})
	if cfg.extraHeaders["X-Tenant"] != "b" {
		t.Errorf("X-Tenant = %q, want b", cfg.extraHeaders["X-Tenant"])
	}
}

func TestWithExtraQuery_AppendsValues(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithExtraQuery("tag", "a"),
		WithExtraQuery("tag", "b"),
	})
	got := cfg.extraQuery["tag"]
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("tag = %v, want [a b]", got)
	}
}

func TestWithConfig_AcceptableStatusAccumulatesUnion(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{AcceptableStatus: []int{404}}),
		WithConfig(CallConfig{AcceptableStatus: []int{409, 410}}),
	})
	for _, code := range []int{404, 409, 410} {
		if _, ok := cfg.acceptableStatus[code]; !ok {
			t.Errorf("missing acceptable code %d", code)
		}
	}
}

func TestWithConfig_TimeoutSetsAndCumulates(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{Timeout: 5 * time.Second}),
	})
	if cfg.timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", cfg.timeout)
	}

	// Zero Timeout preserves prior value (zero = inherit, not clear).
	cfg = applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{Timeout: 5 * time.Second}),
		WithConfig(CallConfig{Timeout: 0}),
	})
	if cfg.timeout != 5*time.Second {
		t.Errorf("zero timeout should NOT clear prior (got %v, want 5s)", cfg.timeout)
	}

	// Later non-zero replaces.
	cfg = applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{Timeout: 5 * time.Second}),
		WithConfig(CallConfig{Timeout: 10 * time.Second}),
	})
	if cfg.timeout != 10*time.Second {
		t.Errorf("later WithConfig should replace (got %v, want 10s)", cfg.timeout)
	}
}

func TestWithConfig_MultipleFieldsAtOnce(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{
		WithConfig(CallConfig{
			BaseURL:        "https://override",
			Method:         "PUT",
			Path:           "/v2/users",
			Timeout:        7 * time.Second,
			AuthProvider:   "alt",
			NoCache:        true,
			CacheKey:       "k",
			IdempotencyKey: "i",
		}),
	})
	if cfg.baseURLOverride != "https://override" || cfg.methodOverride != "PUT" ||
		cfg.pathOverride != "/v2/users" || cfg.timeout != 7*time.Second ||
		cfg.authOverride != "alt" || !cfg.noCache ||
		cfg.cacheKey != "k" || cfg.idempotencyKey != "i" {
		t.Errorf("WithConfig fields not propagated: %+v", cfg)
	}
}

func TestApplyInvokeOptions_NilSafe(t *testing.T) {
	cfg := applyInvokeOptions([]InvokeOption{nil, WithExtraHeader("X", "1"), nil})
	if cfg.extraHeaders["X"] != "1" {
		t.Errorf("nil options should be ignored")
	}
}
