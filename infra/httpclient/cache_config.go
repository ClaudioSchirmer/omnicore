package httpclient

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Framework defaults applied when the cache: block is present but a field
// is omitted. Picked for production sanity — 5 minute TTL, 10k entries
// covers most read paths without unbounded growth.
const (
	frameworkCacheDefaultTTL        = 5 * time.Minute
	frameworkCacheMaxEntries        = 10000
	frameworkCacheHonorCacheControl = true
)

// CacheDefaults is the defaults-level cache: block. Endpoint-level cache
// blocks describe per-endpoint deviations (TTL, varyOn); the global
// defaults control whether cache runs at all, the store sizing, and the
// backend selection.
type CacheDefaults struct {
	Enabled           *bool    `yaml:"enabled"`
	DefaultTTL        Duration `yaml:"defaultTTL"`
	MaxEntries        int      `yaml:"maxEntries"`
	HonorCacheControl *bool    `yaml:"honorCacheControl"`

	// Store selects the cache backend. Accepted values:
	//   - "" / "memory" → in-process LRU+TTL (default; pod-local)
	//   - "redis"       → external Redis (declarative; shared across pods)
	//   - "custom"      → consumer-injected Cache via WithCacheStore at New()
	//
	// The choice is binding: a value of "custom" without WithCacheStore is
	// a boot panic; any other value combined with WithCacheStore is also a
	// boot panic. The YAML declares intent; mismatched wiring fails loudly.
	Store string `yaml:"store"`

	// Redis is required when Store == "redis" and rejected otherwise. The
	// sub-block carries the connection coordinates and the policy knobs
	// the redisCache resolves at construction time.
	Redis *RedisCacheConfig `yaml:"redis,omitempty"`
}

// RedisCacheConfig is the defaults.cache.redis: sub-block consumed when
// Store == "redis". Every field except Addr accepts a zero value and
// falls back to a documented framework default.
type RedisCacheConfig struct {
	// Addr is the host:port of the Redis server. Required.
	Addr string `yaml:"addr"`
	// Password is the AUTH password (optional; empty == no AUTH).
	Password string `yaml:"password"`
	// DB is the Redis logical database index (0 by default). Use a
	// dedicated DB per service to avoid cross-service key collisions even
	// when the same Redis instance hosts multiple services.
	DB int `yaml:"db"`
	// KeyPrefix is prepended to every key written by this backend. Use it
	// when sharing a single DB across multiple services or environments.
	// Empty by default. The default cache key already starts with the
	// service name so cross-service collisions inside one prefix are
	// already structurally impossible; the prefix is for namespacing
	// across deployments.
	KeyPrefix string `yaml:"keyPrefix"`
	// FailMode selects the behavior on Redis transport errors:
	//   - "" / "open"  → swallow + slog.Warn + proceed to upstream (default)
	//   - "closed"     → propagate the error; the call fails at the cache layer
	// The choice is per-Redis-backend; the middleware honors whatever the
	// backend returns.
	FailMode string `yaml:"failMode"`
	// TimeoutMs caps every Get/Set against Redis. 0 falls back to 100ms.
	// Use small values — Redis is the fast path; long blocking defeats
	// the cache.
	TimeoutMs int `yaml:"timeoutMs"`
}

// EndpointCacheConfig is the endpoint-level cache: block. Presence enables
// caching for that endpoint (subject to defaults.cache.enabled).
type EndpointCacheConfig struct {
	TTL    Duration `yaml:"ttl"`
	VaryOn []string `yaml:"varyOn"`
}

// cachePolicy is the resolved runtime shape consumed by cacheMiddleware. It
// is built once at New per endpoint; the request path performs no string
// parsing or map lookups beyond cache key computation.
type cachePolicy struct {
	enabled           bool
	ttl               time.Duration
	varyHeaders       []string
	varyQueries       []string
	honorCacheControl bool
}

