// Package grpcclient is the outbound gRPC/Connect toolbox — the sibling of
// infra/httpclient for the gRPC plane. One Client is built at boot from the
// yaml `grpcClient:` block; per service it carries a pooled *http.Client
// and a connect interceptor chain with the same resilience semantics as the
// HTTP chain — correlation, idempotency, retry, circuit breaker (the state
// machines live in infra/resilience, shared with httpclient so the two
// planes can never drift), tracing and auth. Consumers construct generated
// Connect clients through For / the accessors; application handlers never
// import this package directly (adapter in the service's infra/, exactly
// like httpclient).
//
// Deliberate gaps vs the httpclient chain (documented in the manual):
// cache (no GET/no-store vocabulary in gRPC) and HMAC signing (mTLS is the
// authenticity mechanism on this plane). Auth providers: forward-bearer and
// static-token ship now; the oauth2 family follows once its acquisition
// core is extracted transport-neutrally.
package grpcclient

import (
	"fmt"
	"time"

	"connectrpc.com/connect"

	"github.com/ClaudioSchirmer/omnicore/infra/resilience"
)

// Config is the yaml `grpcClient:` block.
type Config struct {
	Defaults Defaults                 `yaml:"defaults"`
	Services map[string]ServiceConfig `yaml:"services"`
}

// Defaults are the cross-service knobs applied when a ServiceConfig omits a
// value. Resolution happens once at New, never per request.
type Defaults struct {
	// TimeoutSeconds bounds each RPC when the caller's ctx carries no
	// earlier deadline. Falls back to 30s when neither defaults nor the
	// service set it.
	TimeoutSeconds int `yaml:"timeoutSeconds"`

	Retry          *RetryConfig   `yaml:"retry,omitempty"`
	CircuitBreaker *BreakerConfig `yaml:"circuitBreaker,omitempty"`
}

// ServiceConfig declares one upstream gRPC service.
type ServiceConfig struct {
	// BaseURL is the scheme+host(+port) of the upstream's gRPC listener,
	// e.g. "http://users:9090" (h2c) or "https://users:9443".
	BaseURL string `yaml:"baseURL"`

	// TimeoutSeconds overrides Defaults.TimeoutSeconds for this service.
	TimeoutSeconds int `yaml:"timeoutSeconds"`

	Retry          *RetryConfig       `yaml:"retry,omitempty"`
	CircuitBreaker *BreakerConfig     `yaml:"circuitBreaker,omitempty"`
	Auth           *AuthConfig        `yaml:"auth,omitempty"`
	Idempotency    *IdempotencyConfig `yaml:"idempotency,omitempty"`
	Pool           *PoolConfig        `yaml:"pool,omitempty"`
}

// PoolConfig is OPTIONAL connection-pool fine-tuning for high-concurrency
// callers (precedent: relational.pool). The producer's idleTimeoutSeconds
// already keeps the default case healthy; this block lets a heavy caller
// spread and recycle its own pipes without touching another team's
// producer deploy.
type PoolConfig struct {
	// MaxIdleConnsPerHost widens the kept-warm pool (Go default: 2) so
	// concurrent traffic spreads over more upstream pods.
	MaxIdleConnsPerHost int `yaml:"maxIdleConnsPerHost"`

	// ConnMaxLifetimeSeconds recycles pooled idle connections on this
	// cadence (a background sweep closes idle pipes), so the caller
	// re-dials periodically and rebalances even against producers without
	// an idle timeout. 0 disables the sweep.
	ConnMaxLifetimeSeconds int `yaml:"connMaxLifetimeSeconds"`
}

