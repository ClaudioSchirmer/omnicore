package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// CredentialsExchangeProvider is a generic "POST credentials, get token"
// auth provider. Use it for IdPs whose token endpoint diverges from RFC
// 6749 — custom field names, JSON body instead of form-urlencoded, or
// non-standard response shapes.
//
// For Keycloak-style RFC OAuth2 (client_credentials, password,
// refresh_token grants), CredentialsExchangeProvider works too: declare
// the grant body via RequestFields with the canonical RFC names.
//
// Multi-tenant: when RequestFieldsFromCtx is non-empty, the body field
// values come from AppContext at Apply time, and the token cache becomes
// per-identity (hashed over the resolved values) so each tenant has its
// own cached token / single-flight slot.
//
// Implements RevocableProvider so revocationOnUnauthorized clears the
// caller's cached token on a 401 from the resource server and re-acquires.
type CredentialsExchangeProvider struct {
	name                     string
	tokenEndpoint            string
	requestCodec             string
	requestFields            map[string]string // static
	requestFieldsFromCtx     map[string]string // body field name → ctx key
	requestHeaders           map[string]string
	responseTokenPath        string
	attach                   AttachConfig
	cacheCfg                 TokenCacheConfig
	revocationOnUnauthorized bool
	httpClient               *http.Client

	// Per-identity registries. When RequestFieldsFromCtx is empty, the
	// identity hash is always "" so all calls share a single entry —
	// behaviorally equivalent to the single-tenant case.
	mu     sync.RWMutex
	caches map[string]*tokenCache
	sfs    map[string]*singleFlight
}

// CredentialsExchangeOptions bundles the constructor parameters.
type CredentialsExchangeOptions struct {
	Name                 string
	TokenEndpoint        string
	RequestCodec         string            // "json" | "form-urlencoded"; default form-urlencoded
	RequestFields        map[string]string // static body fields
	RequestFieldsFromCtx map[string]string // body field → AppContext key; multi-tenant
	RequestHeaders       map[string]string // optional; extra request headers
	ResponseTokenPath    string            // required; dot-notation JSONPath
	Attach               AttachConfig
	Cache                TokenCacheConfig
	RevocationOnUnauthorized bool
	HttpClient               *http.Client
}

// appContextReader is the minimal interface the provider needs from the
// request context when RequestFieldsFromCtx is configured. AppContext
// satisfies it natively (Get is part of its public surface).
type appContextReader interface {
	Get(key string) (any, bool)
}

// NewCredentialsExchangeProvider validates the options and returns a
// ready provider. Either RequestFields or RequestFieldsFromCtx (or both)
// must be non-empty.
func NewCredentialsExchangeProvider(opts CredentialsExchangeOptions) (*CredentialsExchangeProvider, error) {
	if opts.TokenEndpoint == "" {
		return nil, fmt.Errorf("auth: credentials-exchange %q requires tokenEndpoint", opts.Name)
	}
	if len(opts.RequestFields) == 0 && len(opts.RequestFieldsFromCtx) == 0 {
		return nil, fmt.Errorf("auth: credentials-exchange %q requires non-empty requestFields and/or requestFieldsFromCtx", opts.Name)
	}
	if opts.ResponseTokenPath == "" {
		return nil, fmt.Errorf("auth: credentials-exchange %q requires responseTokenPath", opts.Name)
	}
	codec := strings.ToLower(strings.TrimSpace(opts.RequestCodec))
	if codec == "" {
		codec = "form-urlencoded"
	}
	if codec != "json" && codec != "form-urlencoded" {
		return nil, fmt.Errorf("auth: credentials-exchange %q: requestCodec %q is not one of json|form-urlencoded", opts.Name, opts.RequestCodec)
	}
	attach := opts.Attach
	if attach.Kind == AttachUnknown {
		attach.Kind = AttachHeader
	}
	if attach.Name == "" {
		attach.Name = "Authorization"
	}
	if attach.Format == "" {
		attach.Format = "Bearer {token}"
	}
	hc := opts.HttpClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &CredentialsExchangeProvider{
		name:                     opts.Name,
		tokenEndpoint:            opts.TokenEndpoint,
		requestCodec:             codec,
		requestFields:            copyStringMap(opts.RequestFields),
		requestFieldsFromCtx:     copyStringMap(opts.RequestFieldsFromCtx),
		requestHeaders:           copyStringMap(opts.RequestHeaders),
		responseTokenPath:        opts.ResponseTokenPath,
		attach:                   attach,
		cacheCfg:                 opts.Cache,
		revocationOnUnauthorized: opts.RevocationOnUnauthorized,
		httpClient:               hc,
		caches:                   map[string]*tokenCache{},
		sfs:                      map[string]*singleFlight{},
	}, nil
}

