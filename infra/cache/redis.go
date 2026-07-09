package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// frameworkRedisDefaults centralizes the values resolveRedisCacheConfig
// falls back to when the YAML omits the corresponding field.
const (
	frameworkRedisDefaultTimeout  = 100 * time.Millisecond
	frameworkRedisDefaultFailMode = "open"
)

// RedisConfig is the operator-facing shape of the `cache.redis:`
// YAML block, also exported so consumer code can construct a Redis
// cache directly from Go (tests, alternative wiring) without going
// through the YAML loader.
//
// Addr is the only mandatory field; every other knob falls back to a
// documented framework default.
type RedisConfig struct {
	// Addr is the host:port of the Redis server. Required.
	Addr string `yaml:"addr"`
	// Password is the AUTH password (optional; empty == no AUTH).
	Password string `yaml:"password"`
	// DB is the Redis logical database index (0 by default). Use a
	// dedicated DB per service to avoid cross-service key collisions
	// even when the same Redis instance hosts multiple services.
	DB int `yaml:"db"`
	// KeyPrefix is prepended to every key written by this backend.
	// Use it when sharing a single DB across multiple services or
	// environments. Empty by default.
	KeyPrefix string `yaml:"keyPrefix"`
	// FailMode selects the behavior on Redis transport errors:
	//   - "" / "open"  → swallow + slog.Warn + return miss / nil (default)
	//   - "closed"     → propagate the error; the call fails at the cache layer
	FailMode string `yaml:"failMode"`
	// TimeoutMs caps every Get / Set / Delete against Redis. 0 falls
	// back to 100ms. Use small values — Redis is the fast path; long
	// blocking defeats the cache.
	TimeoutMs int `yaml:"timeoutMs"`
}

// Validate runs the same shape checks the bootstrap loader runs against
// a yaml-sourced RedisConfig — exposed here so consumers constructing a
// Cache programmatically get the same diagnostics.
func (c *RedisConfig) Validate() error {
	if c == nil {
		return fmt.Errorf("cache: redis config is required")
	}
	var errs []string
	if strings.TrimSpace(c.Addr) == "" {
		errs = append(errs, "addr: required")
	}
	if c.DB < 0 {
		errs = append(errs, "db: must be non-negative")
	}
	if c.TimeoutMs < 0 {
		errs = append(errs, "timeoutMs: must be non-negative")
	}
	switch strings.ToLower(strings.TrimSpace(c.FailMode)) {
	case "", "open", "closed":
	default:
		errs = append(errs, fmt.Sprintf("failMode: %q is not valid (open | closed)", c.FailMode))
	}
	if len(errs) > 0 {
		return fmt.Errorf("cache.redis: %s", strings.Join(errs, "; "))
	}
	return nil
}

// NewRedis constructs a Redis-backed Cache. Configuration is validated
// upfront. The Redis connection is lazy — go-redis dials on the first
// command, so an unreachable Redis at boot does not break New(). The
// first Get / Set surfaces the transport error through FailMode.
func NewRedis(cfg *RedisConfig) (Cache, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	timeout := frameworkRedisDefaultTimeout
	if cfg.TimeoutMs > 0 {
		timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	failOpen := !strings.EqualFold(strings.TrimSpace(cfg.FailMode), "closed")

	client := redis.NewClient(&redis.Options{
		Addr:         strings.TrimSpace(cfg.Addr),
		Password:     cfg.Password,
		DB:           cfg.DB,
		DialTimeout:  timeout,
		ReadTimeout:  timeout,
		WriteTimeout: timeout,
	})
	return &redisCache{
		client:    client,
		keyPrefix: cfg.KeyPrefix,
		timeout:   timeout,
		failOpen:  failOpen,
		logger:    slog.Default(),
	}, nil
}

// redisCache implements Cache against go-redis. Logical misses
// (redis.Nil) always return (nil, false, nil) regardless of failMode.
// Transport errors honor failMode: open swallows + logs + returns miss
// / nil; closed propagates the error verbatim.
type redisCache struct {
	client    *redis.Client
	keyPrefix string
	timeout   time.Duration
	failOpen  bool
	logger    *slog.Logger
}

func (r *redisCache) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if r == nil || r.client == nil {
		return nil, false, nil
	}
	opCtx, cancel := r.withTimeout(ctx)
	defer cancel()
	raw, err := r.client.Get(opCtx, r.fullKey(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, r.handleTransport("get", key, err)
	}
	return raw, true, nil
}

func (r *redisCache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return ErrInvalidTTL
	}
	if r == nil || r.client == nil {
		return nil
	}
	opCtx, cancel := r.withTimeout(ctx)
	defer cancel()
	// ttl == 0 means no expiration in go-redis (Redis SET without EX/PX).
	if err := r.client.Set(opCtx, r.fullKey(key), value, ttl).Err(); err != nil {
		return r.handleTransport("set", key, err)
	}
	return nil
}

func (r *redisCache) Delete(ctx context.Context, key string) error {
	if r == nil || r.client == nil {
		return nil
	}
	opCtx, cancel := r.withTimeout(ctx)
	defer cancel()
	if err := r.client.Del(opCtx, r.fullKey(key)).Err(); err != nil {
		return r.handleTransport("delete", key, err)
	}
	return nil
}

// withTimeout derives a child context capped at r.timeout. The parent's
// own deadline (request cancellation, per-call timeout) wins when it is
// shorter — the redis op never outlasts the calling request.
func (r *redisCache) withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	if r.timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, r.timeout)
}

// fullKey applies the configured prefix. Empty prefix returns key
// verbatim — no separator inserted.
func (r *redisCache) fullKey(key string) string {
	if r.keyPrefix == "" {
		return key
	}
	return r.keyPrefix + ":" + key
}

// handleTransport applies the failMode policy. Returns nil under fail-
// open (the middleware proceeds to upstream) or the error verbatim
// under fail-closed (the middleware aborts). The slog emission fires
// regardless so operators always see the underlying failure.
func (r *redisCache) handleTransport(op, key string, err error) error {
	r.logger.Warn("cache.redis.transport.error",
		slog.String("op", op),
		slog.String("key", key),
		slog.String("error", err.Error()),
		slog.Bool("failOpen", r.failOpen))
	if r.failOpen {
		return nil
	}
	return err
}
