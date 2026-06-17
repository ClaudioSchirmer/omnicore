package httpclient

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// helper: spin up an in-process Redis fake bound to a t.Cleanup.
func newRedisFake(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	m, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

// --- redisCache round-trip ----------------------------------------------

func TestRedisCache_SetGet_RoundTrip(t *testing.T) {
	m := newRedisFake(t)
	r, err := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	if err != nil {
		t.Fatalf("newRedisCache: %v", err)
	}
	entry := &CacheEntry{
		Body:          []byte(`{"v":"x"}`),
		Headers:       map[string][]string{"Content-Type": {"application/json"}},
		Status:        200,
		ContentType:   "application/json",
		ContentLength: 9,
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	ctx := context.Background()
	if err := r.Set(ctx, "k", entry); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := r.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("expected hit")
	}
	if string(got.Body) != string(entry.Body) || got.Status != entry.Status || got.ContentType != entry.ContentType {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, entry)
	}
	if got.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("headers lost in round-trip: %v", got.Headers)
	}
}

func TestRedisCache_Miss_ReturnsNoError(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	got, ok, err := r.Get(context.Background(), "missing")
	if err != nil {
		t.Errorf("miss should not error: %v", err)
	}
	if ok || got != nil {
		t.Errorf("miss should be (nil, false, nil); got (%v, %v)", got, ok)
	}
}

func TestRedisCache_TTLApplied(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	entry := &CacheEntry{
		Body:      []byte("v"),
		Status:    200,
		ExpiresAt: time.Now().Add(50 * time.Millisecond),
	}
	ctx := context.Background()
	if err := r.Set(ctx, "k", entry); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// miniredis manages time via FastForward; advance past the TTL.
	m.FastForward(time.Second)
	_, ok, _ := r.Get(ctx, "k")
	if ok {
		t.Error("expected TTL-expired miss")
	}
}

func TestRedisCache_AlreadyExpired_NotStored(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	// ExpiresAt in the past → Set returns nil and does not write.
	entry := &CacheEntry{Body: []byte("v"), ExpiresAt: time.Now().Add(-time.Second)}
	if err := r.Set(context.Background(), "k", entry); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, ok, _ := r.Get(context.Background(), "k"); ok {
		t.Error("already-expired entry should not have been stored")
	}
}

func TestRedisCache_KeyPrefix_AppliedOnRead(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr(), KeyPrefix: "svc-A"})
	entry := &CacheEntry{Body: []byte("v"), Status: 200, ExpiresAt: time.Now().Add(time.Minute)}
	_ = r.Set(context.Background(), "k", entry)
	// Read directly with the un-prefixed key through a second backend without prefix
	// — should miss; the prefix scopes the namespace.
	r2, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	if _, ok, _ := r2.Get(context.Background(), "k"); ok {
		t.Error("un-prefixed read should not hit prefixed write")
	}
	if _, ok, _ := r.Get(context.Background(), "k"); !ok {
		t.Error("prefixed read should hit prefixed write")
	}
}

// --- failMode -----------------------------------------------------------

func TestRedisCache_FailOpen_SwallowsTransportError(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr(), FailMode: "open", TimeoutMs: 10})
	// Kill miniredis to force a transport error.
	m.Close()
	got, ok, err := r.Get(context.Background(), "k")
	if err != nil {
		t.Errorf("failOpen Get should swallow error; got %v", err)
	}
	if ok || got != nil {
		t.Error("failOpen Get on transport error should return (nil, false, nil)")
	}
	setErr := r.Set(context.Background(), "k", &CacheEntry{Body: []byte("v"), ExpiresAt: time.Now().Add(time.Minute)})
	if setErr != nil {
		t.Errorf("failOpen Set should swallow error; got %v", setErr)
	}
}

func TestRedisCache_FailClosed_PropagatesTransportError(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr(), FailMode: "closed", TimeoutMs: 10})
	m.Close()
	_, _, err := r.Get(context.Background(), "k")
	if err == nil {
		t.Error("failClosed Get should propagate transport error")
	}
	setErr := r.Set(context.Background(), "k", &CacheEntry{Body: []byte("v"), ExpiresAt: time.Now().Add(time.Minute)})
	if setErr == nil {
		t.Error("failClosed Set should propagate transport error")
	}
}

// --- newRedisCache constructor validation ------------------------------

func TestNewRedisCache_RejectsEmptyAddr(t *testing.T) {
	_, err := newRedisCache(&RedisCacheConfig{})
	if err == nil {
		t.Error("empty addr should error")
	}
}