func (p *CredentialsExchangeProvider) Name() string { return p.name }

// Apply resolves the dynamic fields (when configured) from the request's
// AppContext, attaches the cached or freshly-acquired token to the
// request via the configured attach.
func (p *CredentialsExchangeProvider) Apply(req *http.Request) error {
	fields, identity, err := p.resolveFields(req.Context())
	if err != nil {
		return err
	}
	token, err := p.getOrAcquireToken(req.Context(), identity, fields)
	if err != nil {
		return err
	}
	Attach(req, p.attach, RenderValue(p.attach.Format, token))
	return nil
}

// Invalidate clears every cached token across identities. Used by the
// auth middleware on a 401 from the resource server — multi-tenant
// callers re-acquire on the next request per tenant.
func (p *CredentialsExchangeProvider) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.caches {
		c.Invalidate()
	}
}

// resolveFields produces the body-field map and the identity hash for
// this call. When RequestFieldsFromCtx is empty, identity is "" so the
// per-identity registries collapse to a single shared slot — behaviorally
// equivalent to a non-multi-tenant provider.
func (p *CredentialsExchangeProvider) resolveFields(ctx context.Context) (map[string]string, string, error) {
	out := make(map[string]string, len(p.requestFields)+len(p.requestFieldsFromCtx))
	for k, v := range p.requestFields {
		out[k] = v
	}
	if len(p.requestFieldsFromCtx) == 0 {
		return out, "", nil
	}
	reader, ok := ctx.(appContextReader)
	if !ok {
		return nil, "", fmt.Errorf("auth: credentials-exchange %q: ctx-sourced fields require AppContext on the request (got %T)", p.name, ctx)
	}
	// Resolve sorted to keep the identity hash stable across runs.
	keys := make([]string, 0, len(p.requestFieldsFromCtx))
	for k := range p.requestFieldsFromCtx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, field := range keys {
		ctxKey := p.requestFieldsFromCtx[field]
		raw, present := reader.Get(ctxKey)
		if !present {
			return nil, "", fmt.Errorf("auth: credentials-exchange %q: AppContext is missing key %q (required by requestFieldsFromCtx)", p.name, ctxKey)
		}
		s, err := ctxValueToString(raw)
		if err != nil {
			return nil, "", fmt.Errorf("auth: credentials-exchange %q: AppContext key %q: %w", p.name, ctxKey, err)
		}
		out[field] = s
		// Hash field|value so two providers whose fields differ but
		// values match by coincidence don't collide.
		h.Write([]byte(field))
		h.Write([]byte{0})
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return out, hex.EncodeToString(h.Sum(nil)), nil
}

// cacheFor / sfFor lazily materialize the per-identity slot. When
// identity is "" (single-tenant), the same slot is reused on every call.
func (p *CredentialsExchangeProvider) cacheFor(identity string) *tokenCache {
	p.mu.RLock()
	if c, ok := p.caches[identity]; ok {
		p.mu.RUnlock()
		return c
	}
	p.mu.RUnlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.caches[identity]; ok {
		return c
	}
	c := &tokenCache{}
	p.caches[identity] = c
	return c
}

func (p *CredentialsExchangeProvider) sfFor(identity string) *singleFlight {
	p.mu.RLock()
	if s, ok := p.sfs[identity]; ok {
		p.mu.RUnlock()
		return s
	}
	p.mu.RUnlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.sfs[identity]; ok {
		return s
	}
	s := &singleFlight{}
	p.sfs[identity] = s
	return s
}

// getOrAcquireToken returns the cached token for the identity or
// acquires a fresh one. Single-flight collapses concurrent misses per
// identity.
func (p *CredentialsExchangeProvider) getOrAcquireToken(ctx context.Context, identity string, fields map[string]string) (string, error) {
	cache := p.cacheFor(identity)
	if tok, ok := cache.Get(p.cacheCfg.Skew); ok {
		return tok, nil
	}
	if p.cacheCfg.SingleFlight {
		return p.sfFor(identity).Do(func() (string, error) {
			if tok, ok := cache.Get(p.cacheCfg.Skew); ok {
				return tok, nil
			}
			return p.acquireToken(ctx, cache, fields)
		})
	}
	return p.acquireToken(ctx, cache, fields)
}

// acquireToken POSTs the credentials body and stores the resulting token
// in the supplied identity-scoped cache.
func (p *CredentialsExchangeProvider) acquireToken(ctx context.Context, cache *tokenCache, fields map[string]string) (string, error) {
	body, contentType, err := p.buildBody(fields)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("auth: credentials-exchange build token request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for k, v := range p.requestHeaders {
		req.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: credentials-exchange token endpoint: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("auth: credentials-exchange read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("auth: credentials-exchange token endpoint returned %d: %s", resp.StatusCode, truncate(string(respBody), 256))
	}
	var payload map[string]any
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("auth: credentials-exchange parse token response: %w", err)
	}
	rawToken, err := walkJSONPath(p.responseTokenPath, payload)
	if err != nil {
		return "", fmt.Errorf("auth: credentials-exchange response: %w", err)
	}
	token, ok := rawToken.(string)
	if !ok || token == "" {
		return "", fmt.Errorf("auth: credentials-exchange: token at %q is not a non-empty string (got %T)", p.responseTokenPath, rawToken)
	}
	expiresAt, err := p.computeExpiry(token, payload)
	if err != nil {
		return "", err
	}
	cache.Set(token, expiresAt)
	return token, nil
}

// buildBody renders the merged field map using the configured codec.
func (p *CredentialsExchangeProvider) buildBody(fields map[string]string) (body, contentType string, err error) {
	switch p.requestCodec {
	case "json":
		raw, err := json.Marshal(fields)
		if err != nil {
			return "", "", fmt.Errorf("auth: credentials-exchange marshal json: %w", err)
		}
		return string(raw), "application/json", nil
	case "form-urlencoded":
		v := url.Values{}
		for k, val := range fields {
			v.Set(k, val)
		}
		return v.Encode(), "application/x-www-form-urlencoded", nil
	}
	return "", "", fmt.Errorf("auth: credentials-exchange: unsupported requestCodec %q", p.requestCodec)
}

// computeExpiry resolves the cached entry's expiresAt per the configured
// source.
func (p *CredentialsExchangeProvider) computeExpiry(token string, payload map[string]any) (time.Time, error) {
	switch p.cacheCfg.Source {
	case SourceJWTExp:
		exp, err := decodeJWTExp(token)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: credentials-exchange jwt-exp: %w", err)
		}
		return exp, nil
	case SourceResponseField:
		raw, err := walkJSONPath(p.cacheCfg.JSONPath, payload)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: credentials-exchange response-field: %w", err)
		}
		return parseResponseFieldExpiry(raw, p.cacheCfg.Unit)
	case SourceTTL:
		return time.Now().Add(p.cacheCfg.TTL), nil
	}
	return time.Time{}, fmt.Errorf("auth: credentials-exchange: no token cache source configured")
}

// ctxValueToString turns an arbitrary AppContext-stored value into the
// string the body codec expects. Supports common scalar types directly;
// other types fall back to fmt.Sprint.
func ctxValueToString(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case *string:
		if x == nil {
			return "", nil
		}
		return *x, nil
	case fmt.Stringer:
		return x.String(), nil
	case bool:
		if x {
			return "true", nil
		}
		return "false", nil
	case int, int32, int64, uint, uint32, uint64, float32, float64:
		return fmt.Sprint(x), nil
	case nil:
		return "", nil
	}
	return fmt.Sprint(v), nil
}

// copyStringMap returns a shallow copy so the registry's source map cannot
// be mutated through the provider's fields.
func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
