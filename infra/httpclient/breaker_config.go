package httpclient

import (
	"fmt"
	"time"
)

// Framework defaults applied when the circuitBreaker: block is present but
// a field is omitted. Picked for production sanity: 5 consecutive failures
// trip; 2 successes in half-open close; 30s recovery window.
const (
	frameworkBreakerFailureThreshold = 5
	frameworkBreakerSuccessThreshold = 2
	frameworkBreakerOpenFor          = 30 * time.Second
)

// CircuitBreakerConfig is the defaults-level circuitBreaker: block. The
// design does not document per-endpoint or per-service overrides today;
// when the block is present, every (service, endpoint) breaker uses the
// same policy. The runtime state is still tracked per pair.
type CircuitBreakerConfig struct {
	Enabled          *bool    `yaml:"enabled"`
	FailureThreshold int      `yaml:"failureThreshold"`
	SuccessThreshold int      `yaml:"successThreshold"`
	OpenFor          Duration `yaml:"openFor"`
}

// resolveBreakerConfig turns the YAML config into the runtime policy
// struct, applying framework defaults for fields the YAML left blank. A
// nil config or Enabled=false yields a disabled policy (no monitoring).
func resolveBreakerConfig(cfg *CircuitBreakerConfig) breakerPolicy {
	if cfg == nil {
		return breakerPolicy{}
	}
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	if !enabled {
		return breakerPolicy{}
	}
	policy := breakerPolicy{
		enabled:          true,
		failureThreshold: cfg.FailureThreshold,
		successThreshold: cfg.SuccessThreshold,
		openFor:          cfg.OpenFor.ToTime(),
	}
	if policy.failureThreshold == 0 {
		policy.failureThreshold = frameworkBreakerFailureThreshold
	}
	if policy.successThreshold == 0 {
		policy.successThreshold = frameworkBreakerSuccessThreshold
	}
	if policy.openFor == 0 {
		policy.openFor = frameworkBreakerOpenFor
	}
	return policy
}

// validateBreakerConfig runs schema checks on the defaults-level block.
// Returns a list of error strings the global validator joins together.
func validateBreakerConfig(prefix string, cfg *CircuitBreakerConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.FailureThreshold < 0 {
		errs = append(errs, fmt.Sprintf("%s.failureThreshold: must be non-negative", prefix))
	}
	if cfg.SuccessThreshold < 0 {
		errs = append(errs, fmt.Sprintf("%s.successThreshold: must be non-negative", prefix))
	}
	if cfg.OpenFor < 0 {
		errs = append(errs, fmt.Sprintf("%s.openFor: must be non-negative", prefix))
	}
	return errs
}
