package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// --- memory backend -----------------------------------------------------

func TestMemory_SetGet(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || string(got) != "v" {
		t.Errorf("Get returned (%q, %v, %v)", got, ok, err)
	}
}

func TestMemory_TTLExpiry(t *testing.T) {
	c := NewMemory(10).(*memoryCache)
	ctx := context.Background()
	// Negative ttl rejected.
	if err := c.Set(ctx, "k", []byte("v"), -time.Second); err == nil {
		t.Error("Set with negative ttl should reject")
	}
	// Already-expired ttl via direct construction not possible (Set computes
	// the deadline from time.Now). Use a tiny positive ttl + sleep.
	_ = c.Set(ctx, "k", []byte("v"), 10*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Errorf("expired entry should not be returned")
	}
	if c.len() != 0 {
		t.Errorf("expired entry should be evicted; len=%d", c.len())
	}
}

func TestMemory_NoTTL(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 0) // 0 == no expiration
	if _, ok, _ := c.Get(ctx, "k"); !ok {
		t.Error("entry with ttl=0 should not expire")
	}
}

func TestMemory_LRUEviction(t *testing.T) {
	c := NewMemory(3).(*memoryCache)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c"} {
		_ = c.Set(ctx, k, []byte(k), time.Minute)
	}
	_, _, _ = c.Get(ctx, "a") // a most recent
	_ = c.Set(ctx, "d", []byte("d"), time.Minute)
	if _, ok, _ := c.Get(ctx, "b"); ok {
		t.Errorf("b should have been evicted (oldest after a touched)")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok, _ := c.Get(ctx, k); !ok {
			t.Errorf("expected %q to remain", k)
		}
	}
}

func TestMemory_Delete(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), time.Minute)
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Error("deleted entry should not be returned")
	}
	// Missing key is not an error.
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete on missing key should be idempotent; got %v", err)
	}
}

func TestMemory_DefensiveCopy(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	v := []byte("original")
	_ = c.Set(ctx, "k", v, time.Minute)
	// Mutate the source slice — cached value should be unaffected.
	v[0] = 'M'
	got, _, _ := c.Get(ctx, "k")
	if string(got) != "original" {
		t.Errorf("cache should defensive-copy on write; got %q", got)
	}
	// Mutate the returned slice — cached value should be unaffected.
	got[0] = 'X'
	got2, _, _ := c.Get(ctx, "k")
	if string(got2) != "original" {
		t.Errorf("cache should defensive-copy on read; got %q", got2)
	}
}

func TestMemory_Concurrent(t *testing.T) {
	c := NewMemory(100)
	done := make(chan struct{}, 20)
	for i := 0; i < 20; i++ {
		go func(i int) {
			ctx := context.Background()
			for j := 0; j < 200; j++ {
				k := string(rune('a' + i%26))
				_ = c.Set(ctx, k, []byte("x"), time.Minute)
				_, _, _ = c.Get(ctx, k)
				_ = c.Delete(ctx, k)
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < 20; i++ {
		<-done
	}
}

// --- redis backend ------------------------------------------------------

func newFakeRedis(t *testing.T) *miniredis.Miniredis {
	t.Helper()
	m, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestRedis_SetGetRoundTrip(t *testing.T) {
	m := newFakeRedis(t)
	c, err := NewRedis(&RedisConfig{Addr: m.Addr()})
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	ctx := context.Background()
	if err := c.Set(ctx, "k", []byte(`{"v":"x"}`), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := c.Get(ctx, "k")
	if err != nil || !ok || string(got) != `{"v":"x"}` {
		t.Errorf("Get returned (%q, %v, %v)", got, ok, err)
	}
}

func TestRedis_Miss(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr()})
	got, ok, err := c.Get(context.Background(), "missing")
	if err != nil || ok || got != nil {
		t.Errorf("miss should be (nil, false, nil); got (%q, %v, %v)", got, ok, err)
	}
}

func TestRedis_TTL(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr()})
	_ = c.Set(context.Background(), "k", []byte("v"), 50*time.Millisecond)
	m.FastForward(time.Second)
	if _, ok, _ := c.Get(context.Background(), "k"); ok {
		t.Error("expected TTL-expired miss")
	}
}

func TestRedis_NoTTL(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr()})
	_ = c.Set(context.Background(), "k", []byte("v"), 0)
	m.FastForward(24 * time.Hour)
	if _, ok, _ := c.Get(context.Background(), "k"); !ok {
		t.Error("ttl=0 should mean no expiration")
	}
}

func TestRedis_Delete(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr()})
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), time.Minute)
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if _, ok, _ := c.Get(ctx, "k"); ok {
		t.Error("deleted entry should not be returned")
	}
	if err := c.Delete(ctx, "k"); err != nil {
		t.Errorf("Delete on missing key should be idempotent; got %v", err)
	}
}

