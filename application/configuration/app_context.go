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
// parent — the HTTP wrappers set the parent to c.UserContext() so that a
// client disconnect or request timeout propagates all the way down to the
// ViewReader / Repository call without any extra plumbing.
//
// When no parent is set (tests, jobs, manual construction), the delegation
// falls back to context.Background() — never nil.
type AppContext struct {
	mu          sync.RWMutex
	id          uuid.UUID
	language    Language
	metadata    map[string]any
	identity    *Identity
	bearerToken string
	parent      context.Context
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

// ActorSubject returns the JWT `sub` of the request, or the sentinel
// domain.AnonymousActor when no Identity is attached. Consumed by the
// audit/event pipelines so every log line carries "who did this".
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

// SetParent injects the cancellation context — typically Fiber's
// c.UserContext() — so the AppContext propagates request cancellation to
// downstream IO. Calling SetParent(nil) restores the context.Background()
// fallback.
func (c *AppContext) SetParent(ctx context.Context) {
	c.mu.Lock()
	c.parent = ctx
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

func (c *AppContext) Deadline() (time.Time, bool) { return c.parentCtx().Deadline() }
func (c *AppContext) Done() <-chan struct{}       { return c.parentCtx().Done() }
func (c *AppContext) Err() error                  { return c.parentCtx().Err() }
func (c *AppContext) Value(key any) any           { return c.parentCtx().Value(key) }
