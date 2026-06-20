package httpclient

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/ClaudioSchirmer/omnicore/infra/httpclient/auth"
)

// AuthProviderConfig is the YAML shape for a single named provider under
// httpClient.authProviders. The active fields depend on Type — Validate
// rejects mismatches up front.
type AuthProviderConfig struct {
	Type string `yaml:"type"`

	Attach *AttachConfig `yaml:"attach,omitempty"`

	// header-static
	// (uses attach.value)

	// bearer-static
	Token string `yaml:"token,omitempty"`

	// basic
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`

	// oauth2-client-credentials
	TokenEndpoint            string             `yaml:"tokenEndpoint,omitempty"`
	ClientID                 string             `yaml:"clientId,omitempty"`
	ClientSecret             string             `yaml:"clientSecret,omitempty"`
	Scope                    []string           `yaml:"scope,omitempty"`
	Audience                 string             `yaml:"audience,omitempty"`
	TokenCache               *TokenCacheConfig  `yaml:"tokenCache,omitempty"`
	RevocationOnUnauthorized bool               `yaml:"revocationOnUnauthorized,omitempty"`

	// credentials-exchange — generic "POST credentials, get token" path.
	// Use when the IdP diverges from RFC 6749: custom field names, JSON
	// body, non-standard response shape.
	RequestCodec  string            `yaml:"requestCodec,omitempty"`  // json | form-urlencoded
	RequestFields map[string]string `yaml:"requestFields,omitempty"` // static key/value to POST

	// RequestFieldsFromCtx maps body field names to AppContext keys; the
	// provider reads the values at Apply time and the token cache becomes
	// per-identity (hashed over the resolved values). Merged with
	// RequestFields; on conflict, ctx wins. When empty, the provider
	// behaves as single-tenant (one shared cache entry).
	RequestFieldsFromCtx map[string]string `yaml:"requestFieldsFromCtx,omitempty"`

	RequestHeaders    map[string]string `yaml:"requestHeaders,omitempty"`    // extra request headers (e.g. Basic Auth)
	ResponseTokenPath string            `yaml:"responseTokenPath,omitempty"` // JSONPath ($.access_token)
}

// TokenCacheConfig is the YAML shape for the per-provider tokenCache block.
// Source decides how the cached entry's expiry is computed.
type TokenCacheConfig struct {
	Source       string   `yaml:"source"`
	Skew         Duration `yaml:"skew"`
	SingleFlight *bool    `yaml:"singleFlight"`

	// response-field
	JSONPath string `yaml:"jsonPath"`
	Unit     string `yaml:"unit"`

	// ttl
	TTL Duration `yaml:"ttl"`
}

// AttachConfig is the YAML shape for the per-provider attach block.
type AttachConfig struct {
	As     string `yaml:"as"`
	Name   string `yaml:"name"`
	Format string `yaml:"format"`
	Value  string `yaml:"value"`
}

// ServiceAuthConfig is the YAML shape for service.auth.
type ServiceAuthConfig struct {
	Provider string `yaml:"provider"`
}

// supportedAuthTypes is the closed set of provider types accepted in the
// current phase. Future phases add forward-bearer + oauth2-*.
var supportedAuthTypes = map[string]struct{}{
	"none":                      {},
	"header-static":             {},
	"bearer-static":             {},
	"basic":                     {},
	"forward-bearer":            {},
	"oauth2-client-credentials": {},
	"credentials-exchange":      {},
}

// futureAuthTypes maps not-yet-supported provider types to the phase that
// introduces them, so the validator can give an actionable rejection.
var futureAuthTypes = map[string]string{
	"oauth2-password": "oauth2 password grant phase",
	"oauth2-refresh":  "oauth2 refresh grant phase",
}

// validateAuthProviders runs schema checks on the top-level
// authProviders block.
func validateAuthProviders(cfg map[string]AuthProviderConfig) []string {
	if len(cfg) == 0 {
		return nil
	}
	var errs []string
	for name, p := range cfg {
		prefix := "httpClient.authProviders." + name
		t := strings.ToLower(strings.TrimSpace(p.Type))
		if t == "" {
			errs = append(errs, fmt.Sprintf("%s.type: required", prefix))
			continue
		}
		if phase, ok := futureAuthTypes[t]; ok {
			errs = append(errs, fmt.Sprintf("%s.type: %q is not yet supported (introduced with the %s)", prefix, p.Type, phase))
			continue
		}
		if _, ok := supportedAuthTypes[t]; !ok {
			errs = append(errs, fmt.Sprintf("%s.type: %q is not a recognized provider type", prefix, p.Type))
			continue
		}
		errs = append(errs, validateAuthProviderShape(prefix, t, p)...)
	}
	return errs
}

// validateAuthProviderShape checks the type-specific field requirements
// once the type itself is known and supported.
// isAbsoluteURL mirrors the services.<name>.baseURL check in validate.go so a
// token endpoint is held to the same standard as a service base URL — a
// typo'd scheme or a host-less value is rejected at boot rather than on the
// first token acquisition.
func isAbsoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func validateAuthProviderShape(prefix, t string, p AuthProviderConfig) []string {
	var errs []string
	if p.Attach != nil {
		if _, err := auth.ParseAttachKind(p.Attach.As); err != nil {
			errs = append(errs, fmt.Sprintf("%s.attach.as: %v", prefix, err))
		}
	}
	switch t {
	case "header-static":
		if p.Attach == nil || p.Attach.Name == "" {
			errs = append(errs, fmt.Sprintf("%s.attach.name: required for header-static", prefix))
		}
		if p.Attach == nil || p.Attach.Value == "" {
			errs = append(errs, fmt.Sprintf("%s.attach.value: required for header-static", prefix))
		}
	case "bearer-static":
		if p.Token == "" {
			errs = append(errs, fmt.Sprintf("%s.token: required for bearer-static", prefix))
		}
	case "basic":
		if p.Username == "" {
			errs = append(errs, fmt.Sprintf("%s.username: required for basic", prefix))
		}
		if p.Password == "" {
			errs = append(errs, fmt.Sprintf("%s.password: required for basic", prefix))
		}
	case "oauth2-client-credentials":
		if p.TokenEndpoint == "" {
			errs = append(errs, fmt.Sprintf("%s.tokenEndpoint: required for oauth2-client-credentials", prefix))
		} else if !isAbsoluteURL(p.TokenEndpoint) {
			errs = append(errs, fmt.Sprintf("%s.tokenEndpoint: %q is not a valid absolute URL", prefix, p.TokenEndpoint))
		}
		if p.ClientID == "" {
			errs = append(errs, fmt.Sprintf("%s.clientId: required for oauth2-client-credentials", prefix))
		}
		if p.ClientSecret == "" {
			errs = append(errs, fmt.Sprintf("%s.clientSecret: required for oauth2-client-credentials", prefix))
		}
		errs = append(errs, validateTokenCacheConfig(prefix+".tokenCache", p.TokenCache)...)
	case "credentials-exchange":
		if p.TokenEndpoint == "" {
			errs = append(errs, fmt.Sprintf("%s.tokenEndpoint: required for credentials-exchange", prefix))
		} else if !isAbsoluteURL(p.TokenEndpoint) {
			errs = append(errs, fmt.Sprintf("%s.tokenEndpoint: %q is not a valid absolute URL", prefix, p.TokenEndpoint))
		}
		if len(p.RequestFields) == 0 && len(p.RequestFieldsFromCtx) == 0 {
			errs = append(errs, fmt.Sprintf("%s: requires requestFields and/or requestFieldsFromCtx (non-empty)", prefix))
		}
		if p.ResponseTokenPath == "" {
			errs = append(errs, fmt.Sprintf("%s.responseTokenPath: required for credentials-exchange", prefix))
		} else if !strings.HasPrefix(strings.TrimSpace(p.ResponseTokenPath), "$") {
			errs = append(errs, fmt.Sprintf("%s.responseTokenPath: %q must start with $", prefix, p.ResponseTokenPath))
		}
		if p.RequestCodec != "" {
			switch strings.ToLower(strings.TrimSpace(p.RequestCodec)) {
			case "json", "form-urlencoded":
			default:
				errs = append(errs, fmt.Sprintf("%s.requestCodec: %q is not one of json|form-urlencoded", prefix, p.RequestCodec))
			}
		}
		errs = append(errs, validateTokenCacheConfig(prefix+".tokenCache", p.TokenCache)...)
	}
	return errs
}

// validateTokenCacheConfig runs schema checks on the tokenCache block of
// a token-caching provider (oauth2-client-credentials and credentials-exchange
// share the same shape). The prefix carries the provider name, so the
// caller's path already identifies which provider is being rejected.
func validateTokenCacheConfig(prefix string, cfg *TokenCacheConfig) []string {
	if cfg == nil {
		return []string{fmt.Sprintf("%s: required", prefix)}
	}
	var errs []string
	switch strings.ToLower(strings.TrimSpace(cfg.Source)) {
	case "jwt-exp", "response-field", "ttl":
	case "":
		errs = append(errs, fmt.Sprintf("%s.source: required (jwt-exp|response-field|ttl)", prefix))
	default:
		errs = append(errs, fmt.Sprintf("%s.source: %q is not one of jwt-exp|response-field|ttl", prefix, cfg.Source))
	}
	if cfg.Skew < 0 {
		errs = append(errs, fmt.Sprintf("%s.skew: must be non-negative", prefix))
	}
	source := strings.ToLower(strings.TrimSpace(cfg.Source))
	switch source {
	case "response-field":
		if cfg.JSONPath == "" {
			errs = append(errs, fmt.Sprintf("%s.jsonPath: required when source=response-field", prefix))
		}
		if cfg.Unit != "" {
			switch strings.ToLower(strings.TrimSpace(cfg.Unit)) {
			case "seconds", "millis", "iso8601":
			default:
				errs = append(errs, fmt.Sprintf("%s.unit: %q is not one of seconds|millis|iso8601", prefix, cfg.Unit))
			}
		}
	case "ttl":
		if cfg.TTL <= 0 {
			errs = append(errs, fmt.Sprintf("%s.ttl: must be positive when source=ttl", prefix))
		}
	}
	return errs
}

// validateServiceAuthReferences checks that every service.auth.provider
// references a declared authProviders entry. Called after
// validateAuthProviders so error ordering is stable.
func validateServiceAuthReferences(services map[string]ServiceConfig, providers map[string]AuthProviderConfig) []string {
	var errs []string
	for name, sc := range services {
		if sc.Auth == nil {
			continue
		}
		if sc.Auth.Provider == "" {
			errs = append(errs, fmt.Sprintf("httpClient.services.%s.auth.provider: required", name))
			continue
		}
		if _, ok := providers[sc.Auth.Provider]; !ok {
			errs = append(errs, fmt.Sprintf("httpClient.services.%s.auth.provider: %q is not declared under httpClient.authProviders", name, sc.Auth.Provider))
		}
	}
	return errs
}

// toRuntimeTokenCacheConfig translates the YAML TokenCacheConfig into the
// runtime auth.TokenCacheConfig consumed by the OAuth2 provider.
func toRuntimeTokenCacheConfig(cfg *TokenCacheConfig) auth.TokenCacheConfig {
	out := auth.TokenCacheConfig{
		Skew: cfg.Skew.ToTime(),
		TTL:  cfg.TTL.ToTime(),
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Source)) {
	case "jwt-exp":
		out.Source = auth.SourceJWTExp
	case "response-field":
		out.Source = auth.SourceResponseField
		out.JSONPath = cfg.JSONPath
		switch strings.ToLower(strings.TrimSpace(cfg.Unit)) {
		case "", "seconds":
			out.Unit = auth.UnitSeconds
		case "millis":
			out.Unit = auth.UnitMillis
		case "iso8601":
			out.Unit = auth.UnitISO8601
		}
	case "ttl":
		out.Source = auth.SourceTTL
	}
	if cfg.SingleFlight != nil {
		out.SingleFlight = *cfg.SingleFlight
	} else {
		// Design Section 6 implies singleFlight is the default for token
		// caches — protect the token endpoint by default.
		out.SingleFlight = true
	}
	return out
}

// toRuntimeAttach converts the YAML AttachConfig into the runtime auth.AttachConfig.
func toRuntimeAttach(cfg *AttachConfig) auth.AttachConfig {
	if cfg == nil {
		return auth.AttachConfig{}
	}
	kind, _ := auth.ParseAttachKind(cfg.As)
	return auth.AttachConfig{
		Kind:   kind,
		Name:   cfg.Name,
		Format: cfg.Format,
		Value:  cfg.Value,
	}
}
