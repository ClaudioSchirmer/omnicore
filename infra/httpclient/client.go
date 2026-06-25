package httpclient

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/cache"
	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/auth"
)

// HttpClient is the outbound HTTP registry built by New and exposed on
// bootstrap.Deps.HttpClient. Features that talk to external services receive
// the client by injection and forward it to their infra/external service
// structs, which encapsulate the typed Call surface and per-vendor concerns.
//
// Per-service runtime (transport, headers, endpoints) lives behind this type
// in unexported maps populated by New from Config. The Call[Req, Resp]
// generic on the package level is the only consumer-facing entry point;
// internals (services map, logger, defaults) stay unexported.
type HttpClient struct {
	// reg holds the service/provider registry as one immutable snapshot
	// swapped atomically. The read path (service, breaker, resolveAuthProvider)
	// loads the pointer once per call and indexes lock-free; runtime
	// registration (RegisterIfAbsent / Unregister) builds a copy-on-write
	// successor and Stores it. Same pattern as cacheStore below — safe to
	// mutate concurrently with in-flight requests.
	reg atomic.Pointer[registry]
	// writeMu serializes registry writers (RegisterIfAbsent / Unregister) so
	// concurrent copy-on-write swaps never lose an entry. Readers never take
	// it — they go through reg.Load().
	writeMu sync.Mutex
	logger  *slog.Logger
	// cacheStore is the byte-level cache the GET cache middleware reads /
	// writes through. Stored as an atomic.Pointer so bootstrap can swap
	// the implementation AFTER New (typically when Wiring.Cache supplies
	// a custom backend resolved post-Wire) without rebuilding the chain.
	// nil pointer means the cache layer is disabled at the runtime
	// boundary — the middleware short-circuits as "bypass".
	cacheStore atomic.Pointer[cache.Cache]
	// fake is non-nil only on clients produced by NewFake. When set, Call
	// short-circuits the middleware chain and routes the request through
	// the in-memory stub registry. Production clients leave it nil and pay
	// no overhead on the call path.
	fake *fakeRegistry
	// resolver, when non-nil, is consulted on every Call to override the
	// per-service YAML baseURL. nil resolver keeps the zero-overhead path
	// (YAML baseURL used verbatim). See BaseURLResolver for the cascade
	// with the YAML configuration.
	resolver BaseURLResolver
	// traceClient gates the outbound client span + traceparent injection
	// middleware. Set by bootstrap from tracing.Instruments(SubHTTPClient) —
	// false (the default) keeps the chain byte-identical to the untraced path,
	// so a client built without tracing pays nothing.
	traceClient bool
}

// registry is the immutable snapshot of the service/provider maps a client
// dispatches against. New builds the initial snapshot; RegisterIfAbsent /
// Unregister publish copy-on-write successors. Existing *serviceClient,
// *breakerState and provider pointers are shared across snapshots, so a swap
// never disturbs warm token caches or live breaker state.
type registry struct {
	services       map[string]*serviceClient
	breakerStore   map[string]*breakerState
	auth           *auth.Registry
	authRevocation map[string]bool
	// runtime holds metadata for services added via RegisterIfAbsent. YAML
	// services are absent here — Count / Registered / Unregister operate on
	// this set only, so programmatic purge can never evict a boot upstream.
	runtime map[string]*runtimeMeta
	// runtimeProviders names the auth providers added via RegisterIfAbsent.
	// Unregister removes one only when it is runtime-origin AND no surviving
	// service references it, so a YAML-declared provider is never dropped.
	runtimeProviders map[string]struct{}
}

// runtimeMeta carries the bookkeeping for one runtime-registered service.
// lastUsed is an atomic so the read path can stamp it without the write lock.
type runtimeMeta struct {
	registeredAt time.Time
	lastUsed     atomic.Int64 // unix nanos of the last Call dispatched here
}

