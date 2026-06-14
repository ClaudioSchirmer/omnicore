package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TokenCacheConfig is the runtime view of the tokenCache YAML block
// consumed by the OAuth2 provider. Source decides how the expiry is
// computed; the per-source fields (JSONPath, Unit, TTL) are consulted
// only when relevant.
type TokenCacheConfig struct {
	Source       TokenCacheSource
	Skew         time.Duration
	SingleFlight bool

	// response-field source
	JSONPath string
	Unit     ResponseFieldUnit

	// ttl source
	TTL time.Duration
}

// TokenCacheSource enumerates the supported TTL-resolution strategies.
type TokenCacheSource int

const (
	SourceUnknown TokenCacheSource = iota
	SourceJWTExp
	SourceResponseField
	SourceTTL
)

// ResponseFieldUnit interprets the value at JSONPath when Source is
// response-field. seconds and millis are integers from "now"; iso8601 is
// an absolute timestamp.
type ResponseFieldUnit int

const (
	UnitUnknown ResponseFieldUnit = iota
	UnitSeconds
	UnitMillis
	UnitISO8601
)

// OAuth2ClientCredentialsProvider implements the OAuth2 client_credentials
// grant. The token endpoint is called via POST application/x-www-form-urlencoded;
// the response is parsed for the access_token and the configured
// TokenCacheSource decides the expiry.
//
// Implements RevocableProvider so the auth middleware can clear the cache
// and re-acquire on a 401 from the resource server.
type OAuth2ClientCredentialsProvider struct {
	name                     string
	tokenEndpoint            string
	clientID, clientSecret   string
	scope                    []string
	audience                 string
	attach                   AttachConfig
	cache                    tokenCache
	cacheCfg                 TokenCacheConfig
	revocationOnUnauthorized bool
	httpClient               *http.Client
	sf                       singleFlight
}

// OAuth2Options bundles the constructor parameters so the call site reads
// naturally without a long positional argument list.
type OAuth2Options struct {
	Name                     string
	TokenEndpoint            string
	ClientID, ClientSecret   string
	Scope                    []string
	Audience                 string
	Attach                   AttachConfig
	Cache                    TokenCacheConfig
	RevocationOnUnauthorized bool

	// HttpClient is the *http.Client used for the token endpoint call.
	// Tests pass an httptest-backed client; production wiring passes
	// nil so the constructor falls back to a default client with a
	// sensible timeout.
	HttpClient *http.Client
}

