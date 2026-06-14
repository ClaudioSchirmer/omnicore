package httpclient

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Framework defaults applied when the retry: block is present but a field is
// not. Idiomatic for production: three attempts with exponential-jitter,
// 100ms-5s envelope, retry on the common transient surface.
const (
	frameworkRetryMaxAttempts    = 1 // no retry by default when block is absent
	frameworkRetryInitialDelay   = 100 * time.Millisecond
	frameworkRetryMaxDelay       = 5 * time.Second
	frameworkRetryBackoffName    = "exponential-jitter"
	frameworkRetryRespectAfter   = true
	frameworkRetryEnabledDefault = 3 // when retry: block is present without maxAttempts
)

// frameworkRetryRetryOnDefault is the default retryOn set applied when the
// YAML block is present but the field is empty. Status codes are listed by
// numeric value; sentinels are listed as keywords.
var frameworkRetryRetryOnDefault = []string{"502", "503", "504", "network", "timeout"}

// RetryConfig is the YAML shape for retry: blocks under defaults and under
// endpoints. Endpoint values override defaults field-by-field; framework
// defaults fill in the gaps. Validation runs at New so a misconfigured
// retry block aborts the boot rather than silently dropping behavior.
type RetryConfig struct {
	MaxAttempts       int      `yaml:"maxAttempts"`
	Backoff           string   `yaml:"backoff"`
	InitialDelay      Duration `yaml:"initialDelay"`
	MaxDelay          Duration `yaml:"maxDelay"`
	RetryOn           []string `yaml:"retryOn"`
	RespectRetryAfter *bool    `yaml:"respectRetryAfter"`
}

// methodAllowsRetry reports whether the HTTP method can be retried under the
// current phase. POST and PATCH return false until the idempotency phase
// ships an opt-in mechanism — until then, retrying them risks double-charge
// semantics on the upstream side.
func methodAllowsRetry(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "PUT", "DELETE":
		return true
	}
	return false
}

// resolveRetryOverride builds a runtime retryPolicy from a per-call
// RetryOverride supplied via CallConfig.Retry. The override fully replaces
// the YAML-resolved policy — there is no field-level merge with the
// endpoint or defaults block; that would surprise callers who expect
// "I supplied a policy, this is what runs". The POST/PATCH gate still
// applies: even an override forcing maxAttempts > 1 is clamped to 1
// unless the endpoint declares idempotency, because the physical safety
// constraint (no double-charge) is independent of who chose the policy.
func resolveRetryOverride(method string, override *RetryOverride, hasIdempotency bool) retryPolicy {
	cfg := &RetryConfig{
		MaxAttempts:  override.MaxAttempts,
		Backoff:      override.Backoff,
		InitialDelay: Duration(override.InitialDelay),
		MaxDelay:     Duration(override.MaxDelay),
		RetryOn:      override.RetryOn,
	}
	if override.RespectRetryAfter != nil {
		v := *override.RespectRetryAfter
		cfg.RespectRetryAfter = &v
	}
	policy := policyFromConfig(cfg)
	if !methodAllowsRetry(method) && !hasIdempotency {
		policy.maxAttempts = 1
	}
	return policy
}

// resolveRetryPolicy merges (defaults, endpoint) into a runtime retryPolicy.
// Endpoint values override defaults field by field. POST/PATCH are forced to
// maxAttempts: 1 unless an idempotency block is declared on the endpoint —
// when hasIdempotency is true, the framework trusts that the upstream
// dedupes by the injected key and allows the configured maxAttempts to
// take effect. When neither defaults nor endpoint declares a retry block,
// the result is an effectively disabled policy (maxAttempts: 1).
func resolveRetryPolicy(method string, defaults, endpoint *RetryConfig, hasIdempotency bool) retryPolicy {
	merged := mergeRetryConfig(defaults, endpoint)
	if merged == nil {
		return retryPolicy{maxAttempts: 1}
	}
	policy := policyFromConfig(merged)
	if !methodAllowsRetry(method) && !hasIdempotency {
		policy.maxAttempts = 1
	}
	return policy
}

// mergeRetryConfig returns a fresh *RetryConfig with field-level override
// semantics. Returns nil when both inputs are nil so the caller knows no
// block was ever present.
func mergeRetryConfig(defaults, endpoint *RetryConfig) *RetryConfig {
	if defaults == nil && endpoint == nil {
		return nil
	}
	out := &RetryConfig{}
	if defaults != nil {
		*out = *defaults
		// Copy slice so subsequent mutation does not poison defaults.
		if defaults.RetryOn != nil {
			out.RetryOn = append([]string(nil), defaults.RetryOn...)
		}
	}
	if endpoint != nil {
		if endpoint.MaxAttempts != 0 {
			out.MaxAttempts = endpoint.MaxAttempts
		}
		if endpoint.Backoff != "" {
			out.Backoff = endpoint.Backoff
		}
		if endpoint.InitialDelay != 0 {
			out.InitialDelay = endpoint.InitialDelay
		}
		if endpoint.MaxDelay != 0 {
			out.MaxDelay = endpoint.MaxDelay
		}
		if endpoint.RetryOn != nil {
			out.RetryOn = append([]string(nil), endpoint.RetryOn...)
		}
		if endpoint.RespectRetryAfter != nil {
			val := *endpoint.RespectRetryAfter
			out.RespectRetryAfter = &val
		}
	}
	return out
}