// emptyRegistry returns a fully-initialized empty snapshot.
func emptyRegistry() *registry {
	return &registry{
		services:         map[string]*serviceClient{},
		breakerStore:     map[string]*breakerState{},
		auth:             auth.NewRegistry(),
		authRevocation:   map[string]bool{},
		runtime:          map[string]*runtimeMeta{},
		runtimeProviders: map[string]struct{}{},
	}
}

// clone returns a shallow copy of the snapshot: fresh maps holding the same
// element pointers. Mutating the clone's maps (insert / delete) leaves the
// published snapshot untouched until the caller Stores the clone.
func (r *registry) clone() *registry {
	n := &registry{
		services:         make(map[string]*serviceClient, len(r.services)+1),
		breakerStore:     make(map[string]*breakerState, len(r.breakerStore)+1),
		auth:             r.auth.Clone(),
		authRevocation:   make(map[string]bool, len(r.authRevocation)+1),
		runtime:          make(map[string]*runtimeMeta, len(r.runtime)+1),
		runtimeProviders: make(map[string]struct{}, len(r.runtimeProviders)+1),
	}
	for k, v := range r.services {
		n.services[k] = v
	}
	for k, v := range r.breakerStore {
		n.breakerStore[k] = v
	}
	for k, v := range r.authRevocation {
		n.authRevocation[k] = v
	}
	for k, v := range r.runtime {
		n.runtime[k] = v
	}
	for k := range r.runtimeProviders {
		n.runtimeProviders[k] = struct{}{}
	}
	return n
}

// snap loads the current registry snapshot. Returns nil only on a zero-value
// client (e.g. the NewFake stub, whose Call path short-circuits before the
// registry is consulted).
func (c *HttpClient) snap() *registry {
	if c == nil {
		return nil
	}
	return c.reg.Load()
}

// Option customizes the constructor without changing the YAML schema.
// Currently only WithLogger is offered — every other knob lives in YAML so
// the configuration of an http client never spans Go + YAML.
type Option func(*HttpClient)

