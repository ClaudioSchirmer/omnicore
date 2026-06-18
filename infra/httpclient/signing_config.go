package httpclient

import (
	"fmt"
	"strings"
)

// Supported timestamp formats for the signing block. The framework injects
// the chosen timestampHeader with the current UTC time formatted per this
// enum. New formats land here as the upstream catalog widens.
const (
	timestampFormatRFC1123    = "rfc1123"
	timestampFormatISO8601    = "iso8601"
	timestampFormatUnixSecond = "unix-seconds"
)

// SigningConfig is the YAML shape for the per-service signing: block. It
// describes an HMAC request-signing policy compatible with payment
// gateways, Twilio-style webhooks, and AWS SigV4-lite upstreams.
//
// All fields are validated at boot via validateSigningConfig; the
// rejection messages list every issue at once so the operator sees the
// whole policy state on a single boot attempt.
type SigningConfig struct {
	// Type is the signing scheme. Phase 6 supports "hmac-sha256" only;
	// additional schemes (hmac-sha512, hmac-sha1, asymmetric) arrive in
	// dedicated phases without changing this field's semantics.
	Type string `yaml:"type"`

	// KeyId identifies the signing key on the upstream side. Optional —
	// some APIs derive the key from the URL/host instead. When set,
	// KeyIdHeader is the header the framework uses to send it.
	KeyId string `yaml:"keyId"`

	// KeyIdHeader is the outbound header name carrying KeyId. Required
	// when KeyId is set; ignored otherwise.
	KeyIdHeader string `yaml:"keyIdHeader"`

	// Secret is the HMAC secret. Required. Typically interpolated from
	// env or vault via the canonical ${...} forms.
	Secret string `yaml:"secret"`

	// SignedHeaders lists which headers participate in the canonical
	// string. Required, non-empty. Order is normalized internally —
	// the framework lowercases each name and sorts alphabetically before
	// signing. Only headers present on the request when the signing
	// middleware runs (slot 8 of the chain) can be referenced: host,
	// authorization (auth middleware at slot 3), idempotency key
	// (idempotency middleware at slot 4), content-type/headers cascade,
	// the framework-injected timestamp and content-sha256 headers, plus
	// any WithExtraHeader / CallConfig override.
	SignedHeaders []string `yaml:"signedHeaders"`

	// TimestampHeader is the outbound header name where the framework
	// writes the current UTC time before signing. Required. The header
	// is added to the request immediately before signing runs.
	TimestampHeader string `yaml:"timestampHeader"`

	// TimestampFormat is the time format written into TimestampHeader.
	// Optional; defaults to rfc1123 ("Mon, 02 Jan 2006 15:04:05 GMT").
	// Accepts rfc1123 | iso8601 | unix-seconds.
	TimestampFormat string `yaml:"timestampFormat"`

	// ContentSHA256Header is the outbound header name where the framework
	// writes hex(SHA256(body)). Optional; defaults to "X-Content-SHA256".
	// Set to the empty string explicitly to disable injection (some APIs
	// expect the SHA only in the canonical string, not on the wire).
	ContentSHA256Header string `yaml:"contentSHA256Header"`

	// SignatureHeader is the outbound header name where the framework
	// writes the final signature. Required.
	SignatureHeader string `yaml:"signatureHeader"`

	// SignaturePrefix is an optional literal prefix added to the
	// SignatureHeader value before the hex signature. Empty by default;
	// common values include "HMAC-SHA256 " when an upstream expects the
	// scheme name embedded in the header value.
	SignaturePrefix string `yaml:"signaturePrefix"`
}

// signingPolicy is the resolved runtime shape consumed by signingMiddleware.
// Built once at New from a validated SigningConfig so the request path
// performs no map lookups or string parsing.
type signingPolicy struct {
	enabled              bool
	algorithm            string
	keyId                string
	keyIdHeader          string
	secret               []byte
	signedHeaders        []string // lowercase, sorted
	timestampHeader      string
	timestampFormat      string
	contentSHA256Header  string // empty means do not inject
	signatureHeader      string
	signaturePrefix      string
}

// disabled reports whether the policy is a no-op. Used by buildChain to
// skip adding the signing middleware entirely when the service did not
// declare a signing: block.
func (p signingPolicy) disabled() bool {
	return !p.enabled
}

