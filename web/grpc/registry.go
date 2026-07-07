package grpc

import (
	"net/http"
	"time"

	"connectrpc.com/connect"

	"github.com/ClaudioSchirmer/omnicore/application/pipeline"
	"github.com/ClaudioSchirmer/omnicore/web/authcore"
)

// Registry is the gRPC surface's registration point — the sibling of
// graphql.Registry. The consumer builds it with grpc.New(deps.Pipeline),
// constructs each generated service handler with reg.HandlerOptions(), and
// mounts the (path, handler) pair; bootstrap injects the runtime policy
// (auth, tracing, timeout) from the yaml before serving reg.Handler() on
// the dedicated gRPC listener.
//
// Policy setters (EnableAuth, EnableServerSpanTracing, SetRequestTimeout)
// are boot-time calls: they must complete before the listener accepts
// traffic and are not synchronized for concurrent mutation afterwards —
// the same single-writer-then-read-only contract every bootstrap knob has.
type Registry struct {
	pipe *pipeline.Pipeline
	mux  *http.ServeMux

	services []string // "pkg.v1.Service" names, for the reflection service

	auth       *authcore.Validator
	authPolicy AuthPolicy

	traceServerSpan bool
	requestTimeout  time.Duration
	authzEnabled    bool

	posture     AuthPosture
	attribution *authcore.Validator
}

// AuthPolicy is the transport policy around the shared JWT core — the gRPC
// counterpart of the Fiber shell's PublicRoutes/TenantRequired knobs.
type AuthPolicy struct {
	// PublicProcedures lists fully-qualified procedures that bypass
	// authentication entirely, e.g. "/users.v1.UsersService/Health".
	PublicProcedures []string

	// TenantRequired rejects any authenticated request whose Identity
	// carries no tenant claim — same semantics as the REST middleware.
	TenantRequired bool
}

// New builds an empty Registry over the service's Pipeline (translation of
// failure envelopes rides it, exactly like every other surface).
func New(pipe *pipeline.Pipeline) *Registry {
	return &Registry{pipe: pipe, mux: http.NewServeMux()}
}

// Pipeline exposes the pipeline for wrapper construction at wire time.
func (r *Registry) Pipeline() *pipeline.Pipeline { return r.pipe }

// HandlerOptions returns the connect options every generated service
// constructor must receive — the framework interceptor chain, outermost →
// innermost: recovery, AppContext, auth.
func (r *Registry) HandlerOptions() []connect.HandlerOption {
	return []connect.HandlerOption{
		connect.WithInterceptors(
			r.recoveryInterceptor(),
			r.appContextInterceptor(),
			r.authInterceptor(),
		),
	}
}

// mountProcedure registers one RPC under its generated procedure constant,
// recording the fully-qualified service name once for the reflection
// service.
func (r *Registry) mountProcedure(procedure string, h http.Handler) {
	r.mux.Handle(procedure, h)
	svc := serviceOf(procedure)
	if svc == "" {
		return
	}
	for _, existing := range r.services {
		if existing == svc {
			return
		}
	}
	r.services = append(r.services, svc)
}

// MountRaw registers an arbitrary handler on the gRPC listener's mux
// without recording a service name (health endpoints, reflection).
func (r *Registry) MountRaw(pattern string, h http.Handler) {
	r.mux.Handle(pattern, h)
}

// Handler is the listener-facing mux; bootstrap wraps it with h2c so the
// gRPC protocol works without TLS in dev.
func (r *Registry) Handler() http.Handler { return r.mux }

// ServiceNames lists the mounted fully-qualified service names, in mount
// order — the input the reflection service needs.
func (r *Registry) ServiceNames() []string {
	return append([]string(nil), r.services...)
}

// EnableAuth arms the auth interceptor with the shared JWT core
// (web/authcore) and the transport policy. Called by bootstrap when
// auth.mode=jwt; never called → the interceptor is a no-op (auth disabled,
// dev only, mirroring the REST surface).
func (r *Registry) EnableAuth(v *authcore.Validator, policy AuthPolicy) {
	r.auth = v
	r.authPolicy = policy
}

// EnableAuthorization is the Layer-1 permission-gate master switch — the
// gRPC twin of web.SetAuthorizationEnabled and
// graphql.Registry.EnableAuthorization: off (the default) leaves every
// RequirePermission annotation inert (services annotate ahead of the
// operator flip); on enforces Identity.HasPermission per procedure.
// Called by bootstrap from auth.authorization.enabled.
func (r *Registry) EnableAuthorization(on bool) { r.authzEnabled = on }

// EnableServerSpanTracing gates the inbound server span, mirroring
// web.WithServerSpanTracing: off (the default) means no span, no
// traceparent extraction — the dispatch span roots the trace locally.
func (r *Registry) EnableServerSpanTracing(on bool) { r.traceServerSpan = on }

// SetRequestTimeout bounds each RPC's lifetime server-side, mirroring
// web.WithRequestTimeout: the AppContext's cancellation parent gets the
// deadline, so pgx/mongo/outbound calls abort when it elapses — surfaced
// as DEADLINE_EXCEEDED (the 504 sibling). An inbound RPC deadline (the
// protocol timeout header) composes with this: whichever is earlier wins,
// context semantics. d <= 0 disables the server-side ceiling.
func (r *Registry) SetRequestTimeout(d time.Duration) { r.requestTimeout = d }

func (r *Registry) isPublicProcedure(procedure string) bool {
	for _, p := range r.authPolicy.PublicProcedures {
		if p == procedure {
			return true
		}
	}
	return false
}
