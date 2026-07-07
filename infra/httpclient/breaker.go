package httpclient

import (
	"time"

	"github.com/ClaudioSchirmer/omnicore/infra/resilience"
)

// The circuit-breaker state machine moved to infra/resilience — the
// transport-neutral core shared with infra/grpcclient. This file keeps the
// package's historical seams (breakerPolicy, breakerState and its method
// set) as thin delegations so every consumer and test in this package
// stays untouched; what counts as a failure remains decided HERE, by the
// breaker middleware.

// breakerPolicy is the resolved runtime shape consumed by breakerState.
// Built once from CircuitBreakerConfig + framework defaults at New.
type breakerPolicy struct {
	enabled          bool
	failureThreshold int
	successThreshold int
	openFor          time.Duration
}

// breakerState is the per-(service, endpoint) state machine — a thin shell
// over resilience.Breaker.
type breakerState struct {
	core   *resilience.Breaker
	policy breakerPolicy
}

// newBreakerState constructs a breaker in the closed state.
func newBreakerState(policy breakerPolicy) *breakerState {
	return &breakerState{policy: policy, core: resilience.NewBreaker(resilience.BreakerPolicy{
		Enabled:          policy.enabled,
		FailureThreshold: policy.failureThreshold,
		SuccessThreshold: policy.successThreshold,
		OpenFor:          policy.openFor,
	})}
}

func (b *breakerState) allow() (bool, string) {
	if b == nil {
		return true, "closed"
	}
	return b.core.Allow()
}

func (b *breakerState) recordSuccess() {
	if b != nil {
		b.core.RecordSuccess()
	}
}

func (b *breakerState) recordFailure() {
	if b != nil {
		b.core.RecordFailure()
	}
}

func (b *breakerState) snapshotState() string {
	if b == nil {
		return "closed"
	}
	return b.core.SnapshotState()
}
