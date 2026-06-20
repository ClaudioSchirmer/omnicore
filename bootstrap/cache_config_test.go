package bootstrap

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// stubCache is a no-op cache.Cache used to exercise the custom-store
// injection branches without a real backend.
type stubCache struct{}

func (stubCache) Get(_ context.Context, _ string) ([]byte, bool, error) { return nil, false, nil }
func (stubCache) Set(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (stubCache) Delete(_ context.Context, _ string) error { return nil }

// ─── validateCache ───────────────────────────────────────────────────────────

func TestValidateCache_NilIsNoError(t *testing.T) {
	if errs := validateCache(nil); errs != nil {
		t.Errorf("nil cfg must yield no errors, got %v", errs)
	}
}

func TestValidateCache_AcceptsValidStores(t *testing.T) {
	for _, store := range []string{"", "memory", "redis", "custom"} {
		cfg := &CacheConfig{Store: store}
		if store == "redis" {
			cfg.Redis = &cache.RedisConfig{Addr: "localhost:6379"}
		}
		if errs := validateCache(cfg); len(errs) != 0 {
			t.Errorf("store=%q must validate, got %v", store, errs)
		}
	}
}

func TestValidateCache_RejectsUnknownStore(t *testing.T) {
	errs := validateCache(&CacheConfig{Store: "memcached"})
	if !containsSubstr(errs, "not a valid backend") {
		t.Errorf("expected invalid-backend error, got %v", errs)
	}
}

func TestValidateCache_RejectsNegativeMaxEntries(t *testing.T) {
	errs := validateCache(&CacheConfig{Store: "memory", MaxEntries: -1})
	if !containsSubstr(errs, "maxEntries") {
		t.Errorf("expected maxEntries error, got %v", errs)
	}
}

func TestValidateCache_RedisRequiredWhenStoreRedis(t *testing.T) {
	errs := validateCache(&CacheConfig{Store: "redis"})
	if !containsSubstr(errs, "cache.redis: required") {
		t.Errorf("expected required-redis error, got %v", errs)
	}
}

func TestValidateCache_RedisRejectedWhenStoreNotRedis(t *testing.T) {
	errs := validateCache(&CacheConfig{Store: "memory", Redis: &cache.RedisConfig{Addr: "x:1"}})
	if !containsSubstr(errs, "only allowed when cache.store: redis") {
		t.Errorf("expected redis-only-when error, got %v", errs)
	}
}

func TestValidateCache_PropagatesSharedErrors(t *testing.T) {
	errs := validateCache(&CacheConfig{
		Store:  "memory",
		Shared: &CacheSharedConfig{Store: "memory"},
	})
	if !containsSubstr(errs, "shared.store: memory is not allowed") {
		t.Errorf("expected shared memory rejection, got %v", errs)
	}
}

// ─── validateCacheShared ─────────────────────────────────────────────────────

func TestValidateCacheShared(t *testing.T) {
	cases := []struct {
		name string
		cfg  *CacheSharedConfig
		want string // substring; "" means no error expected
	}{
		{"memory rejected", &CacheSharedConfig{Store: "memory"}, "cannot be shared"},
		{"empty store required", &CacheSharedConfig{Store: ""}, "required (redis | custom)"},
		{"unknown store", &CacheSharedConfig{Store: "etcd"}, "not a valid backend"},
		{"redis without sub-block", &CacheSharedConfig{Store: "redis"}, "shared.redis: required"},
		{"redis ok", &CacheSharedConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: "x:1"}}, ""},
		{"custom ok", &CacheSharedConfig{Store: "custom"}, ""},
		{"redis allowed only on redis", &CacheSharedConfig{Store: "custom", Redis: &cache.RedisConfig{Addr: "x:1"}}, "only allowed when cache.shared.store: redis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateCacheShared(c.cfg)
			if c.want == "" {
				if len(errs) != 0 {
					t.Errorf("expected no error, got %v", errs)
				}
				return
			}
			if !containsSubstr(errs, c.want) {
				t.Errorf("expected error containing %q, got %v", c.want, errs)
			}
		})
	}
}

// ─── resolveCache ────────────────────────────────────────────────────────────

func TestResolveCache_NilCfgNoInjection(t *testing.T) {
	c, err := resolveCache(nil, nil)
	if err != nil || c != nil {
		t.Errorf("nil cfg + no injection → (nil, nil), got (%v, %v)", c, err)
	}
}

