package httpclient

import "context"

// BaseURLResolver resolves a service's baseURL at call time. When the
// HttpClient has no resolver (the default), every call uses the YAML's
// services.<name>.baseURL verbatim — zero overhead on the hot path.
// Register a resolver via WithResolver(r) to plug in dynamic service
// discovery: Consul lookups, Kubernetes Service DNS, per-tenant URL maps,
// internal load balancers, environment-driven routing.
//
// The resolver is consulted on every call. Implementations are responsible
// for their own caching / refresh policy — the framework does not memoize
// resolver returns.
//
// Cascade with the YAML configuration:
//
//   - Resolve returns (url, nil) with a non-empty url → that URL replaces
//     the YAML baseURL for this call.
//   - Resolve returns ("", nil) → fall back to the YAML baseURL. Useful
//     when the resolver only knows a subset of services; the rest stay
//     declared in YAML.
//   - Resolve returns (_, err) → the call aborts with *HttpError before
//     dialing, carrying the resolver's error as Cause.
//   - Resolver and YAML both empty → the call aborts with *HttpError
//     describing the missing baseURL.
//
// The context passed to Resolve is the same callCtx the framework will use
// for the outbound dial (already carries the per-call timeout). Resolvers
// that touch the network should honor cancellation.
type BaseURLResolver interface {
	Resolve(ctx context.Context, service string) (string, error)
}

// StaticBaseURLResolver is the trivial reference implementation: a
// per-service map of baseURLs. Unknown services return "" (fall back to
// the YAML). Useful for tests and for static per-environment maps the YAML
// cannot express directly (e.g., a single binary that needs to switch
// targets between region-specific clusters at boot).
//
//	resolver := httpclient.StaticBaseURLResolver{
//	    "keycloak": "https://kc.east.example.com",
//	    "payment":  "https://pay.east.example.com",
//	}
//	client, _ := httpclient.New(cfg, httpclient.WithResolver(resolver))
type StaticBaseURLResolver map[string]string

// Resolve returns the entry for service or "" when absent. Never errors —
// the map is the source of truth.
func (s StaticBaseURLResolver) Resolve(_ context.Context, service string) (string, error) {
	return s[service], nil
}
