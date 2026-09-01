package configuration

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AppContext is the application-layer request envelope and the canonical
// vehicle for per-request state (UUID, Language, metadata). It also implements
// context.Context by delegating Deadline/Done/Err/Value to an injectable
// parent — the HTTP wrappers set the parent to c (the fiber.Ctx itself, which
// implements context.Context in Fiber v3) so that a client disconnect or
// request timeout propagates all the way down to the ViewReader / Repository
// call without any extra plumbing.
//
// When no parent is set (tests, jobs, manual construction), the delegation
// falls back to context.Background() — never nil.
type AppContext struct {
	mu            sync.RWMutex
	id            uuid.UUID
	language      Language
	metadata      map[string]any
	identity      *Identity
	bearerToken   string
	clientIP      string
	correlationID uuid.UUID
	causationID   uuid.UUID
	parent        context.Context
	traceCtx      context.Context
}

func NewAppContext(id uuid.UUID, lang Language) *AppContext {
	if lang == LangUnknown {
		lang = LangENG
	}
	return &AppContext{
		id:       id,
		language: lang,
		metadata: map[string]any{},
	}
}

func NewAppContextWithRandomID(lang Language) *AppContext {
	return NewAppContext(uuid.New(), lang)
}

func (c *AppContext) ID() uuid.UUID {
	return c.id
}

func (c *AppContext) Language() Language {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.language
}

func (c *AppContext) SetLanguage(lang Language) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.language = lang
}

func (c *AppContext) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.metadata[key] = value
}

func (c *AppContext) Get(key string) (any, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.metadata[key]
	return v, ok
}

// Identity returns the authenticated principal of the request, or nil when
// the request is unauthenticated (public route, auth disabled, or middleware
// has not populated it yet).
func (c *AppContext) Identity() *Identity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.identity
}

// SetIdentity is the auth middleware's hook to attach the authenticated
// principal to the request. Passing nil clears the identity.
func (c *AppContext) SetIdentity(id *Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identity = id
}

// BearerToken returns the raw verified JWT of the request, or "" when no
// bearer is attached (public route, auth disabled, middleware did not run, or
// authentication failed). Consumed exclusively by the httpclient
// forward-bearer auth provider so it can propagate the inbound user's
// credential downstream. Services should keep reading Identity() for
// principal data — the raw token carries no parsed information.
func (c *AppContext) BearerToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.bearerToken
}

// SetBearerToken is the auth middleware's hook to attach the verified raw
// token to the request — called only after JWT validation (and the optional
// external revocation check) succeeds. Passing "" clears the token.
func (c *AppContext) SetBearerToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bearerToken = token
}

// ClientIP returns the network origin of the request as the framework
// resolved it, or "" when nothing resolved it (a background job, a consumer
// handler, a test fixture — anything that is not an inbound HTTP request).
//
// Behind a reverse proxy the value is only as trustworthy as the deployment
// says it is: with no `http.trustProxy` block it is the socket peer, which is
// spoof-proof but names the balancer; with the block declared it is the
// rightmost untrusted entry of the forwarded chain. A service building a
// network-based control (an IP allow-list, a per-origin rate limit) on this
// value is therefore also depending on that block being declared correctly —
// the framework resolves the address, it cannot make an undeclared topology
// trustworthy.
func (c *AppContext) ClientIP() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.clientIP
}

// SetClientIP is the framework middleware's hook to attach the resolved
// origin. Passing "" clears it.
func (c *AppContext) SetClientIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientIP = ip
}

// ActorSubject returns the JWT `sub` of the request, or the sentinel
// "anonymous" (persistence.AnonymousActor) when no Identity is attached.
// Consumed by the audit/event pipelines so every log line carries
// "who did this".
func (c *AppContext) ActorSubject() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.identity == nil {
		return "anonymous"
	}
	return c.identity.Subject
}

// ActorIssuer returns the JWT `iss` of the request, or "" when no Identity
// is attached. Empty (not "anonymous") so the audit log only surfaces an
// issuer when one really exists.
func (c *AppContext) ActorIssuer() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.identity == nil {
		return ""
	}
	return c.identity.Issuer
}

// ActorClaims returns a shallow copy of the JWT claim map so downstream
// consumers (audit, handlers) can safely read or mutate without affecting
// the stored Identity. Returns nil when no Identity is attached.
func (c *AppContext) ActorClaims() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.identity == nil || len(c.identity.Claims) == 0 {
		return nil
	}
	out := make(map[string]any, len(c.identity.Claims))
	for k, v := range c.identity.Claims {
		out[k] = v
	}
	return out
}

