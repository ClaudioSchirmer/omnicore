// Package cache is the framework's generic byte-level key-value cache
// subsystem. The Cache interface is the single port consumer code, domain
// services, infrastructure adapters, and the outbound httpclient all
// consult via bootstrap.Deps.Cache.
//
// The package ships two canonical implementations — in-process LRU+TTL
// (cache.NewMemory) and Redis (cache.NewRedis) — selected via the
// top-level `cache:` block in microservice.<profile>.yaml. Consumers
// wanting a different backend (Memcached, Valkey, Hazelcast, …)
// implement the Cache interface and inject the implementation via
// bootstrap.Wiring.Cache paired with `cache.store: custom` in YAML.
//
// The interface is intentionally narrow:
//
//   - Get / Set / Delete cover every common cache flow.
//   - Has is omitted (use Get + the bool return).
//   - Clear is omitted (operator-controlled invalidation lives in the
//     backend's CLI; "wipe everything" is too sharp to expose as code).
//   - Batch ops are omitted (Mget/Mset/Pipeline can land in a later
//     revision if a real workload needs them; today every framework
//     consumer reads keys one at a time).
//
// Typed-value flows are served by the package-level helpers GetJSON /
// SetJSON which marshal through encoding/json. They sit outside the
// interface so backends don't have to think about Go types.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Cache is the byte-level key-value port. Implementations MUST be safe
// for concurrent use.
//
// Get returns (value, true, nil) on hit, (nil, false, nil) on logical
// miss, or (nil, false, err) on transport / decode failure. Backends
// MAY swallow transport errors internally and degrade to a miss when
// configured to fail-open (see RedisConfig.FailMode); the contract at
// the interface is the same in both modes — the caller branches on
// (ok, err) without caring how the backend resolved its own failure
// policy.
//
// Set persists value under key with a relative TTL. The framework
// accepts ttl == 0 as "no expiration" so backends with a native
// no-expire semantic (Redis: SET without EX/PX, memory: zero
// ExpiresAt sentinel) honor it; negative TTLs are rejected.
//
// Delete removes the entry under key. Missing keys are NOT an error —
// Delete is idempotent by design (consumers branch on "the key is
// gone now", not on "we removed something that existed").
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// ErrInvalidTTL is returned by Set when the caller passes a negative
// duration. ttl == 0 is valid (no expiration); only negatives are an
// error so the framework forces clarity at the call site rather than
// silently coercing.
var ErrInvalidTTL = errors.New("cache: ttl must be non-negative")

// GetJSON is the typed helper for the common case of caching a Go
// value. The framework marshals via encoding/json; the consumer
// supplies the type parameter so the call site stays clean of cast
// boilerplate.
//
//	type Profile struct { Name string }
//	p, ok, err := cache.GetJSON[Profile](ctx, deps.Cache, "user:42:profile")
//
// Logical misses return (zero, false, nil). Decode failures
// (corrupted entry, schema drift between the stored bytes and T)
// return (zero, false, err) so the consumer can decide whether to
// recompute or surface the failure.
func GetJSON[T any](ctx context.Context, c Cache, key string) (T, bool, error) {
	var zero T
	if c == nil {
		return zero, false, nil
	}
	raw, ok, err := c.Get(ctx, key)
	if err != nil || !ok {
		return zero, ok, err
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, false, fmt.Errorf("cache: decode %q: %w", key, err)
	}
	return out, true, nil
}

// SetJSON is the typed inverse. Marshals value via encoding/json and
// delegates to Cache.Set. Returns the underlying Set error verbatim;
// encode failures wrap the underlying json.Marshal error.
func SetJSON[T any](ctx context.Context, c Cache, key string, value T, ttl time.Duration) error {
	if c == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache: encode %q: %w", key, err)
	}
	return c.Set(ctx, key, raw, ttl)
}