// varyOnEntryRE matches the supported varyOn vocabulary today: header:Name
// or query:Name. Name allows letters, digits, dash and underscore — enough
// for any standard HTTP header or query parameter without leaving room for
// surprises (no spaces, no JSONPath, no glob).
var varyOnEntryRE = regexp.MustCompile(`^(header|query):([A-Za-z][A-Za-z0-9_-]*)$`)

// resolveCachePolicy merges defaults and endpoint cache config. The
// endpoint block enables caching for that endpoint; the defaults block
// gates whether the runtime layer participates at all.
//
// Cascade rules:
//   - When defaults.enabled is false → policy disabled regardless of endpoint
//   - When endpoint has no cache block → policy disabled for that endpoint
//   - Endpoint.TTL overrides defaults.DefaultTTL; defaults.DefaultTTL falls
//     back to the framework constant when unset.
//
// honorCacheControl flows from defaults; endpoint cannot override (the
// design keeps that as a service-level knob, expressed via defaults).
func resolveCachePolicy(defaults *CacheDefaults, endpoint *EndpointCacheConfig) cachePolicy {
	if endpoint == nil {
		return cachePolicy{}
	}
	defaultsEnabled := true
	if defaults != nil && defaults.Enabled != nil {
		defaultsEnabled = *defaults.Enabled
	}
	if !defaultsEnabled {
		return cachePolicy{}
	}
	ttl := endpoint.TTL.ToTime()
	if ttl == 0 && defaults != nil {
		ttl = defaults.DefaultTTL.ToTime()
	}
	if ttl == 0 {
		ttl = frameworkCacheDefaultTTL
	}
	honor := frameworkCacheHonorCacheControl
	if defaults != nil && defaults.HonorCacheControl != nil {
		honor = *defaults.HonorCacheControl
	}
	p := cachePolicy{enabled: true, ttl: ttl, honorCacheControl: honor}
	for _, raw := range endpoint.VaryOn {
		m := varyOnEntryRE.FindStringSubmatch(strings.TrimSpace(raw))
		if m == nil {
			continue
		}
		switch m[1] {
		case "header":
			p.varyHeaders = append(p.varyHeaders, m[2])
		case "query":
			p.varyQueries = append(p.varyQueries, m[2])
		}
	}
	return p
}

// resolveMaxEntries returns the cache size cap to use when constructing
// the in-memory store. Defaults to the framework constant; explicit
// values in the YAML override.
func resolveMaxEntries(defaults *CacheDefaults) int {
	if defaults == nil || defaults.MaxEntries == 0 {
		return frameworkCacheMaxEntries
	}
	return defaults.MaxEntries
}

// resolveCacheStore picks the effective Cache implementation given the
// YAML cache block and a possibly-injected store from WithCacheStore.
//
// Cascade (the rule fixed in the design round before this change shipped):
//
//   - YAML store="" or "memory" + injected==nil → newMemoryCache (default)
//   - YAML store="" or "memory" + injected!=nil → error (declare store: custom)
//   - YAML store="redis" + injected==nil        → newRedisCache from YAML
//   - YAML store="redis" + injected!=nil        → error (declare store: custom)
//   - YAML store="custom" + injected==nil       → error (WithCacheStore required)
//   - YAML store="custom" + injected!=nil       → injected store
//
// Mismatched wiring fails loudly at New() rather than producing a silently
// wrong store choice at runtime.
func resolveCacheStore(defaults *CacheDefaults, injected Cache) (Cache, error) {
	store := ""
	if defaults != nil {
		store = strings.ToLower(strings.TrimSpace(defaults.Store))
	}
	if store == "" {
		store = "memory"
	}
	switch store {
	case "memory":
		if injected != nil {
			return nil, fmt.Errorf("httpclient: WithCacheStore requires defaults.cache.store: custom (got %q) — declare the intent in the YAML or remove the Wire injection", defaults.Store)
		}
		return newMemoryCache(resolveMaxEntries(defaults)), nil
	case "redis":
		if injected != nil {
			return nil, fmt.Errorf("httpclient: WithCacheStore requires defaults.cache.store: custom (got \"redis\") — declare the intent in the YAML or remove the Wire injection")
		}
		if defaults == nil || defaults.Redis == nil {
			return nil, fmt.Errorf("httpclient: defaults.cache.store: redis requires a defaults.cache.redis block")
		}
		return newRedisCache(defaults.Redis)
	case "custom":
		if injected == nil {
			return nil, fmt.Errorf("httpclient: defaults.cache.store: custom requires WithCacheStore at New()")
		}
		return injected, nil
	default:
		// Validate already rejected unknown values; reachable only if
		// resolveCacheStore is called without Validate (tests).
		return nil, fmt.Errorf("httpclient: defaults.cache.store: %q is not a valid backend (memory | redis | custom)", defaults.Store)
	}
}