func TestResolveCache_NilCfgWithInjectionIsError(t *testing.T) {
	_, err := resolveCache(nil, stubCache{})
	if err == nil || !strings.Contains(err.Error(), "block is absent") {
		t.Errorf("expected absent-block error, got %v", err)
	}
}

func TestResolveCache_MemoryBuildsInProcess(t *testing.T) {
	c, err := resolveCache(&CacheConfig{Store: "memory"}, nil)
	if err != nil || c == nil {
		t.Errorf("memory store must build a cache, got (%v, %v)", c, err)
	}
}

func TestResolveCache_MemoryWithInjectionRejected(t *testing.T) {
	_, err := resolveCache(&CacheConfig{Store: "memory"}, stubCache{})
	if err == nil || !strings.Contains(err.Error(), "custom") {
		t.Errorf("expected custom-required error, got %v", err)
	}
}

func TestResolveCache_CustomRequiresInjection(t *testing.T) {
	_, err := resolveCache(&CacheConfig{Store: "custom"}, nil)
	if err == nil || !strings.Contains(err.Error(), "requires Wiring.Cache") {
		t.Errorf("expected custom-requires-injection error, got %v", err)
	}
}

func TestResolveCache_CustomReturnsInjected(t *testing.T) {
	inj := stubCache{}
	c, err := resolveCache(&CacheConfig{Store: "custom"}, inj)
	if err != nil || c != cache.Cache(inj) {
		t.Errorf("custom must return the injected cache, got (%v, %v)", c, err)
	}
}

func TestResolveCache_RedisWithInjectionRejected(t *testing.T) {
	_, err := resolveCache(&CacheConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: "x:1"}}, stubCache{})
	if err == nil || !strings.Contains(err.Error(), "custom") {
		t.Errorf("expected custom-required error for redis+injection, got %v", err)
	}
}

// ─── resolveSharedCache ──────────────────────────────────────────────────────

func TestResolveSharedCache_NilSharedNoInjection(t *testing.T) {
	c, err := resolveSharedCache(&CacheConfig{Store: "memory"}, nil)
	if err != nil || c != nil {
		t.Errorf("no shared block + no injection → (nil, nil), got (%v, %v)", c, err)
	}
}

func TestResolveSharedCache_NilCfg(t *testing.T) {
	c, err := resolveSharedCache(nil, nil)
	if err != nil || c != nil {
		t.Errorf("nil cfg → (nil, nil), got (%v, %v)", c, err)
	}
}

func TestResolveSharedCache_NilSharedWithInjectionIsError(t *testing.T) {
	_, err := resolveSharedCache(&CacheConfig{Store: "memory"}, stubCache{})
	if err == nil || !strings.Contains(err.Error(), "shared: block is absent") {
		t.Errorf("expected absent-shared-block error, got %v", err)
	}
}

func TestResolveSharedCache_CustomRequiresInjection(t *testing.T) {
	cfg := &CacheConfig{Store: "memory", Shared: &CacheSharedConfig{Store: "custom"}}
	_, err := resolveSharedCache(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "requires Wiring.SharedCache") {
		t.Errorf("expected custom-requires-injection error, got %v", err)
	}
}

func TestResolveSharedCache_CustomReturnsInjected(t *testing.T) {
	cfg := &CacheConfig{Store: "memory", Shared: &CacheSharedConfig{Store: "custom"}}
	inj := stubCache{}
	c, err := resolveSharedCache(cfg, inj)
	if err != nil || c != cache.Cache(inj) {
		t.Errorf("custom shared must return the injected cache, got (%v, %v)", c, err)
	}
}

func TestResolveSharedCache_RedisWithInjectionRejected(t *testing.T) {
	cfg := &CacheConfig{Store: "memory", Shared: &CacheSharedConfig{Store: "redis", Redis: &cache.RedisConfig{Addr: "x:1"}}}
	_, err := resolveSharedCache(cfg, stubCache{})
	if err == nil || !strings.Contains(err.Error(), "custom") {
		t.Errorf("expected custom-required error for redis shared+injection, got %v", err)
	}
}

func TestResolveSharedCache_MemoryFallsToDefensiveError(t *testing.T) {
	// validateCacheShared rejects memory upstream; resolveSharedCache's
	// switch hits the defensive default for memory/unknown stores.
	cfg := &CacheConfig{Store: "memory", Shared: &CacheSharedConfig{Store: "memory"}}
	_, err := resolveSharedCache(cfg, nil)
	if err == nil || !strings.Contains(err.Error(), "not a valid backend for the shared cache") {
		t.Errorf("expected defensive backend error, got %v", err)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func containsSubstr(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}
