package cache

import (
	"context"
	"testing"
	"time"
)

// NewMemory option branches: zero falls back to the framework default; a
// negative value is clamped to 0 (unbounded growth, no eviction).
func TestNewMemory_OptionBranches(t *testing.T) {
	def := NewMemory(0).(*memoryCache)
	if def.maxLen != frameworkMemoryMaxEntries {
		t.Errorf("maxEntries==0 must fall back to default, got %d", def.maxLen)
	}
	clamped := NewMemory(-5).(*memoryCache)
	if clamped.maxLen != 0 {
		t.Errorf("negative maxEntries must clamp to 0, got %d", clamped.maxLen)
	}
	// maxLen==0 disables eviction: many sets, all retained.
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		_ = clamped.Set(ctx, string(rune('A'+i%26))+string(rune('0'+i/26)), []byte("v"), time.Minute)
	}
	if clamped.len() != 50 {
		t.Errorf("maxLen==0 should never evict, got len=%d", clamped.len())
	}
}

// Set on an existing key updates value+ttl in place (the entries[key] branch)
// and moves it to the LRU front.
func TestMemory_SetUpdatesExistingKey(t *testing.T) {
	c := NewMemory(10).(*memoryCache)
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v1"), time.Minute)
	_ = c.Set(ctx, "k", []byte("v2"), time.Minute) // update in place
	if c.len() != 1 {
		t.Fatalf("update must not add a second entry, len=%d", c.len())
	}
	got, ok, _ := c.Get(ctx, "k")
	if !ok || string(got) != "v2" {
		t.Errorf("expected updated value v2, got (%q,%v)", got, ok)
	}
}

// RedisConfig.Validate accumulates every shape error.
func TestRedisConfig_Validate_AllErrors(t *testing.T) {
	if err := (*RedisConfig)(nil).Validate(); err == nil {
		t.Error("nil config must be rejected")
	}
	err := (&RedisConfig{Addr: "", DB: -1, TimeoutMs: -5, FailMode: "bogus"}).Validate()
	if err == nil {
		t.Fatal("expected accumulated validation errors")
	}
	for _, want := range []string{"addr", "db", "timeoutMs", "failMode"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	// Valid permutations pass.
	for _, fm := range []string{"", "open", "closed", "OPEN", "Closed"} {
		if e := (&RedisConfig{Addr: "h:1", FailMode: fm}).Validate(); e != nil {
			t.Errorf("failMode %q should be valid, got %v", fm, e)
		}
	}
}

// withTimeout's no-deadline branch: timeout<=0 returns a plain cancel context.
func TestRedisCache_WithTimeout_NoDeadline(t *testing.T) {
	r := &redisCache{timeout: 0}
	ctx, cancel := r.withTimeout(context.Background())
	defer cancel()
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		t.Error("timeout<=0 must yield a context with no deadline")
	}
	// positive timeout yields a deadline
	r2 := &redisCache{timeout: 50 * time.Millisecond}
	ctx2, cancel2 := r2.withTimeout(context.Background())
	defer cancel2()
	if _, hasDeadline := ctx2.Deadline(); !hasDeadline {
		t.Error("positive timeout must yield a deadline")
	}
}

// Nil-receiver / nil-client guards on the redis backend.
func TestRedisCache_NilGuards(t *testing.T) {
	var r *redisCache // nil receiver
	ctx := context.Background()
	if v, ok, err := r.Get(ctx, "k"); v != nil || ok || err != nil {
		t.Errorf("nil Get = (%v,%v,%v)", v, ok, err)
	}
	if err := r.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Errorf("nil Set = %v", err)
	}
	if err := r.Delete(ctx, "k"); err != nil {
		t.Errorf("nil Delete = %v", err)
	}
	// negative ttl rejected before the nil-client guard
	if err := r.Set(ctx, "k", []byte("v"), -time.Second); err != ErrInvalidTTL {
		t.Errorf("negative ttl must be ErrInvalidTTL, got %v", err)
	}
}

// fullKey with and without prefix.
func TestRedisCache_FullKey(t *testing.T) {
	if got := (&redisCache{}).fullKey("k"); got != "k" {
		t.Errorf("empty prefix should pass key verbatim, got %q", got)
	}
	if got := (&redisCache{keyPrefix: "p"}).fullKey("k"); got != "p:k" {
		t.Errorf("prefixed key, got %q", got)
	}
}

// contains is a tiny substring helper local to this file.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