// CorrelationID returns the cross-service trace identifier of the request,
// or the zero UUID when none is set. Populated by the integration receiver
// path when the inbound event carried correlation_id; outbound
// fwintegration.Dispatch reads it as fallback when WithCorrelation is
// omitted, so events emitted inside a receiver handler automatically carry
// the inbound trace chain. Defensive against the zero value: callers
// branch on the boolean second return shape via != uuid.Nil.
func (c *AppContext) CorrelationID() uuid.UUID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.correlationID
}

// SetCorrelationID overwrites the correlation identifier. Concurrent-safe
// via the same mutex protecting language/metadata/identity. The integration
// receiver pipeline calls it on every invocation; consumer code can also
// set it from a custom inbound carrier (HTTP header, vendor envelope) if a
// trace chain comes through a non-framework path.
func (c *AppContext) SetCorrelationID(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.correlationID = id
}

// CausationID returns the identifier of the event that caused the current
// invocation, or the zero UUID when none is set. The integration receiver
// pipeline populates it with the inbound event_id so outbound Dispatch
// calls automatically thread "this event happened because of that event"
// into the integration_events row.
func (c *AppContext) CausationID() uuid.UUID {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.causationID
}

// SetCausationID overwrites the causation identifier. Same semantics as
// SetCorrelationID.
func (c *AppContext) SetCausationID(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.causationID = id
}

// SetParent injects the cancellation context — typically the Fiber request
// ctx (`c` itself in v3, since `fiber.Ctx` implements `context.Context`) — so
// the AppContext propagates request cancellation to downstream IO. Calling
// SetParent(nil) restores the context.Background() fallback.
func (c *AppContext) SetParent(ctx context.Context) {
	c.mu.Lock()
	c.parent = ctx
	c.mu.Unlock()
}

// SetParentIfAbsent injects the cancellation context only when none is set yet,
// leaving an already-wired parent untouched. The web AppContextMiddleware owns
// the parent in production (wrapping it with the request deadline); the HTTP
// wrappers call this so a bypassed-middleware path (tests, manual mounts) still
// gets request cancellation without clobbering the middleware's deadline-bearing
// parent.
func (c *AppContext) SetParentIfAbsent(ctx context.Context) {
	c.mu.Lock()
	if c.parent == nil {
		c.parent = ctx
	}
	c.mu.Unlock()
}

func (c *AppContext) parentCtx() context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.parent == nil {
		return context.Background()
	}
	return c.parent
}

// Parent returns the current cancellation/value parent (the fiber request ctx
// in production, context.Background() when unset). The tracing layer reads it to
// start a span FROM the parent — never from the AppContext itself, which would
// form a Value/Deadline delegation cycle once the resulting span context is set
// back as the new parent.
func (c *AppContext) Parent() context.Context {
	return c.parentCtx()
}

// SetTraceContext records the context carrying the inbound server span, set by
// the web AppContextMiddleware. It is kept SEPARATE from the cancellation parent
// because fiber v3's Ctx.Value does not delegate to the context installed via
// SetContext — so the span cannot ride the parent chain. The pipeline starts the
// business span FROM this context, making it a child of the server span. A plain
// context.Context (no OpenTelemetry import in this layer); nil clears it.
func (c *AppContext) SetTraceContext(ctx context.Context) {
	c.mu.Lock()
	c.traceCtx = ctx
	c.mu.Unlock()
}

// TraceContext returns the inbound-span context set by SetTraceContext, falling
// back to the cancellation parent and then context.Background() — so the
// pipeline span code has a non-nil base whether or not an inbound span exists
// (tests, jobs, tracing disabled).
func (c *AppContext) TraceContext() context.Context {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.traceCtx != nil {
		return c.traceCtx
	}
	if c.parent != nil {
		return c.parent
	}
	return context.Background()
}

func (c *AppContext) Deadline() (time.Time, bool) { return c.parentCtx().Deadline() }
func (c *AppContext) Done() <-chan struct{}       { return c.parentCtx().Done() }
func (c *AppContext) Err() error                  { return c.parentCtx().Err() }
func (c *AppContext) Value(key any) any           { return c.parentCtx().Value(key) }
