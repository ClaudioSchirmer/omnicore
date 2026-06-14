package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ExternalValidatorOptions describes the per-request HTTP call to the IdP
// (typically a token-introspection endpoint, RFC 7662) that catches token
// revocation. Optional even when local JWT validation is enabled — services
// without revocation requirements simply leave it unset.
//
// Cache is opt-in via CacheTTLSeconds and default-off. The default trades one
// HTTP round-trip per authenticated request for immediate revocation; enabling
// the cache reverses the trade-off — fewer IdP calls in exchange for a
// revocation window of up to TTL seconds for tokens that were already cached
// as valid.
type ExternalValidatorOptions struct {
	// Method is the HTTP verb. Currently GET or POST.
	Method string

	// URL is the validator endpoint.
	URL string

	// TokenPlacement names where the token is carried in the outgoing request:
	// bearer_header (Authorization: Bearer ...), form_field
	// (application/x-www-form-urlencoded body), json_body (JSON body), or
	// query_param (query string).
	TokenPlacement string

	// TokenField is the field name when placement is not bearer_header (form
	// key, JSON property, query parameter name). Ignored for bearer_header.
	TokenField string

	// ExtraHeaders are appended to every request (e.g., Basic auth for
	// confidential clients calling Keycloak introspection).
	ExtraHeaders map[string]string

	// Success declares how to read the validator's positive answer from the
	// response body.
	Success ExternalValidatorSuccess

	// TimeoutMS bounds the call. Zero → 2000ms default applied at construction.
	TimeoutMS int

	// FailMode controls the response when the validator itself errors
	// (transport / non-2xx / parse failure). "closed" rejects the request
	// (safer default); "open" accepts on validator error provided the local
	// JWT validation already passed.
	FailMode string

	// CacheTTLSeconds, when > 0, enables an in-memory positive-only cache of
	// successful validator answers keyed by the SHA-256 hash of the bearer
	// token. Negative answers and transport errors are NEVER cached so a
	// revocation hits the IdP on the next request. Default 0 disables the
	// cache entirely (every authenticated request calls the IdP).
	CacheTTLSeconds int
}

// ExternalValidatorSuccess declares the JSONPath in the response body and the
// expected value at that path. Equality is Go's `==` operator on `any`, so
// types and values must both match (`true` != `"true"`).
type ExternalValidatorSuccess struct {
	JSONPath      string
	ExpectedValue any
}

const (
	failModeClosed = "closed"
	failModeOpen   = "open"

	placementBearerHeader = "bearer_header"
	placementFormField    = "form_field"
	placementJSONBody     = "json_body"
	placementQueryParam   = "query_param"
)

// externalValidator is the prepared, request-time form of
// ExternalValidatorOptions.
type externalValidator struct {
	method        string
	url           string
	placement     string
	tokenField    string
	extraHeaders  map[string]string
	jsonPath      []string
	expectedValue any
	failOpen      bool
	client        *http.Client
	cache         *tokenCache // nil when CacheTTLSeconds == 0
}

// tokenCache memorizes successful validator answers for a bounded TTL. Only
// positive answers (Validate returned nil) are stored — negative answers and
// transport errors deliberately bypass the cache so a revocation on the IdP
// side is honored on the next request. Eviction is lazy on read; a stale
// entry is observed once and then dropped.
type tokenCache struct {
	mu  sync.RWMutex
	m   map[string]time.Time // SHA-256 hex digest of the token → expiresAt
	ttl time.Duration
}

func newTokenCache(ttl time.Duration) *tokenCache {
	return &tokenCache{m: map[string]time.Time{}, ttl: ttl}
}

func (c *tokenCache) hit(key string) bool {
	c.mu.RLock()
	exp, ok := c.m[key]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		c.mu.Lock()
		// Re-check under write lock — another goroutine may have refreshed it.
		if cur, stillThere := c.m[key]; stillThere && !time.Now().After(cur) {
			c.mu.Unlock()
			return true
		}
		delete(c.m, key)
		c.mu.Unlock()
		return false
	}
	return true
}

func (c *tokenCache) remember(key string) {
	c.mu.Lock()
	c.m[key] = time.Now().Add(c.ttl)
	c.mu.Unlock()
}

func tokenCacheKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newExternalValidator validates the options and returns a ready-to-call
// externalValidator. Returns an error for any invariant violation so the
// service fails fast at boot rather than per request.
func newExternalValidator(opts ExternalValidatorOptions) (*externalValidator, error) {
	if opts.URL == "" {
		return nil, errors.New("URL is required")
	}
	method := strings.ToUpper(opts.Method)
	switch method {
	case http.MethodGet, http.MethodPost:
	case "":
		method = http.MethodPost
	default:
		return nil, fmt.Errorf("method %q is invalid (expected GET or POST)", opts.Method)
	}
	switch opts.TokenPlacement {
	case placementBearerHeader, placementFormField, placementJSONBody, placementQueryParam:
	default:
		return nil, fmt.Errorf("tokenPlacement %q is invalid", opts.TokenPlacement)
	}
	if opts.TokenPlacement != placementBearerHeader && opts.TokenField == "" {
		return nil, fmt.Errorf("tokenField is required when tokenPlacement=%q", opts.TokenPlacement)
	}
	if opts.Success.JSONPath == "" {
		return nil, errors.New("success.jsonPath is required")
	}
	if opts.Success.ExpectedValue == nil {
		return nil, errors.New("success.expectedValue is required")
	}
	segments, err := parseJSONPath(opts.Success.JSONPath)
	if err != nil {
		return nil, err
	}
	failOpen := false
	switch opts.FailMode {
	case "", failModeClosed:
		failOpen = false
	case failModeOpen:
		failOpen = true
	default:
		return nil, fmt.Errorf("failMode %q is invalid (expected %q or %q)", opts.FailMode, failModeClosed, failModeOpen)
	}
	timeout := time.Duration(opts.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2000 * time.Millisecond
	}
	if opts.CacheTTLSeconds < 0 {
		return nil, fmt.Errorf("cacheTtlSeconds must be >= 0 (got %d)", opts.CacheTTLSeconds)
	}
	var cache *tokenCache
	if opts.CacheTTLSeconds > 0 {
		cache = newTokenCache(time.Duration(opts.CacheTTLSeconds) * time.Second)
	}
	return &externalValidator{
		method:        method,
		url:           opts.URL,
		placement:     opts.TokenPlacement,
		tokenField:    opts.TokenField,
		extraHeaders:  opts.ExtraHeaders,
		jsonPath:      segments,
		expectedValue: opts.Success.ExpectedValue,
		failOpen:      failOpen,
		client:        &http.Client{Timeout: timeout},
		cache:         cache,
	}, nil
}