// NewOAuth2ClientCredentialsProvider applies design defaults and returns a
// ready provider. Validates that the runtime configuration matches what
// the YAML schema requires.
func NewOAuth2ClientCredentialsProvider(opts OAuth2Options) (*OAuth2ClientCredentialsProvider, error) {
	if opts.TokenEndpoint == "" {
		return nil, fmt.Errorf("auth: oauth2 %q requires tokenEndpoint", opts.Name)
	}
	if opts.ClientID == "" || opts.ClientSecret == "" {
		return nil, fmt.Errorf("auth: oauth2 %q requires clientId and clientSecret", opts.Name)
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
	return &OAuth2ClientCredentialsProvider{
		name:                     opts.Name,
		tokenEndpoint:            opts.TokenEndpoint,
		clientID:                 opts.ClientID,
		clientSecret:             opts.ClientSecret,
		scope:                    opts.Scope,
		audience:                 opts.Audience,
		attach:                   attach,
		cacheCfg:                 opts.Cache,
		revocationOnUnauthorized: opts.RevocationOnUnauthorized,
		httpClient:               hc,
	}, nil
}

func (p *OAuth2ClientCredentialsProvider) Name() string { return p.name }

// Apply attaches the cached or freshly-acquired token to the request via
// the configured attach. The single-flight ensures concurrent misses do
// not stampede the token endpoint.
func (p *OAuth2ClientCredentialsProvider) Apply(req *http.Request) error {
	token, err := p.getOrAcquireToken(req.Context())
	if err != nil {
		return err
	}
	Attach(req, p.attach, RenderValue(p.attach.Format, token))
	return nil
}

// Invalidate clears the cached token so the next call re-acquires from
// the token endpoint. Used by the auth middleware under
// revocationOnUnauthorized.
func (p *OAuth2ClientCredentialsProvider) Invalidate() {
	p.cache.Invalidate()
}

// getOrAcquireToken returns the cached token when valid or acquires a
// fresh one. Single-flight collapses concurrent acquisitions.
func (p *OAuth2ClientCredentialsProvider) getOrAcquireToken(ctx context.Context) (string, error) {
	if tok, ok := p.cache.Get(p.cacheCfg.Skew); ok {
		return tok, nil
	}
	if p.cacheCfg.SingleFlight {
		return p.sf.Do(func() (string, error) {
			// Re-check after acquiring the slot — another goroutine may have
			// already populated the cache.
			if tok, ok := p.cache.Get(p.cacheCfg.Skew); ok {
				return tok, nil
			}
			return p.acquireToken(ctx)
		})
	}
	return p.acquireToken(ctx)
}

// acquireToken POSTs to the token endpoint with the client_credentials
// grant body and stores the resulting token in the cache.
func (p *OAuth2ClientCredentialsProvider) acquireToken(ctx context.Context) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)
	if len(p.scope) > 0 {
		form.Set("scope", strings.Join(p.scope, " "))
	}
	if p.audience != "" {
		form.Set("audience", p.audience)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("auth: oauth2 build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("auth: oauth2 token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("auth: oauth2 read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("auth: oauth2 token endpoint returned %d: %s", resp.StatusCode, truncate(string(body), 256))
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("auth: oauth2 parse token response: %w", err)
	}
	tokenAny, ok := payload["access_token"]
	if !ok {
		return "", fmt.Errorf("auth: oauth2 response missing access_token")
	}
	token, ok := tokenAny.(string)
	if !ok || token == "" {
		return "", fmt.Errorf("auth: oauth2 access_token is not a non-empty string")
	}
	expiresAt, err := p.computeExpiry(token, payload)
	if err != nil {
		return "", err
	}
	p.cache.Set(token, expiresAt)
	return token, nil
}

// computeExpiry resolves the cached entry's expiresAt per the configured
// source.
func (p *OAuth2ClientCredentialsProvider) computeExpiry(token string, payload map[string]any) (time.Time, error) {
	switch p.cacheCfg.Source {
	case SourceJWTExp:
		exp, err := decodeJWTExp(token)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: oauth2 jwt-exp: %w", err)
		}
		return exp, nil
	case SourceResponseField:
		raw, err := walkJSONPath(p.cacheCfg.JSONPath, payload)
		if err != nil {
			return time.Time{}, fmt.Errorf("auth: oauth2 response-field: %w", err)
		}
		return parseResponseFieldExpiry(raw, p.cacheCfg.Unit)
	case SourceTTL:
		return time.Now().Add(p.cacheCfg.TTL), nil
	}
	return time.Time{}, fmt.Errorf("auth: oauth2: no token cache source configured")
}

// parseResponseFieldExpiry interprets the value at JSONPath according to
// the configured unit and returns the absolute expiry time.
func parseResponseFieldExpiry(raw any, unit ResponseFieldUnit) (time.Time, error) {
	switch unit {
	case UnitSeconds, UnitUnknown:
		n, err := numberOf(raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("seconds unit: %w", err)
		}
		return time.Now().Add(time.Duration(n) * time.Second), nil
	case UnitMillis:
		n, err := numberOf(raw)
		if err != nil {
			return time.Time{}, fmt.Errorf("millis unit: %w", err)
		}
		return time.Now().Add(time.Duration(n) * time.Millisecond), nil
	case UnitISO8601:
		s, ok := raw.(string)
		if !ok {
			return time.Time{}, fmt.Errorf("iso8601 unit: expected string, got %T", raw)
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Time{}, fmt.Errorf("iso8601 unit: %w", err)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unsupported unit")
}

func numberOf(v any) (int64, error) {
	switch x := v.(type) {
	case float64:
		return int64(x), nil
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case json.Number:
		return x.Int64()
	case string:
		return strconv.ParseInt(x, 10, 64)
	}
	return 0, fmt.Errorf("not a number: %T", v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
