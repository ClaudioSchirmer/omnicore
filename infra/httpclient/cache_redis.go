package httpclient

import (
	"context"
	"encoding/json"
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

// redisCache is the Redis-backed Cache implementation selected via
// defaults.cache.store: redis. Connection pooling is managed by the
// underlying *redis.Client. JSON is the on-wire format (CacheEntry is
// primitive-only; Body becomes a base64 string).
//
// The backend resolves the configured failMode internally: open swallows
// transport errors + slog.Warn + returns (nil, false, nil) on Get / nil
// on Set; closed propagates the error and lets cacheMiddleware abort the
// call. Logical misses (redis.Nil) are NOT errors — they always return
// (nil, false, nil) regardless of failMode.
type redisCache struct {
	client    *redis.Client
	keyPrefix string
	timeout   time.Duration
	failOpen  bool
	logger    *slog.Logger
}

// redisCacheEntryEnvelope is the on-wire shape persisted to Redis. The
// struct mirrors CacheEntry verbatim; JSON tags are explicit so a future
// rename of a CacheEntry field never silently breaks deserialization of
// already-stored entries.
type redisCacheEntryEnvelope struct {
	Body          []byte    `json:"body"`
	Headers       map[string][]string `json:"headers"`
	Status        int       `json:"status"`
	ContentType   string    `json:"contentType"`
	ContentLength int64     `json:"contentLength"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// newRedisCache constructs a redisCache from the YAML sub-block. The
// configuration shape has already been validated by validateRedisCacheConfig
// at boot; this constructor only translates the value into runtime state.
//
// Connection establishment is lazy — go-redis dials on the first command,
// so a Redis outage at boot does not break HttpClient construction. The
// first Get/Set against a down Redis surfaces the transport error through
// failMode.
func newRedisCache(cfg *RedisCacheConfig) (*redisCache, error) {
	if cfg == nil {
		return nil, fmt.Errorf("httpclient: newRedisCache requires a RedisCacheConfig")
	}
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		return nil, fmt.Errorf("httpclient: redis cache requires a non-empty addr")
	}
	timeout := frameworkRedisDefaultTimeout
	if cfg.TimeoutMs > 0 {
		timeout = time.Duration(cfg.TimeoutMs) * time.Millisecond
	}
	failOpen := true
	switch strings.ToLower(strings.TrimSpace(cfg.FailMode)) {
	case "", frameworkRedisDefaultFailMode:
		failOpen = true
	case "closed":
		failOpen = false
	default:
		// Validate would have rejected this; defensive only.
		return nil, fmt.Errorf("httpclient: redis cache failMode %q is not valid (open | closed)", cfg.FailMode)
	}
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
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

// Get looks up the entry under key. Logical miss (redis.Nil) is always
// (nil, false, nil). Transport errors honor the configured failMode.
func (r *redisCache) Get(ctx context.Context, key string) (*CacheEntry, bool, error) {
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
		return r.handleTransportError("get", key, err)
	}
	var env redisCacheEntryEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// Corrupt entry — treat as miss and log. Not a transport error;
		// failMode does not apply. The next Set will overwrite it.
		r.logger.Warn("httpclient.cache.redis.decode.error",
			slog.String("key", key),
			slog.String("error", err.Error()))
		return nil, false, nil
	}
	entry := &CacheEntry{
		Body:          env.Body,
		Headers:       env.Headers,
		Status:        env.Status,
		ContentType:   env.ContentType,
		ContentLength: env.ContentLength,
		ExpiresAt:     env.ExpiresAt,
	}
	return entry, true, nil
}

// Set persists entry under key with TTL derived from entry.ExpiresAt.
// Entries already expired at write time are dropped (no error). Transport
// errors honor the configured failMode.
func (r *redisCache) Set(ctx context.Context, key string, entry *CacheEntry) error {
	if r == nil || r.client == nil || entry == nil {
		return nil
	}
	ttl := time.Until(entry.ExpiresAt)
	if ttl <= 0 {
		return nil
	}
	env := redisCacheEntryEnvelope{
		Body:          entry.Body,
		Headers:       entry.Headers,
		Status:        entry.Status,
		ContentType:   entry.ContentType,
		ContentLength: entry.ContentLength,
		ExpiresAt:     entry.ExpiresAt,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		// CacheEntry only carries primitives + http.Header → json.Marshal
		// cannot fail in practice. Defensive: propagate the error so the
		// middleware does not silently lose the write.
		return fmt.Errorf("httpclient: redis cache encode: %w", err)
	}
	opCtx, cancel := r.withTimeout(ctx)
	defer cancel()
	if err := r.client.Set(opCtx, r.fullKey(key), raw, ttl).Err(); err != nil {
		_, _, e := r.handleTransportError("set", key, err)
		return e
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

// handleTransportError applies the failMode policy. Returns (nil, false,
// nil) under fail-open (the middleware proceeds to upstream) or the
// error verbatim under fail-closed (the middleware aborts). The slog
// emission fires regardless so operators always see the underlying
// failure.
func (r *redisCache) handleTransportError(op, key string, err error) (*CacheEntry, bool, error) {
	r.logger.Warn("httpclient.cache.redis.transport.error",
		slog.String("op", op),
		slog.String("key", key),
		slog.String("error", err.Error()),
		slog.Bool("failOpen", r.failOpen))
	if r.failOpen {
		return nil, false, nil
	}
	return nil, false, err
}