// WithLogger swaps the slog.Logger used for the per-call observation line.
// Defaults to slog.Default() when the option is not passed.
func WithLogger(l *slog.Logger) Option {
	return func(c *HttpClient) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithResolver registers a BaseURLResolver that dynamically overrides the
// per-service YAML baseURL on every call. Pass nil to keep the default
// behavior (YAML baseURL used verbatim). See BaseURLResolver for the
// cascade between the resolver and the YAML.
func WithResolver(r BaseURLResolver) Option {
	return func(c *HttpClient) {
		c.resolver = r
	}
}

// WithClientTracing enables the outbound client span + W3C traceparent
// injection on every Call. bootstrap passes tracing.Instruments(SubHTTPClient)
// here; left false (default) the tracing middleware is never added to the chain.
// When enabled but no tracer provider is installed the span is a no-op, so the
// flag is safe to set whenever the operator lists "httpclient" in the
// instrument allowlist.
func WithClientTracing(enabled bool) Option {
	return func(c *HttpClient) {
		c.traceClient = enabled
	}
}

// WithCache binds the byte-level cache.Cache the GET cache middleware
// reads / writes through. Pass nil to keep the cache layer disabled
// (the middleware short-circuits as "bypass"). Symmetric with
// WithResolver: the consumer / bootstrap supplies the dependency, the
// httpclient consumes it.
//
// The backend selection (memory | redis | custom) lives on the top-
// level cache: block in microservice.<profile>.yaml, resolved by
// bootstrap.buildDeps and forwarded here. Consumers building the
// HttpClient manually (tests, custom lifecycle) pass any cache.Cache
// implementation directly.
func WithCache(c cache.Cache) Option {
	return func(hc *HttpClient) {
		hc.SetCache(c)
	}
}

// SetCache atomically swaps the cache backend the GET cache middleware
// consults. Called by bootstrap AFTER Wire(deps) when Wiring.Cache
// supplies a custom backend that could not be resolved at New() time
// (custom backends depend on consumer code that runs in the Wire
// callback). Safe to call concurrently with in-flight requests — the
// middleware reads the pointer per call.
//
// Passing nil disables the cache layer (subsequent requests bypass);
// passing a non-nil value re-enables it.
func (c *HttpClient) SetCache(store cache.Cache) {
	if store == nil {
		c.cacheStore.Store(nil)
		return
	}
	c.cacheStore.Store(&store)
}

// cacheStoreGetter returns a closure the middleware reads at request
// time — late binding so SetCache after construction takes effect
// without chain rebuild.
func (c *HttpClient) cacheStoreGetter() func() cache.Cache {
	return func() cache.Cache {
		p := c.cacheStore.Load()
		if p == nil {
			return nil
		}
		return *p
	}
}

// New constructs the HttpClient from cfg. cfg may be nil (no httpClient: block
// in the YAML) — the resulting client is a valid type carrier with no
// services; Call rejects unknown service names rather than dialing nowhere.
//
// When cfg is non-nil, New runs applyDefaults + Validate before constructing
// any transport. Validation accumulates every issue into a single error so
// the operator sees the full list on one boot attempt. Reserved YAML keys
// (auth, retry, cache, circuitBreaker, pool, tls, redaction, signing,
// idempotency, responseStream, responseSSE, authProviders) are rejected with
// a message pointing at the future phase that introduces the feature.
func New(cfg *Config, opts ...Option) (*HttpClient, error) {
	c := &HttpClient{logger: slog.Default()}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	if cfg == nil {
		c.reg.Store(emptyRegistry())
		return c, nil
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	snap := emptyRegistry()
	breakerPolicy := resolveBreakerConfig(cfg.Defaults.CircuitBreaker)
	for name, sc := range cfg.Services {
		svc, err := buildServiceClient(name, sc, cfg.Defaults)
		if err != nil {
			return nil, err
		}
		snap.services[name] = svc
		for epName := range svc.endpoints {
			if breakerPolicy.enabled {
				snap.breakerStore[name+"|"+epName] = newBreakerState(breakerPolicy)
			}
		}
	}
	if len(cfg.AuthProviders) > 0 {
		reg, revocation, err := buildAuthRegistry(cfg.AuthProviders)
		if err != nil {
			return nil, err
		}
		snap.auth = reg
		snap.authRevocation = revocation
	}
	c.reg.Store(snap)
	return c, nil
}

// buildAuthRegistry materializes named providers from YAML and collects
// the per-provider revocationOnUnauthorized flag. Returns the first
// construction error so the operator sees a single actionable boot
// failure.
func buildAuthRegistry(cfg map[string]AuthProviderConfig) (*auth.Registry, map[string]bool, error) {
	reg := auth.NewRegistry()
	revocation := make(map[string]bool, len(cfg))
	for name, p := range cfg {
		provider, err := buildAuthProvider(name, p)
		if err != nil {
			return nil, nil, fmt.Errorf("httpclient: build provider %q: %w", name, err)
		}
		reg.Register(name, provider)
		revocation[name] = p.RevocationOnUnauthorized
	}
	return reg, revocation, nil
}

// buildAuthProvider instantiates a single provider by type. Validate has
// already accepted the shape; this only translates YAML into provider
// constructors. Unsupported types here would be a programmer error since
// the validator gates them upstream.
func buildAuthProvider(name string, p AuthProviderConfig) (auth.AuthProvider, error) {
	attach := toRuntimeAttach(p.Attach)
	switch strings.ToLower(strings.TrimSpace(p.Type)) {
	case "none":
		return auth.NewNoneProvider(name), nil
	case "header-static":
		return auth.NewHeaderStaticProvider(name, attach)
	case "bearer-static":
		return auth.NewBearerStaticProvider(name, p.Token, attach)
	case "basic":
		return auth.NewBasicProvider(name, p.Username, p.Password, attach)
	case "forward-bearer":
		return auth.NewForwardBearerProvider(name, attach), nil
	case "oauth2-client-credentials":
		var cache auth.TokenCacheConfig
		if p.TokenCache != nil {
			cache = toRuntimeTokenCacheConfig(p.TokenCache)
		}
		return auth.NewOAuth2ClientCredentialsProvider(auth.OAuth2Options{
			Name:                     name,
			TokenEndpoint:            p.TokenEndpoint,
			ClientID:                 p.ClientID,
			ClientSecret:             p.ClientSecret,
			Scope:                    p.Scope,
			Audience:                 p.Audience,
			Attach:                   attach,
			Cache:                    cache,
			RevocationOnUnauthorized: p.RevocationOnUnauthorized,
		})
	case "credentials-exchange":
		var cache auth.TokenCacheConfig
		if p.TokenCache != nil {
			cache = toRuntimeTokenCacheConfig(p.TokenCache)
		}
		return auth.NewCredentialsExchangeProvider(auth.CredentialsExchangeOptions{
			Name:                     name,
			TokenEndpoint:            p.TokenEndpoint,
			RequestCodec:             p.RequestCodec,
			RequestFields:            p.RequestFields,
			RequestFieldsFromCtx:     p.RequestFieldsFromCtx,
			RequestHeaders:           p.RequestHeaders,
			ResponseTokenPath:        p.ResponseTokenPath,
			Attach:                   attach,
			Cache:                    cache,
			RevocationOnUnauthorized: p.RevocationOnUnauthorized,
		})
	}
	return nil, fmt.Errorf("provider type %q is not implemented in this phase", p.Type)
}

// resolveEffectiveAuthProvider applies the precedence cascade for the
// auth provider that should authenticate this call:
//
//  1. cfg.inlineAuth (set via CallConfig.InlineAuth) — credentials supplied
//     at call time; the framework builds an ephemeral provider and reports
//     it on obs.AuthProvider as "inline:<scheme>". No revocation is
//     attached: inline credentials are static for the duration of one call
//     and have nowhere to refresh.
//  2. cfg.authOverride (set via CallConfig.AuthProvider) or the service's
//     declared provider — looked up against the registry built at New.
//
// Returns (nil, false, nil) when neither path produces a provider (the
// service has no auth and no override was supplied).
func (c *HttpClient) resolveEffectiveAuthProvider(svc *serviceClient, cfg *invokeConfig) (auth.AuthProvider, bool, error) {
	if cfg != nil && cfg.inlineAuth != nil {
		provider, err := buildInlineAuthProvider(cfg.inlineAuth)
		if err != nil {
			return nil, false, err
		}
		return provider, false, nil
	}
	override := ""
	if cfg != nil {
		override = cfg.authOverride
	}
	return c.resolveAuthProvider(svc, override)
}

// buildInlineAuthProvider materializes one of the three inline schemes
// (Bearer / APIKey / Basic) into an existing auth.* provider so the rest
// of the middleware chain treats it identically to a YAML-declared one.
// Rejects ambiguous shapes (none or multiple schemes set) before dialing.
func buildInlineAuthProvider(in *InlineAuth) (auth.AuthProvider, error) {
	set := 0
	if in.Bearer != "" {
		set++
	}
	if in.APIKey != nil {
		set++
	}
	if in.Basic != nil {
		set++
	}
	if set == 0 {
		return nil, fmt.Errorf("inline auth: no credential set (one of Bearer / APIKey / Basic required)")
	}
	if set > 1 {
		return nil, fmt.Errorf("inline auth: exactly one of Bearer / APIKey / Basic must be set (got %d)", set)
	}
	if in.Bearer != "" {
		return auth.NewBearerStaticProvider("inline:bearer", in.Bearer, auth.AttachConfig{})
	}
	if in.APIKey != nil {
		if in.APIKey.Value == "" {
			return nil, fmt.Errorf("inline auth: APIKey.Value required")
		}
		header := in.APIKey.Header
		if header == "" {
			header = "X-API-Key"
		}
		return auth.NewHeaderStaticProvider("inline:apikey", auth.AttachConfig{
			Kind: auth.AttachHeader, Name: header, Value: in.APIKey.Value,
		})
	}
	if in.Basic != nil {
		if in.Basic.Username == "" || in.Basic.Password == "" {
			return nil, fmt.Errorf("inline auth: Basic.Username and Basic.Password required")
		}
		return auth.NewBasicProvider("inline:basic", in.Basic.Username, in.Basic.Password, auth.AttachConfig{})
	}
	return nil, fmt.Errorf("inline auth: unreachable")
}

// resolveAuthProvider returns the provider that should authenticate the
// next call along with the revocationOnUnauthorized flag for that
// provider. Per-call CallConfig.AuthProvider override wins over the
// service-level configuration; both are looked up against the registry
// built at New. Returns nil (no auth applied) when neither the override
// nor the service declares one.
func (c *HttpClient) resolveAuthProvider(svc *serviceClient, override string) (auth.AuthProvider, bool, error) {
	name := override
	if name == "" {
		name = svc.authProvider
	}
	if name == "" {
		return nil, false, nil
	}
	snap := c.snap()
	if snap == nil || snap.auth == nil || snap.auth.Len() == 0 {
		if override != "" {
			return nil, false, fmt.Errorf("httpclient: CallConfig.AuthProvider %q used but no authProviders are configured", override)
		}
		return nil, false, fmt.Errorf("httpclient: service %q references provider %q but no authProviders are configured", svc.name, name)
	}
	provider, err := snap.auth.Lookup(name)
	if err != nil {
		return nil, false, err
	}
	return provider, snap.authRevocation[name], nil
}

// breaker returns the breaker state for the given (service, endpoint)
// pair. Returns nil when the breaker is disabled — the middleware
// short-circuits in that case.
func (c *HttpClient) breaker(service, endpoint string) *breakerState {
	snap := c.snap()
	if snap == nil {
		return nil
	}
	return snap.breakerStore[service+"|"+endpoint]
}

// service returns the per-service runtime for the named service or an error
// if the service was neither declared in the YAML nor registered at runtime.
// Used by the call surface; kept unexported so consumers cannot reach into the
// registry behind the typed Call[Req, Resp] generic. Stamps the runtime
// entry's lastUsed (lock-free) when the resolved service was registered at
// runtime, so Registered() can drive least-recently-used purge.
func (c *HttpClient) service(name string) (*serviceClient, error) {
	snap := c.snap()
	if snap == nil || len(snap.services) == 0 {
		return nil, fmt.Errorf("httpclient: no services configured (httpClient: block absent or empty)")
	}
	s, ok := snap.services[name]
	if !ok {
		return nil, fmt.Errorf("httpclient: unknown service %q", name)
	}
	if m := snap.runtime[name]; m != nil {
		m.lastUsed.Store(time.Now().UnixNano())
	}
	return s, nil
}

// RegisteredService is one runtime-registered service's metadata, returned by
// Registered so the consumer can program purge (e.g. sort by LastUsedAt and
// Unregister the oldest N). YAML-declared services are never reported.
type RegisteredService struct {
	Name         string
	RegisteredAt time.Time // when RegisterIfAbsent first inserted it
	LastUsedAt   time.Time // last Call dispatched against it (init = RegisteredAt)
}

// RegisterIfAbsent compiles the services and auth providers in cfg and merges
// the ones not already present into the live client, idempotently. It is the
// code-wiring twin of New: cfg uses the same Config / ServiceConfig /
// AuthProviderConfig shapes the YAML decodes into, runs the same
// applyDefaults + Validate, and the merged services share the same token
// cache, connection pool, circuit breaker, retry and signing machinery as
// YAML-declared ones. A service / provider whose name already exists (YAML or
// a prior registration) is left untouched, so a repeated call preserves the
// warm token cache and breaker state — register a dynamic upstream once and
// reuse it by name on every Call.
//
// cfg is self-contained: a service may only reference an auth provider
// declared in the same cfg (Validate enforces it). Validation runs at call
// time, so a malformed cfg returns the error here instead of at boot; on any
// error nothing is merged (all-or-nothing). The dynamic target's lifecycle is
// the consumer's — pair this with Unregister; the framework ships no implicit
// TTL or eviction.
func (c *HttpClient) RegisterIfAbsent(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	cur := c.snap()
	if cur == nil {
		cur = emptyRegistry()
	}
	next := cur.clone()
	breakerPolicy := resolveBreakerConfig(cfg.Defaults.CircuitBreaker)
	added := false

	// Providers first — services reference them by name. Skip any name that
	// already exists so a YAML / prior-registration provider (and its warm
	// token cache) is preserved.
	for pname, p := range cfg.AuthProviders {
		if next.auth.Has(pname) {
			continue
		}
		provider, err := buildAuthProvider(pname, p)
		if err != nil {
			return fmt.Errorf("httpclient: build provider %q: %w", pname, err)
		}
		next.auth.Register(pname, provider)
		next.authRevocation[pname] = p.RevocationOnUnauthorized
		next.runtimeProviders[pname] = struct{}{}
		added = true
	}

	now := time.Now()
	for name, sc := range cfg.Services {
		if _, exists := next.services[name]; exists {
			continue // idempotent; also the YAML-collision no-op
		}
		svc, err := buildServiceClient(name, sc, cfg.Defaults)
		if err != nil {
			return err
		}
		next.services[name] = svc
		for epName := range svc.endpoints {
			if breakerPolicy.enabled {
				next.breakerStore[name+"|"+epName] = newBreakerState(breakerPolicy)
			}
		}
		m := &runtimeMeta{registeredAt: now}
		m.lastUsed.Store(now.UnixNano())
		next.runtime[name] = m
		added = true
	}

	if !added {
		return nil // everything already present — avoid a needless swap
	}
	c.reg.Store(next)
	return nil
}

// Unregister removes a runtime-registered service by name, dropping its
// breaker entries and — when the auth provider it referenced was itself
// runtime-registered and no surviving service still references it — that
// provider (and its token cache). Returns true when a service was removed;
// false when the name is unknown or YAML-declared (Unregister never touches a
// boot upstream). In-flight Calls holding the pre-swap snapshot finish against
// the old entry; no draining is required.
func (c *HttpClient) Unregister(name string) bool {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	cur := c.snap()
	if cur == nil {
		return false
	}
	if _, ok := cur.runtime[name]; !ok {
		return false // unknown or YAML-origin — refuse
	}

	next := cur.clone()
	svc := next.services[name]
	delete(next.services, name)
	delete(next.runtime, name)
	if svc != nil {
		for epName := range svc.endpoints {
			delete(next.breakerStore, name+"|"+epName)
		}
		if p := svc.authProvider; p != "" {
			if _, isRuntime := next.runtimeProviders[p]; isRuntime && !providerReferenced(next.services, p) {
				next.auth.Remove(p)
				delete(next.authRevocation, p)
				delete(next.runtimeProviders, p)
			}
		}
	}
	c.reg.Store(next)
	return true
}

// Count reports how many services were registered at runtime via
// RegisterIfAbsent. YAML-declared services are excluded.
func (c *HttpClient) Count() int {
	snap := c.snap()
	if snap == nil {
		return 0
	}
	return len(snap.runtime)
}

// Registered lists the runtime-registered services sorted by name, with their
// RegisteredAt and LastUsedAt timestamps, so the consumer can implement any
// purge policy. YAML-declared services are never listed. Returns nil when none
// are registered.
func (c *HttpClient) Registered() []RegisteredService {
	snap := c.snap()
	if snap == nil || len(snap.runtime) == 0 {
		return nil
	}
	out := make([]RegisteredService, 0, len(snap.runtime))
	for name, m := range snap.runtime {
		out = append(out, RegisteredService{
			Name:         name,
			RegisteredAt: m.registeredAt,
			LastUsedAt:   time.Unix(0, m.lastUsed.Load()),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// providerReferenced reports whether any service in the set still references
// the named auth provider — the guard that keeps Unregister from dropping a
// provider shared by a surviving service.
func providerReferenced(services map[string]*serviceClient, provider string) bool {
	for _, s := range services {
		if s.authProvider == provider {
			return true
		}
	}
	return false
}