func TestRedis_KeyPrefix(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr(), KeyPrefix: "svc-A"})
	_ = c.Set(context.Background(), "k", []byte("v"), time.Minute)
	c2, _ := NewRedis(&RedisConfig{Addr: m.Addr()})
	if _, ok, _ := c2.Get(context.Background(), "k"); ok {
		t.Error("un-prefixed read should not hit prefixed write")
	}
	if _, ok, _ := c.Get(context.Background(), "k"); !ok {
		t.Error("prefixed read should hit prefixed write")
	}
}

func TestRedis_FailOpen(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr(), FailMode: "open", TimeoutMs: 10})
	m.Close()
	got, ok, err := c.Get(context.Background(), "k")
	if err != nil || ok || got != nil {
		t.Errorf("failOpen Get should swallow error; got (%q, %v, %v)", got, ok, err)
	}
	if err := c.Set(context.Background(), "k", []byte("v"), time.Minute); err != nil {
		t.Errorf("failOpen Set should swallow error; got %v", err)
	}
	if err := c.Delete(context.Background(), "k"); err != nil {
		t.Errorf("failOpen Delete should swallow error; got %v", err)
	}
}

func TestRedis_FailClosed(t *testing.T) {
	m := newFakeRedis(t)
	c, _ := NewRedis(&RedisConfig{Addr: m.Addr(), FailMode: "closed", TimeoutMs: 10})
	m.Close()
	if _, _, err := c.Get(context.Background(), "k"); err == nil {
		t.Error("failClosed Get should propagate error")
	}
	if err := c.Set(context.Background(), "k", []byte("v"), time.Minute); err == nil {
		t.Error("failClosed Set should propagate error")
	}
	if err := c.Delete(context.Background(), "k"); err == nil {
		t.Error("failClosed Delete should propagate error")
	}
}

func TestRedis_RejectsEmptyAddr(t *testing.T) {
	if _, err := NewRedis(&RedisConfig{}); err == nil {
		t.Error("empty addr should error")
	}
}

func TestRedis_RejectsInvalidFailMode(t *testing.T) {
	m := newFakeRedis(t)
	if _, err := NewRedis(&RedisConfig{Addr: m.Addr(), FailMode: "panic"}); err == nil {
		t.Error("invalid failMode should error")
	}
}

// --- typed JSON helpers -------------------------------------------------

type sample struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func TestGetSetJSON_RoundTrip(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	if err := SetJSON(ctx, c, "user:1", sample{Name: "Alice", Age: 30}, time.Minute); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	got, ok, err := GetJSON[sample](ctx, c, "user:1")
	if err != nil || !ok || got.Name != "Alice" || got.Age != 30 {
		t.Errorf("GetJSON returned (%+v, %v, %v)", got, ok, err)
	}
}

func TestGetJSON_Miss(t *testing.T) {
	c := NewMemory(10)
	got, ok, err := GetJSON[sample](context.Background(), c, "missing")
	if err != nil || ok || got != (sample{}) {
		t.Errorf("miss should yield (zero, false, nil); got (%+v, %v, %v)", got, ok, err)
	}
}

func TestGetJSON_DecodeFailure(t *testing.T) {
	c := NewMemory(10)
	ctx := context.Background()
	// Pre-populate with invalid JSON to force a decode failure.
	_ = c.Set(ctx, "broken", []byte("not-json-{"), time.Minute)
	_, ok, err := GetJSON[sample](ctx, c, "broken")
	if err == nil {
		t.Error("decode failure should surface as error")
	}
	if ok {
		t.Error("decode failure should not be reported as a hit")
	}
}

func TestGetJSON_NilCacheIsSafe(t *testing.T) {
	// Consumer pattern: feature with optional cache. nil Cache + helper
	// behaves as miss.
	got, ok, err := GetJSON[sample](context.Background(), nil, "k")
	if err != nil || ok || got != (sample{}) {
		t.Errorf("nil cache should be safe-miss; got (%+v, %v, %v)", got, ok, err)
	}
	if err := SetJSON(context.Background(), nil, "k", sample{}, time.Minute); err != nil {
		t.Errorf("nil cache Set should be safe-no-op; got %v", err)
	}
}

// --- interface contract -------------------------------------------------

// Compile-time checks that both implementations satisfy Cache.
var _ Cache = (*memoryCache)(nil)
var _ Cache = (*redisCache)(nil)

// Sanity: avoid the "unused" lint warning on atomic for any later
// concurrency-heavy assertion.
var _ = atomic.LoadInt32