// Validate calls the IdP and reports whether the token is still active. nil
// → the token passed; non-nil → reject (caller renders 401). Transport,
// non-2xx, and parse errors are coerced to nil when failOpen is set.
//
// When CacheTTLSeconds > 0, a successful answer is memoized for the configured
// TTL keyed by SHA-256(token). Within the TTL the IdP is not called. Negative
// answers and transport errors are never cached.
func (v *externalValidator) Validate(ctx context.Context, token string) error {
	var key string
	if v.cache != nil {
		key = tokenCacheKey(token)
		if v.cache.hit(key) {
			return nil
		}
	}
	if err := v.callIdP(ctx, token); err != nil {
		return err
	}
	if v.cache != nil {
		v.cache.remember(key)
	}
	return nil
}

// callIdP performs the network call and validation without consulting the
// cache. Extracted so Validate can short-circuit on cache hits.
func (v *externalValidator) callIdP(ctx context.Context, token string) error {
	req, err := v.buildRequest(ctx, token)
	if err != nil {
		return v.handleError(fmt.Errorf("build request: %w", err))
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return v.handleError(fmt.Errorf("http: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return v.handleError(fmt.Errorf("non-2xx status: %d", resp.StatusCode))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return v.handleError(fmt.Errorf("read body: %w", err))
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return v.handleError(fmt.Errorf("parse json: %w", err))
	}
	got, ok := lookupJSONPath(payload, v.jsonPath)
	// Path-not-found and value mismatch are NOT transport errors — they are
	// the validator's explicit "this token is not active" answer. failOpen
	// does not convert them to success.
	if !ok {
		return fmt.Errorf("token not active: path %q not found in response", v.formatPath())
	}
	if got != v.expectedValue {
		return fmt.Errorf("token not active: %s=%v, want %v", v.formatPath(), got, v.expectedValue)
	}
	return nil
}

// handleError applies the failMode policy: closed propagates; open swallows.
func (v *externalValidator) handleError(err error) error {
	if v.failOpen {
		return nil
	}
	return fmt.Errorf("external validator: %w", err)
}

func (v *externalValidator) formatPath() string {
	if len(v.jsonPath) == 0 {
		return "$"
	}
	return "$." + strings.Join(v.jsonPath, ".")
}

func (v *externalValidator) buildRequest(ctx context.Context, token string) (*http.Request, error) {
	var body io.Reader
	var contentType string
	targetURL := v.url

	switch v.placement {
	case placementFormField:
		form := url.Values{}
		form.Set(v.tokenField, token)
		body = strings.NewReader(form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case placementJSONBody:
		raw, err := json.Marshal(map[string]string{v.tokenField: token})
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	case placementQueryParam:
		parsed, err := url.Parse(v.url)
		if err != nil {
			return nil, err
		}
		q := parsed.Query()
		q.Set(v.tokenField, token)
		parsed.RawQuery = q.Encode()
		targetURL = parsed.String()
	case placementBearerHeader:
		// no body; header set below
	}

	req, err := http.NewRequestWithContext(ctx, v.method, targetURL, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if v.placement == placementBearerHeader {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, val := range v.extraHeaders {
		req.Header.Set(k, val)
	}
	return req, nil
}

// parseJSONPath turns "$.foo.bar" into ["foo", "bar"]. Supports dot notation
// only; bracket / wildcard syntax is not needed for introspection responses.
func parseJSONPath(path string) ([]string, error) {
	if !strings.HasPrefix(path, "$") {
		return nil, fmt.Errorf("jsonPath %q must start with $", path)
	}
	rest := strings.TrimPrefix(path, "$")
	if rest == "" {
		return nil, nil
	}
	if !strings.HasPrefix(rest, ".") {
		return nil, fmt.Errorf("jsonPath %q segment must start with .", path)
	}
	segs := strings.Split(strings.TrimPrefix(rest, "."), ".")
	for _, s := range segs {
		if s == "" {
			return nil, fmt.Errorf("jsonPath %q has empty segment", path)
		}
	}
	return segs, nil
}

// lookupJSONPath walks the segments through nested map[string]any. Returns
// (value, true) on hit; (nil, false) on path miss or non-map traversal.
func lookupJSONPath(payload any, segments []string) (any, bool) {
	cur := payload
	for _, seg := range segments {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