func TestNewRedisCache_RejectsInvalidFailMode(t *testing.T) {
	m := newRedisFake(t)
	_, err := newRedisCache(&RedisCacheConfig{Addr: m.Addr(), FailMode: "panic"})
	if err == nil {
		t.Error("invalid failMode should error")
	}
}

func TestRedisCache_CorruptValue_TreatedAsMiss(t *testing.T) {
	m := newRedisFake(t)
	r, _ := newRedisCache(&RedisCacheConfig{Addr: m.Addr()})
	// Write garbage directly through miniredis so the decode fails.
	if err := m.Set("k", "not-json-{"); err != nil {
		t.Fatalf("miniredis.Set: %v", err)
	}
	got, ok, err := r.Get(context.Background(), "k")
	if err != nil {
		t.Errorf("decode error should be swallowed as miss; got %v", err)
	}
	if ok || got != nil {
		t.Error("decode failure should produce a miss, not a hit")
	}
}

// --- New() cascade matrix ----------------------------------------------

func newCacheCfg(store string, redis *RedisCacheConfig) *Config {
	return &Config{
		Defaults: Defaults{
			Cache: &CacheDefaults{Store: store, Redis: redis},
		},
		Services: map[string]ServiceConfig{
			"svc": {
				BaseURL:   "https://example.com",
				Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x", Cache: &EndpointCacheConfig{TTL: Duration(time.Minute)}}},
			},
		},
	}
}

// fakeCache satisfies Cache for the conflict-matrix tests.
type fakeCache struct{}

func (fakeCache) Get(_ context.Context, _ string) (*CacheEntry, bool, error) { return nil, false, nil }
func (fakeCache) Set(_ context.Context, _ string, _ *CacheEntry) error       { return nil }

func TestNew_Memory_NoInjection_OK(t *testing.T) {
	c, err := New(newCacheCfg("memory", nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.cacheStore.(*memoryCache); !ok {
		t.Errorf("expected *memoryCache, got %T", c.cacheStore)
	}
}

func TestNew_Memory_WithInjection_Panics(t *testing.T) {
	_, err := New(newCacheCfg("memory", nil), WithCacheStore(fakeCache{}))
	if err == nil || !contains(err.Error(), "store: custom") {
		t.Errorf("expected store-mismatch error; got %v", err)
	}
}

func TestNew_Redis_NoInjection_OK(t *testing.T) {
	m := newRedisFake(t)
	c, err := New(newCacheCfg("redis", &RedisCacheConfig{Addr: m.Addr()}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.cacheStore.(*redisCache); !ok {
		t.Errorf("expected *redisCache, got %T", c.cacheStore)
	}
}

func TestNew_Redis_WithInjection_Panics(t *testing.T) {
	m := newRedisFake(t)
	_, err := New(newCacheCfg("redis", &RedisCacheConfig{Addr: m.Addr()}), WithCacheStore(fakeCache{}))
	if err == nil || !contains(err.Error(), "store: custom") {
		t.Errorf("expected store-mismatch error; got %v", err)
	}
}

func TestNew_Custom_NoInjection_Panics(t *testing.T) {
	_, err := New(newCacheCfg("custom", nil))
	if err == nil || !contains(err.Error(), "WithCacheStore") {
		t.Errorf("expected WithCacheStore-required error; got %v", err)
	}
}

func TestNew_Custom_WithInjection_OK(t *testing.T) {
	c, err := New(newCacheCfg("custom", nil), WithCacheStore(fakeCache{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := c.cacheStore.(fakeCache); !ok {
		t.Errorf("expected fakeCache, got %T", c.cacheStore)
	}
}

func TestNew_Injection_NoEndpointCacheBlock_Panics(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{Cache: &CacheDefaults{Store: "custom"}},
		Services: map[string]ServiceConfig{
			"svc": {BaseURL: "https://example.com", Endpoints: map[string]EndpointConfig{"e": {Method: "GET", Path: "/x"}}},
		},
	}
	_, err := New(cfg, WithCacheStore(fakeCache{}))
	if err == nil || !contains(err.Error(), "no endpoint declares a cache: block") {
		t.Errorf("expected unused-store error; got %v", err)
	}
}

func TestNew_RedisStoreNoBlock_Panics(t *testing.T) {
	cfg := newCacheCfg("redis", nil)
	_, err := New(cfg)
	if err == nil {
		t.Error("redis store without redis block should error")
	}
}

// --- helpers ------------------------------------------------------------

func contains(haystack, needle string) bool {
	// avoid pulling strings package; keep test deps lean
	return len(haystack) >= len(needle) && (haystack == needle ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ Cache = (*redisCache)(nil)
var _ Cache = (*memoryCache)(nil)