// RetryConfig mirrors the httpclient retry block with Connect-code
// triggers instead of HTTP statuses.
type RetryConfig struct {
	// MaxAttempts caps the total tries (1 = no retry).
	MaxAttempts int `yaml:"maxAttempts"`

	// Backoff names the wait curve: constant | linear | exponential |
	// exponential-jitter (default).
	Backoff string `yaml:"backoff"`

	InitialDelayMS int `yaml:"initialDelayMS"`
	MaxDelayMS     int `yaml:"maxDelayMS"`

	// RetryOn lists the connect codes that trigger a retry, by canonical
	// name (e.g. "unavailable", "deadline_exceeded", "resource_exhausted").
	// Empty defaults to ["unavailable"]. Transport-level dial errors always
	// retry (the sibling of the HTTP chain's network trigger).
	RetryOn []string `yaml:"retryOn"`
}

// BreakerConfig mirrors the httpclient circuitBreaker block; runtime state
// is tracked per (service, procedure).
type BreakerConfig struct {
	Enabled          bool `yaml:"enabled"`
	FailureThreshold int  `yaml:"failureThreshold"`
	SuccessThreshold int  `yaml:"successThreshold"`
	OpenForMS        int  `yaml:"openForMS"`
}

// AuthConfig selects the credential attached to every call, on the
// `authorization` header (Connect metadata is HTTP headers).
type AuthConfig struct {
	// Mode: "forward" re-sends the inbound caller's bearer
	// (AppContext.BearerToken — fails the call when absent); "static"
	// attaches Token verbatim.
	Mode  string `yaml:"mode"`
	Token string `yaml:"token,omitempty"`
}

// IdempotencyConfig enables the per-call idempotency key — same semantics
// as the httpclient middleware: generated once per logical call (UUIDv7),
// stable across retry attempts, so a deduping upstream makes retried
// writes safe.
type IdempotencyConfig struct {
	Enabled bool `yaml:"enabled"`

	// Header carrying the key. Default "X-Idempotency-Key".
	Header string `yaml:"header"`
}

const (
	defaultTimeout           = 30 * time.Second
	defaultIdempotencyHeader = "X-Idempotency-Key"
)

// resolvedService is the per-service runtime shape: everything parsed and
// defaulted once at New.
type resolvedService struct {
	name        string
	baseURL     string
	timeout     time.Duration
	retry       retryPolicy
	breakerCfg  resilience.BreakerPolicy
	auth        *AuthConfig
	idempotency *IdempotencyConfig
	pool        *PoolConfig
}

type retryPolicy struct {
	maxAttempts int
	backoff     resilience.BackoffPolicy
	retryOn     map[connect.Code]struct{}
}

func (p retryPolicy) disabled() bool { return p.maxAttempts <= 1 }

// codeByName maps the yaml trigger names to connect codes.
var codeByName = map[string]connect.Code{
	"canceled":            connect.CodeCanceled,
	"unknown":             connect.CodeUnknown,
	"invalid_argument":    connect.CodeInvalidArgument,
	"deadline_exceeded":   connect.CodeDeadlineExceeded,
	"not_found":           connect.CodeNotFound,
	"already_exists":      connect.CodeAlreadyExists,
	"permission_denied":   connect.CodePermissionDenied,
	"resource_exhausted":  connect.CodeResourceExhausted,
	"failed_precondition": connect.CodeFailedPrecondition,
	"aborted":             connect.CodeAborted,
	"out_of_range":        connect.CodeOutOfRange,
	"unimplemented":       connect.CodeUnimplemented,
	"internal":            connect.CodeInternal,
	"unavailable":         connect.CodeUnavailable,
	"data_loss":           connect.CodeDataLoss,
	"unauthenticated":     connect.CodeUnauthenticated,
}

var backoffByName = map[string]resilience.BackoffStrategy{
	"constant":           resilience.BackoffConstant,
	"linear":             resilience.BackoffLinear,
	"exponential":        resilience.BackoffExponential,
	"exponential-jitter": resilience.BackoffExponentialJitter,
}

