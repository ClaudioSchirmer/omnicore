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
// defaults control whether cache runs at all and the store sizing.
type CacheDefaults struct {
	Enabled           *bool    `yaml:"enabled"`
	DefaultTTL        Duration `yaml:"defaultTTL"`
	MaxEntries        int      `yaml:"maxEntries"`
	HonorCacheControl *bool    `yaml:"honorCacheControl"`
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