// resolveSigningPolicy turns the YAML config into the runtime policy
// struct. Returns a disabled policy when cfg is nil so the chain skips
// the middleware cleanly.
func resolveSigningPolicy(cfg *SigningConfig) signingPolicy {
	if cfg == nil {
		return signingPolicy{}
	}
	p := signingPolicy{
		enabled:             true,
		algorithm:           strings.ToLower(strings.TrimSpace(cfg.Type)),
		keyId:               cfg.KeyId,
		keyIdHeader:         cfg.KeyIdHeader,
		secret:              []byte(cfg.Secret),
		signedHeaders:       normalizeSignedHeaders(cfg.SignedHeaders),
		timestampHeader:     cfg.TimestampHeader,
		timestampFormat:     resolveTimestampFormat(cfg.TimestampFormat),
		contentSHA256Header: resolveContentSHA256Header(cfg.ContentSHA256Header),
		signatureHeader:     cfg.SignatureHeader,
		signaturePrefix:     cfg.SignaturePrefix,
	}
	return p
}

// normalizeSignedHeaders lowercases and sorts the signedHeaders list so
// the canonical string is deterministic regardless of YAML ordering.
func normalizeSignedHeaders(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, h := range in {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	sortStrings(out)
	return out
}

// resolveTimestampFormat applies the default when the YAML left the field
// empty. Validate has already rejected unknown values upstream, so the
// only branches here are the three accepted formats.
func resolveTimestampFormat(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case timestampFormatISO8601, timestampFormatUnixSecond:
		return v
	default:
		return timestampFormatRFC1123
	}
}

// resolveContentSHA256Header applies the default header name when the
// YAML left the field empty and was not the explicit "-" sentinel.
// Explicit empty string in YAML is preserved as "disable injection".
func resolveContentSHA256Header(s string) string {
	// Empty in the YAML means "use default". Operators that want to
	// disable injection set the value to the literal "-" (we keep "" as
	// "use default" so omitting the field works the same as not having
	// it). The middleware treats the empty string returned here as
	// "inject the default name"; "-" returns empty downstream to skip
	// injection. Documented in the docs site (httpclient section).
	t := strings.TrimSpace(s)
	if t == "" {
		return "X-Content-SHA256"
	}
	if t == "-" {
		return ""
	}
	return t
}

// validateSigningConfig runs schema checks on the YAML signing: block.
// Returns a slice of error strings the caller accumulates into the global
// validation accumulator. Returns nil when cfg is nil (signing is opt-in).
func validateSigningConfig(prefix string, cfg *SigningConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	algo := strings.ToLower(strings.TrimSpace(cfg.Type))
	switch algo {
	case "hmac-sha256":
	case "":
		errs = append(errs, fmt.Sprintf("%s.type: required", prefix))
	default:
		errs = append(errs, fmt.Sprintf("%s.type: %q is not supported (only hmac-sha256 in this phase)", prefix, cfg.Type))
	}
	if strings.TrimSpace(cfg.Secret) == "" {
		errs = append(errs, fmt.Sprintf("%s.secret: required", prefix))
	}
	if len(cfg.SignedHeaders) == 0 {
		errs = append(errs, fmt.Sprintf("%s.signedHeaders: required and must be non-empty", prefix))
	} else {
		for _, h := range cfg.SignedHeaders {
			if strings.TrimSpace(h) == "" {
				errs = append(errs, fmt.Sprintf("%s.signedHeaders: contains an empty entry", prefix))
				break
			}
		}
	}
	if strings.TrimSpace(cfg.TimestampHeader) == "" {
		errs = append(errs, fmt.Sprintf("%s.timestampHeader: required", prefix))
	}
	if cfg.TimestampFormat != "" {
		switch strings.ToLower(strings.TrimSpace(cfg.TimestampFormat)) {
		case timestampFormatRFC1123, timestampFormatISO8601, timestampFormatUnixSecond:
		default:
			errs = append(errs, fmt.Sprintf("%s.timestampFormat: %q is not one of rfc1123|iso8601|unix-seconds", prefix, cfg.TimestampFormat))
		}
	}
	if strings.TrimSpace(cfg.SignatureHeader) == "" {
		errs = append(errs, fmt.Sprintf("%s.signatureHeader: required", prefix))
	}
	if strings.TrimSpace(cfg.KeyId) != "" && strings.TrimSpace(cfg.KeyIdHeader) == "" {
		errs = append(errs, fmt.Sprintf("%s.keyIdHeader: required when keyId is set", prefix))
	}
	if strings.TrimSpace(cfg.KeyIdHeader) != "" && strings.TrimSpace(cfg.KeyId) == "" {
		errs = append(errs, fmt.Sprintf("%s.keyId: required when keyIdHeader is set", prefix))
	}
	return errs
}

// sortStrings is a tiny shim — keeps the file free of an extra import for
// callers that only need ascending lexicographic order.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
