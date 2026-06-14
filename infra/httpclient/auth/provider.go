// Package auth holds the outbound auth provider implementations consumed by
// the httpclient subsystem. Providers are package-private at the type level
// — consumers configure named providers in YAML and reference them by name
// on services; there is no public RegisterProvider extension surface.
package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// AuthProvider attaches credentials to the outbound request. Implementations
// are framework-internal. The middleware reads Name() for slog and calls
// Apply on every request that reaches the middleware (cache hits skip it
// because the chain short-circuits before).
type AuthProvider interface {
	// Name is the provider name as declared in YAML, surfaced on the slog
	// observation under "authProvider".
	Name() string

	// Apply mutates the request to carry the credential. Static providers
	// always succeed; dynamic providers (forward-bearer, oauth2-*) may
	// fail at token acquisition.
	Apply(req *http.Request) error
}

// AttachConfig is the resolved attach block. Every provider type accepts an
// attach: configuration that decides where the credential goes — request
// header (default), query parameter, or cookie.
type AttachConfig struct {
	Kind   AttachKind
	Name   string
	Format string // optional; "{token}" placeholder; empty means raw value
	Value  string // header-static: raw value to attach
}

// AttachKind is the placement of the credential on the request.
type AttachKind int

const (
	AttachUnknown AttachKind = iota
	AttachHeader
	AttachQuery
	AttachCookie
)

// ParseAttachKind reads the YAML scalar (header|query|cookie) into the
// enum. An empty input defaults to header, matching the design's default.
func ParseAttachKind(s string) (AttachKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "header":
		return AttachHeader, nil
	case "query":
		return AttachQuery, nil
	case "cookie":
		return AttachCookie, nil
	default:
		return AttachUnknown, fmt.Errorf("auth: attach.as %q is not one of header|query|cookie", s)
	}
}

// renderValue resolves the attach.format template against the supplied
// token. The placeholder is the literal "{token}"; absence of a format
// returns the token verbatim. Used by token-bearing providers (bearer-static,
// forward-bearer, oauth2).
func RenderValue(format, token string) string {
	if format == "" {
		return token
	}
	return strings.ReplaceAll(format, "{token}", token)
}

// Attach applies the rendered value to the request via the configured kind.
// Header: Set. Query: append. Cookie: Add.
func Attach(req *http.Request, cfg AttachConfig, value string) {
	switch cfg.Kind {
	case AttachHeader:
		req.Header.Set(cfg.Name, value)
	case AttachQuery:
		q := req.URL.Query()
		q.Set(cfg.Name, value)
		req.URL.RawQuery = q.Encode()
	case AttachCookie:
		req.AddCookie(&http.Cookie{Name: cfg.Name, Value: value})
	}
}