// resolve validates and freezes one service's runtime shape.
func resolve(name string, svc ServiceConfig, defaults Defaults) (*resolvedService, error) {
	if svc.BaseURL == "" {
		return nil, fmt.Errorf("grpcclient: service %q: baseURL is required", name)
	}
	timeoutSeconds := svc.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = defaults.TimeoutSeconds
	}
	timeout := defaultTimeout
	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
	}

	retryCfg := svc.Retry
	if retryCfg == nil {
		retryCfg = defaults.Retry
	}
	retry, err := resolveRetry(name, retryCfg)
	if err != nil {
		return nil, err
	}

	breakerCfg := svc.CircuitBreaker
	if breakerCfg == nil {
		breakerCfg = defaults.CircuitBreaker
	}
	breaker := resilience.BreakerPolicy{}
	if breakerCfg != nil && breakerCfg.Enabled {
		breaker = resilience.BreakerPolicy{
			Enabled:          true,
			FailureThreshold: max(1, breakerCfg.FailureThreshold),
			SuccessThreshold: max(1, breakerCfg.SuccessThreshold),
			OpenFor:          time.Duration(max(1, breakerCfg.OpenForMS)) * time.Millisecond,
		}
	}

	if svc.Auth != nil {
		switch svc.Auth.Mode {
		case "forward":
			if svc.Auth.Token != "" {
				return nil, fmt.Errorf("grpcclient: service %q: auth.token is not accepted with mode=forward", name)
			}
		case "static":
			if svc.Auth.Token == "" {
				return nil, fmt.Errorf("grpcclient: service %q: auth.token is required with mode=static", name)
			}
		default:
			return nil, fmt.Errorf("grpcclient: service %q: auth.mode %q (want forward|static)", name, svc.Auth.Mode)
		}
	}

	idem := svc.Idempotency
	if idem != nil && idem.Enabled && idem.Header == "" {
		idem = &IdempotencyConfig{Enabled: true, Header: defaultIdempotencyHeader}
	}

	if svc.Pool != nil && (svc.Pool.MaxIdleConnsPerHost < 0 || svc.Pool.ConnMaxLifetimeSeconds < 0) {
		return nil, fmt.Errorf("grpcclient: service %q: pool values must be >= 0", name)
	}

	return &resolvedService{
		name:        name,
		baseURL:     svc.BaseURL,
		timeout:     timeout,
		retry:       retry,
		breakerCfg:  breaker,
		auth:        svc.Auth,
		idempotency: idem,
		pool:        svc.Pool,
	}, nil
}

func resolveRetry(name string, cfg *RetryConfig) (retryPolicy, error) {
	if cfg == nil || cfg.MaxAttempts <= 1 {
		return retryPolicy{maxAttempts: 1}, nil
	}
	strategy := resilience.BackoffExponentialJitter
	if cfg.Backoff != "" {
		s, ok := backoffByName[cfg.Backoff]
		if !ok {
			return retryPolicy{}, fmt.Errorf("grpcclient: service %q: retry.backoff %q (want constant|linear|exponential|exponential-jitter)", name, cfg.Backoff)
		}
		strategy = s
	}
	initial := 100 * time.Millisecond
	if cfg.InitialDelayMS > 0 {
		initial = time.Duration(cfg.InitialDelayMS) * time.Millisecond
	}
	maxDelay := 2 * time.Second
	if cfg.MaxDelayMS > 0 {
		maxDelay = time.Duration(cfg.MaxDelayMS) * time.Millisecond
	}
	retryOn := map[connect.Code]struct{}{}
	if len(cfg.RetryOn) == 0 {
		retryOn[connect.CodeUnavailable] = struct{}{}
	}
	for _, nameStr := range cfg.RetryOn {
		code, ok := codeByName[nameStr]
		if !ok {
			return retryPolicy{}, fmt.Errorf("grpcclient: service %q: retry.retryOn %q is not a connect code", name, nameStr)
		}
		retryOn[code] = struct{}{}
	}
	return retryPolicy{
		maxAttempts: cfg.MaxAttempts,
		backoff: resilience.BackoffPolicy{
			Strategy:     strategy,
			InitialDelay: initial,
			MaxDelay:     maxDelay,
		},
		retryOn: retryOn,
	}, nil
}