// policyFromConfig turns the YAML config into the runtime policy struct,
// applying framework defaults for fields the merged config left blank.
func policyFromConfig(cfg *RetryConfig) retryPolicy {
	p := retryPolicy{
		maxAttempts:       cfg.MaxAttempts,
		initialDelay:      cfg.InitialDelay.ToTime(),
		maxDelay:          cfg.MaxDelay.ToTime(),
		respectRetryAfter: frameworkRetryRespectAfter,
		retryOnStatus:     map[int]struct{}{},
	}
	if p.maxAttempts == 0 {
		// A retry: block was present but maxAttempts was omitted. Pick the
		// "enabled-default" so the operator's intent (declare retry) is
		// preserved.
		p.maxAttempts = frameworkRetryEnabledDefault
	}
	if p.initialDelay == 0 {
		p.initialDelay = frameworkRetryInitialDelay
	}
	if p.maxDelay == 0 {
		p.maxDelay = frameworkRetryMaxDelay
	}
	p.backoff = parseBackoffName(cfg.Backoff)
	if cfg.RespectRetryAfter != nil {
		p.respectRetryAfter = *cfg.RespectRetryAfter
	}
	retryOn := cfg.RetryOn
	if len(retryOn) == 0 {
		retryOn = frameworkRetryRetryOnDefault
	}
	for _, item := range retryOn {
		applyRetryOnEntry(&p, item)
	}
	return p
}

// parseBackoffName maps the YAML backoff label to the runtime enum. Unknown
// labels become backoffExponentialJitter so the runtime always has a strategy
// — Validate already rejects unknown labels at boot, so this fallback only
// runs in tests that synthesize the policy directly.
func parseBackoffName(name string) backoffStrategy {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "constant":
		return backoffConstant
	case "linear":
		return backoffLinear
	case "exponential":
		return backoffExponential
	case "exponential-jitter", "":
		return backoffExponentialJitter
	default:
		return backoffExponentialJitter
	}
}

// applyRetryOnEntry parses one retryOn token (numeric status or sentinel)
// onto the policy. Used by both policyFromConfig and Validate so the
// recognized vocabulary stays in lockstep.
func applyRetryOnEntry(p *retryPolicy, item string) {
	v := strings.ToLower(strings.TrimSpace(item))
	switch v {
	case "network":
		p.retryOnNetwork = true
	case "timeout":
		p.retryOnTimeout = true
	case "dns":
		p.retryOnDNS = true
	default:
		if code, err := strconv.Atoi(v); err == nil {
			p.retryOnStatus[code] = struct{}{}
		}
	}
}

// validateRetryConfig runs schema checks on a YAML retry block. Returns a
// joined error string (empty when valid) so the caller can collect issues
// into the global validation accumulator.
func validateRetryConfig(prefix string, cfg *RetryConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.MaxAttempts < 0 {
		errs = append(errs, fmt.Sprintf("%s.maxAttempts: must be non-negative", prefix))
	}
	if cfg.InitialDelay < 0 {
		errs = append(errs, fmt.Sprintf("%s.initialDelay: must be non-negative", prefix))
	}
	if cfg.MaxDelay < 0 {
		errs = append(errs, fmt.Sprintf("%s.maxDelay: must be non-negative", prefix))
	}
	if cfg.Backoff != "" {
		switch strings.ToLower(strings.TrimSpace(cfg.Backoff)) {
		case "constant", "linear", "exponential", "exponential-jitter":
		default:
			errs = append(errs, fmt.Sprintf("%s.backoff: %q is not one of constant|linear|exponential|exponential-jitter", prefix, cfg.Backoff))
		}
	}
	for _, item := range cfg.RetryOn {
		v := strings.ToLower(strings.TrimSpace(item))
		switch v {
		case "network", "timeout", "dns":
		default:
			code, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s.retryOn: %q is not a status code or sentinel (network|timeout|dns)", prefix, item))
				continue
			}
			if code < 100 || code > 599 {
				errs = append(errs, fmt.Sprintf("%s.retryOn: status %d is out of range 100..599", prefix, code))
			}
		}
	}
	return errs
}
