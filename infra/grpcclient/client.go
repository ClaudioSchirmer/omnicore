package grpcclient

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/ClaudioSchirmer/omnicore/infra/resilience"
)

// Client is the boot-time singleton over the yaml `grpcClient:` block.
// Immutable after New; safe for concurrent use.
type Client struct {
	services map[string]*service
}

// service pairs the resolved config with its runtime state: the pooled
// HTTP client and the per-procedure breakers.
type service struct {
	cfg      *resolvedService
	http     *http.Client
	breakers *breakerRegistry
	tracing  bool
}

// Option configures New.
type Option func(*options)

type options struct {
	tracing   bool
	transport http.RoundTripper // test seam
}

// WithClientTracing gates the outbound client span + W3C traceparent
// injection — the sibling of httpclient.WithClientTracing; bootstrap passes
// the `httpclient` tracing instrument resolution.
func WithClientTracing(on bool) Option {
	return func(o *options) { o.tracing = on }
}

// WithTransport overrides the underlying RoundTripper (tests).
func WithTransport(rt http.RoundTripper) Option {
	return func(o *options) { o.transport = rt }
}

// New resolves the config into a ready Client. Errors are boot-time only.
func New(cfg *Config, opts ...Option) (*Client, error) {
	if cfg == nil {
		return &Client{services: map[string]*service{}}, nil
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	services := make(map[string]*service, len(cfg.Services))
	for name, svcCfg := range cfg.Services {
		resolved, err := resolve(name, svcCfg, cfg.Defaults)
		if err != nil {
			return nil, err
		}
		httpClient := &http.Client{}
		if o.transport != nil {
			httpClient.Transport = o.transport
		} else if resolved.pool != nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			if resolved.pool.MaxIdleConnsPerHost > 0 {
				transport.MaxIdleConnsPerHost = resolved.pool.MaxIdleConnsPerHost
				if transport.MaxIdleConns < resolved.pool.MaxIdleConnsPerHost {
					transport.MaxIdleConns = resolved.pool.MaxIdleConnsPerHost
				}
			}
			httpClient.Transport = transport
			if resolved.pool.ConnMaxLifetimeSeconds > 0 {
				// Background sweep: close pooled idle pipes on the lifetime
				// cadence so the caller re-dials (and rebalances) even
				// against producers without an idle timeout. The Client is
				// a boot singleton, so the ticker lives for the process.
				go func(t *http.Transport, every time.Duration) {
					ticker := time.NewTicker(every)
					defer ticker.Stop()
					for range ticker.C {
						t.CloseIdleConnections()
					}
				}(transport, time.Duration(resolved.pool.ConnMaxLifetimeSeconds)*time.Second)
			}
		}
		services[name] = &service{
			cfg:      resolved,
			http:     httpClient,
			breakers: newBreakerRegistry(resolved.breakerCfg),
			tracing:  o.tracing,
		}
	}
	return &Client{services: services}, nil
}

// lookup returns the service or a descriptive error naming the yaml seat.
func (c *Client) lookup(name string) (*service, error) {
	s, ok := c.services[name]
	if !ok {
		return nil, fmt.Errorf("grpcclient: service %q is not declared under grpcClient.services", name)
	}
	return s, nil
}

// HTTPClient returns the service's pooled HTTP client (the first argument
// of every generated Connect constructor).
func (c *Client) HTTPClient(name string) (connect.HTTPClient, error) {
	s, err := c.lookup(name)
	if err != nil {
		return nil, err
	}
	return s.http, nil
}

// BaseURL returns the service's resolved base URL (the second argument of
// every generated Connect constructor).
func (c *Client) BaseURL(name string) (string, error) {
	s, err := c.lookup(name)
	if err != nil {
		return "", err
	}
	return s.cfg.baseURL, nil
}

// ClientOptions returns the connect options carrying the service's
// interceptor chain, outermost → innermost: correlation → tracing → auth →
// idempotency → deadline → logging → retry → breaker.
//
// Order rationale: correlation and auth read the AppContext by type
// assertion on ctx (the framework convention: the AppContext IS the call
// ctx), so they run BEFORE the deadline layer derives a child context. The
// deadline still sits outside retry — it bounds the whole logical call,
// attempts and backoffs included. Idempotency sits outside retry so the key
// is stable across attempts; the breaker sits inside retry so every attempt
// consults and feeds the state machine — the same relative order as the
// httpclient chain.
func (c *Client) ClientOptions(name string) ([]connect.ClientOption, error) {
	s, err := c.lookup(name)
	if err != nil {
		return nil, err
	}
	return s.clientOptions(), nil
}

func (s *service) clientOptions() []connect.ClientOption {
	return []connect.ClientOption{connect.WithInterceptors(
		s.correlationInterceptor(),
		s.tracingInterceptor(),
		s.authInterceptor(),
		s.idempotencyInterceptor(),
		s.deadlineInterceptor(),
		s.loggingInterceptor(),
		s.retryInterceptor(),
		s.breakerInterceptor(),
	)}
}

// For constructs a generated Connect client for a declared service in one
// call — the typed entrypoint in the spirit of httpclient.Call:
//
//	users, err := grpcclient.For(deps.GRPCClient, "users",
//	    usersv1connect.NewUsersServiceClient)
func For[T any](
	c *Client,
	name string,
	ctor func(connect.HTTPClient, string, ...connect.ClientOption) T,
) (T, error) {
	var zero T
	s, err := c.lookup(name)
	if err != nil {
		return zero, err
	}
	return ctor(s.http, s.cfg.baseURL, s.clientOptions()...), nil
}

// breakerRegistry tracks one resilience.Breaker per procedure. Configuration
// is shared across the service's procedures; runtime state is per pair —
// the same model as the httpclient's per-(service, endpoint) registry.
type breakerRegistry struct {
	policy   resilience.BreakerPolicy
	mu       sync.Mutex
	breakers map[string]*resilience.Breaker
}

func newBreakerRegistry(policy resilience.BreakerPolicy) *breakerRegistry {
	return &breakerRegistry{policy: policy, breakers: map[string]*resilience.Breaker{}}
}

func (r *breakerRegistry) get(procedure string) *resilience.Breaker {
	if !r.policy.Enabled {
		return nil // nil Breaker is a no-op by contract
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.breakers[procedure]
	if !ok {
		b = resilience.NewBreaker(r.policy)
		r.breakers[procedure] = b
	}
	return b
}
