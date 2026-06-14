package httpclient

import (
	"fmt"
	"strings"
)

// IdempotencyConfig is the endpoint-level idempotency: block. Presence
// enables idempotency-key injection for the endpoint and — together with
// retry: — unblocks POST/PATCH retry.
type IdempotencyConfig struct {
	Header string `yaml:"header"`
	Source string `yaml:"source"`
}

// idempotencySource enumerates where the per-call idempotency key comes
// from. ctx is the default — the framework generates a UUIDv7 per call.
// explicit requires the caller to pass CallConfig.IdempotencyKey, otherwise the
// call fails before dialing.
type idempotencySource int

const (
	idempotencyDisabled idempotencySource = iota
	idempotencyCtx
	idempotencyExplicit
)

// idempotencyPolicy is the resolved runtime shape consumed by the
// idempotency middleware. Built once per endpoint at New.
type idempotencyPolicy struct {
	enabled bool
	header  string
	source  idempotencySource
}

// resolveIdempotencyPolicy turns the YAML config into the runtime policy.
// Empty config (block absent) yields a disabled policy.
func resolveIdempotencyPolicy(cfg *IdempotencyConfig) idempotencyPolicy {
	if cfg == nil {
		return idempotencyPolicy{}
	}
	src := idempotencyCtx
	switch strings.ToLower(strings.TrimSpace(cfg.Source)) {
	case "", "ctx":
		src = idempotencyCtx
	case "explicit":
		src = idempotencyExplicit
	}
	return idempotencyPolicy{
		enabled: true,
		header:  cfg.Header,
		source:  src,
	}
}

// validateIdempotencyConfig runs schema checks on an idempotency: block.
// Returns the error strings the global validator joins together.
func validateIdempotencyConfig(prefix string, cfg *IdempotencyConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if strings.TrimSpace(cfg.Header) == "" {
		errs = append(errs, fmt.Sprintf("%s.header: required", prefix))
	}
	if cfg.Source != "" {
		switch strings.ToLower(strings.TrimSpace(cfg.Source)) {
		case "ctx", "explicit":
		default:
			errs = append(errs, fmt.Sprintf("%s.source: %q is not one of ctx|explicit", prefix, cfg.Source))
		}
	}
	return errs
}