// validateCacheDefaults runs schema checks on the defaults-level block and
// returns a list of error strings the global validator joins together.
func validateCacheDefaults(prefix string, cfg *CacheDefaults) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.DefaultTTL < 0 {
		errs = append(errs, fmt.Sprintf("%s.defaultTTL: must be non-negative", prefix))
	}
	if cfg.MaxEntries < 0 {
		errs = append(errs, fmt.Sprintf("%s.maxEntries: must be non-negative (0 disables the cap)", prefix))
	}
	store := strings.ToLower(strings.TrimSpace(cfg.Store))
	switch store {
	case "", "memory", "redis", "custom":
		// ok
	default:
		errs = append(errs, fmt.Sprintf("%s.store: %q is not a valid backend (memory | redis | custom)", prefix, cfg.Store))
	}
	if store == "redis" && cfg.Redis == nil {
		errs = append(errs, fmt.Sprintf("%s.redis: required when store: redis", prefix))
	}
	if store != "redis" && cfg.Redis != nil {
		errs = append(errs, fmt.Sprintf("%s.redis: only allowed when store: redis", prefix))
	}
	if cfg.Redis != nil {
		errs = append(errs, validateRedisCacheConfig(prefix+".redis", cfg.Redis)...)
	}
	return errs
}

// validateRedisCacheConfig runs schema checks on the redis sub-block.
// Addr is the only mandatory field; FailMode is gated to the documented
// values; TimeoutMs / DB / KeyPrefix accept zero (fallback) without error.
func validateRedisCacheConfig(prefix string, cfg *RedisCacheConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if strings.TrimSpace(cfg.Addr) == "" {
		errs = append(errs, fmt.Sprintf("%s.addr: required", prefix))
	}
	if cfg.DB < 0 {
		errs = append(errs, fmt.Sprintf("%s.db: must be non-negative", prefix))
	}
	if cfg.TimeoutMs < 0 {
		errs = append(errs, fmt.Sprintf("%s.timeoutMs: must be non-negative", prefix))
	}
	switch strings.ToLower(strings.TrimSpace(cfg.FailMode)) {
	case "", "open", "closed":
		// ok
	default:
		errs = append(errs, fmt.Sprintf("%s.failMode: %q is not valid (open | closed)", prefix, cfg.FailMode))
	}
	return errs
}

// validateEndpointCache runs schema checks on an endpoint-level cache block.
// The method check enforces "GET / HEAD only" so a misconfigured POST
// endpoint with caching fails the boot rather than silently never caching.
func validateEndpointCache(prefix, method string, cfg *EndpointCacheConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.TTL < 0 {
		errs = append(errs, fmt.Sprintf("%s.ttl: must be non-negative", prefix))
	}
	if up := strings.ToUpper(method); up != "GET" && up != "HEAD" {
		errs = append(errs, fmt.Sprintf("%s: cache is only supported on GET and HEAD endpoints (current method: %s)", prefix, up))
	}
	for i, raw := range cfg.VaryOn {
		v := strings.TrimSpace(raw)
		if !varyOnEntryRE.MatchString(v) {
			errs = append(errs, fmt.Sprintf("%s.varyOn[%d]: %q does not match header:Name or query:Name", prefix, i, raw))
		}
	}
	return errs
}
