package bootstrap

import (
	"fmt"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
)

// CacheConfig is the top-level `cache:` block consumed by bootstrap.
// nil when the operator omits the block entirely — Deps.Cache stays nil
// and the framework's cache subsystem is dormant (httpclient cache
// middleware bypasses, GetJSON/SetJSON helpers tolerate nil).
//
// When the block is present, Store is mandatory and selects the backend
// of the SERVICE-PRIVATE cache exposed on Deps.Cache. The optional
// Shared sub-block declares a SECOND cache, exposed on
// Deps.SharedCache, scoped to cross-service reads (typically a
// dedicated Redis prefix or DB the operator coordinates across the
// team's services).
type CacheConfig struct {
	// Store selects the backend:
	//   - "memory" (default when block present) → in-process LRU+TTL
	//   - "redis"                               → cache.NewRedis
	//   - "custom"                              → Wiring.Cache required
	Store string `yaml:"store"`
	// MaxEntries caps the in-process LRU when Store == "memory".
	// Ignored otherwise. 0 falls back to the framework default (10k).
	MaxEntries int `yaml:"maxEntries"`
	// Redis is required when Store == "redis" and rejected otherwise.
	Redis *cache.RedisConfig `yaml:"redis,omitempty"`

	// Shared declares the cross-service cache exposed on
	// Deps.SharedCache. nil leaves Deps.SharedCache nil — services
	// that never publish keys outside their own scope pay zero
	// extra cost. When present, the same Store / Redis fields drive
	// its construction, with the additional rule that store=memory
	// is rejected: an in-process LRU cannot be "shared" across
	// service boundaries.
	Shared *CacheSharedConfig `yaml:"shared,omitempty"`
}

// CacheSharedConfig mirrors CacheConfig's Store/Redis fields for the
// SHARED cache. memory is intentionally rejected — sharing keys across
// services requires a backend that crosses process boundaries.
type CacheSharedConfig struct {
	Store string             `yaml:"store"`
	Redis *cache.RedisConfig `yaml:"redis,omitempty"`
}

// validateCache runs the schema invariants on the top-level cache block.
// Returns a list of error strings the global validator joins together —
// same shape every other boot validator follows so a single failure
// message lists all issues.
func validateCache(cfg *CacheConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	store := strings.ToLower(strings.TrimSpace(cfg.Store))
	switch store {
	case "", "memory", "redis", "custom":
		// ok
	default:
		errs = append(errs, fmt.Sprintf("cache.store: %q is not a valid backend (memory | redis | custom)", cfg.Store))
	}
	if cfg.MaxEntries < 0 {
		errs = append(errs, "cache.maxEntries: must be non-negative (0 falls back to the framework default)")
	}
	if store == "redis" && cfg.Redis == nil {
		errs = append(errs, "cache.redis: required when cache.store: redis")
	}
	if store != "redis" && cfg.Redis != nil {
		errs = append(errs, fmt.Sprintf("cache.redis: only allowed when cache.store: redis (got store=%q)", cfg.Store))
	}
	if cfg.Redis != nil {
		if err := cfg.Redis.Validate(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if cfg.Shared != nil {
		errs = append(errs, validateCacheShared(cfg.Shared)...)
	}
	return errs
}

func validateCacheShared(cfg *CacheSharedConfig) []string {
	var errs []string
	store := strings.ToLower(strings.TrimSpace(cfg.Store))
	switch store {
	case "memory":
		errs = append(errs, "cache.shared.store: memory is not allowed for the shared cache — an in-process LRU cannot be shared across services. Use redis or custom.")
	case "redis", "custom":
		// ok
	case "":
		errs = append(errs, "cache.shared.store: required (redis | custom)")
	default:
		errs = append(errs, fmt.Sprintf("cache.shared.store: %q is not a valid backend (redis | custom)", cfg.Store))
	}
	if store == "redis" && cfg.Redis == nil {
		errs = append(errs, "cache.shared.redis: required when cache.shared.store: redis")
	}
	if store != "redis" && cfg.Redis != nil {
		errs = append(errs, fmt.Sprintf("cache.shared.redis: only allowed when cache.shared.store: redis (got store=%q)", cfg.Store))
	}
	if cfg.Redis != nil {
		if err := cfg.Redis.Validate(); err != nil {
			errs = append(errs, "shared: "+err.Error())
		}
	}
	return errs
}

// resolveCache builds the SERVICE-PRIVATE cache from cfg + an optional
// Wire-injected instance. The conflict matrix mirrors the one we
// previously enforced on httpclient.WithCacheStore: declaring
// store: custom without Wiring.Cache, or passing Wiring.Cache with any
// other store value, aborts the boot. Returns nil when cfg is nil
// (operator declared no cache block).
func resolveCache(cfg *CacheConfig, injected cache.Cache) (cache.Cache, error) {
	if cfg == nil {
		if injected != nil {
			return nil, fmt.Errorf("bootstrap: Wiring.Cache was set but the cache: block is absent in YAML — declare cache.store: custom to enable Wire injection")
		}
		return nil, nil
	}
	store := strings.ToLower(strings.TrimSpace(cfg.Store))
	if store == "" {
		store = "memory"
	}
	switch store {
	case "memory":
		if injected != nil {
			return nil, fmt.Errorf("bootstrap: Wiring.Cache requires cache.store: custom (got %q)", cfg.Store)
		}
		return cache.NewMemory(cfg.MaxEntries), nil
	case "redis":
		if injected != nil {
			return nil, fmt.Errorf("bootstrap: Wiring.Cache requires cache.store: custom (got \"redis\")")
		}
		return cache.NewRedis(cfg.Redis)
	case "custom":
		if injected == nil {
			return nil, fmt.Errorf("bootstrap: cache.store: custom requires Wiring.Cache to be set")
		}
		return injected, nil
	default:
		return nil, fmt.Errorf("bootstrap: cache.store: %q is not a valid backend (memory | redis | custom)", cfg.Store)
	}
}

// resolveSharedCache builds the CROSS-SERVICE cache from cfg.Shared + an
// optional Wire-injected instance. Returns nil when cfg.Shared is nil —
// Deps.SharedCache stays nil and the consumer feature MUST guard before
// dereferencing.
//
// Same conflict matrix as resolveCache, with one extra rule already
// enforced upstream (validateCacheShared): store=memory is rejected.
func resolveSharedCache(cfg *CacheConfig, injected cache.Cache) (cache.Cache, error) {
	shared := (*CacheSharedConfig)(nil)
	if cfg != nil {
		shared = cfg.Shared
	}
	if shared == nil {
		if injected != nil {
			return nil, fmt.Errorf("bootstrap: Wiring.SharedCache was set but cache.shared: block is absent in YAML — declare cache.shared.store: custom to enable Wire injection")
		}
		return nil, nil
	}
	store := strings.ToLower(strings.TrimSpace(shared.Store))
	switch store {
	case "redis":
		if injected != nil {
			return nil, fmt.Errorf("bootstrap: Wiring.SharedCache requires cache.shared.store: custom (got \"redis\")")
		}
		return cache.NewRedis(shared.Redis)
	case "custom":
		if injected == nil {
			return nil, fmt.Errorf("bootstrap: cache.shared.store: custom requires Wiring.SharedCache to be set")
		}
		return injected, nil
	default:
		// memory + unknown values rejected by validateCacheShared; defensive.
		return nil, fmt.Errorf("bootstrap: cache.shared.store: %q is not a valid backend for the shared cache (redis | custom)", shared.Store)
	}
}
