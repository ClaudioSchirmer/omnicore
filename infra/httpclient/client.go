package httpclient

import (
	"fmt"
	"log/slog"
	"strings"

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
	services     map[string]*serviceClient
	logger       *slog.Logger
	cacheStore   Cache
	breakerStore map[string]*breakerState
	auth         *auth.Registry
	// authRevocation tracks the revocationOnUnauthorized flag per provider
	// name so the middleware can opt into the cache-invalidate-retry path
	// without the provider needing to expose it.
	authRevocation map[string]bool
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
		return c, nil
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c.services = make(map[string]*serviceClient, len(cfg.Services))
	breakerPolicy := resolveBreakerConfig(cfg.Defaults.CircuitBreaker)
	c.breakerStore = make(map[string]*breakerState)
	anyCacheEnabled := false
	for name, sc := range cfg.Services {
		svc, err := buildServiceClient(name, sc, cfg.Defaults)
		if err != nil {
			return nil, err
		}
		c.services[name] = svc
		for epName, ep := range c.services[name].endpoints {
			if ep.cache.enabled {
				anyCacheEnabled = true
			}
			if breakerPolicy.enabled {
				c.breakerStore[name+"|"+epName] = newBreakerState(breakerPolicy)
			}
		}
	}
	if anyCacheEnabled {
		c.cacheStore = newMemoryCache(resolveMaxEntries(cfg.Defaults.Cache))
	}
	if len(cfg.AuthProviders) > 0 {
		reg, revocation, err := buildAuthRegistry(cfg.AuthProviders)
		if err != nil {
			return nil, err
		}
		c.auth = reg
		c.authRevocation = revocation
	}
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
	if c == nil || c.auth == nil {
		if override != "" {
			return nil, false, fmt.Errorf("httpclient: CallConfig.AuthProvider %q used but no authProviders are configured", override)
		}
		return nil, false, fmt.Errorf("httpclient: service %q references provider %q but no authProviders are configured", svc.name, name)
	}
	provider, err := c.auth.Lookup(name)
	if err != nil {
		return nil, false, err
	}
	return provider, c.authRevocation[name], nil
}

// breaker returns the breaker state for the given (service, endpoint)
// pair. Returns nil when the breaker is disabled — the middleware
// short-circuits in that case.
func (c *HttpClient) breaker(service, endpoint string) *breakerState {
	if c == nil || c.breakerStore == nil {
		return nil
	}
	return c.breakerStore[service+"|"+endpoint]
}

// service returns the per-service runtime for the named service or an error
// if the service was not declared in the YAML. Used by the call surface;
// kept unexported so consumers cannot reach into the registry behind the
// typed Call[Req, Resp] generic.
func (c *HttpClient) service(name string) (*serviceClient, error) {
	if c == nil || c.services == nil {
		return nil, fmt.Errorf("httpclient: no services configured (httpClient: block absent or empty)")
	}
	s, ok := c.services[name]
	if !ok {
		return nil, fmt.Errorf("httpclient: unknown service %q", name)
	}
	return s, nil
}
