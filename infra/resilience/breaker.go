// Package resilience holds the transport-neutral resilience cores shared by
// the framework's outbound clients: the circuit-breaker state machine and
// the retry backoff curves consumed by infra/httpclient (HTTP) and
// infra/grpcclient (Connect/gRPC), plus the idempotency-key generator. The
// packages that consume it own everything transport-specific — what counts
// as a failure, which signals trigger a retry, where the key is attached;
// this package owns only the state machines and the math, so the two
// clients can never drift semantically.
package resilience

import (
	"sync"
	"time"
)

// BreakerState enumerates the three states of the circuit-breaker state
// machine. Transitions are driven by observed outcomes.
type BreakerState int

const (
	BreakerClosed BreakerState = iota
	BreakerOpen
	BreakerHalfOpen
)

// String renders the state for slog observation; matches the labels used
// in the clients' observation fields and YAML defaults.
func (k BreakerState) String() string {
	switch k {
	case BreakerOpen:
		return "open"
	case BreakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// BreakerPolicy is the resolved runtime shape consumed by Breaker. Built
// once from the client's YAML config at boot.
type BreakerPolicy struct {
	Enabled          bool
	FailureThreshold int
	SuccessThreshold int
	OpenFor          time.Duration
}

// Breaker is the per-(service, endpoint/method) circuit-breaker state
// machine. Concurrent callers are serialized through mu; half-open admits
// one probe at a time by claiming the probeInFlight flag — subsequent
// half-open requests are rejected as if the breaker were open, so a
// thundering herd cannot flood a recovering upstream.
type Breaker struct {
	mu            sync.Mutex
	policy        BreakerPolicy
	state         BreakerState
	failureCount  int
	successCount  int
	openedAt      time.Time
	probeInFlight bool
}

// NewBreaker constructs a breaker in the closed state.
func NewBreaker(policy BreakerPolicy) *Breaker {
	return &Breaker{policy: policy, state: BreakerClosed}
}

// Allow consults the current state and decides whether the next call may
// proceed. Returns (true, state-label) when the call is admitted and
// (false, "open") otherwise. The state-label is the observation value the
// caller records on its slog line.
//
// State transitions inside Allow:
//
//   - closed → admit
//   - open → check OpenFor: if elapsed, transition to half-open and admit
//   - open → if not elapsed, reject
//   - half-open → admit only when no probe is in flight
func (b *Breaker) Allow() (bool, string) {
	if b == nil || !b.policy.Enabled {
		return true, "closed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		return true, "closed"
	case BreakerOpen:
		if time.Since(b.openedAt) >= b.policy.OpenFor {
			b.state = BreakerHalfOpen
			b.successCount = 0
			b.failureCount = 0
			b.probeInFlight = true
			return true, "half-open"
		}
		return false, "open"
	case BreakerHalfOpen:
		if b.probeInFlight {
			return false, "open"
		}
		b.probeInFlight = true
		return true, "half-open"
	}
	return true, "closed"
}

// RecordSuccess records a successful upstream outcome. In half-open it
// increments successCount and closes the circuit once SuccessThreshold is
// reached; in closed it resets failureCount.
func (b *Breaker) RecordSuccess() {
	if b == nil || !b.policy.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerHalfOpen:
		b.probeInFlight = false
		b.successCount++
		if b.successCount >= b.policy.SuccessThreshold {
			b.state = BreakerClosed
			b.successCount = 0
			b.failureCount = 0
		}
	case BreakerClosed:
		b.failureCount = 0
	}
}

// RecordFailure records a failing upstream outcome. Closed → may transition
// to open; half-open transitions straight back to open (a single failure
// resets the probe window).
func (b *Breaker) RecordFailure() {
	if b == nil || !b.policy.Enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case BreakerClosed:
		b.failureCount++
		if b.failureCount >= b.policy.FailureThreshold {
			b.state = BreakerOpen
			b.openedAt = time.Now()
			b.failureCount = 0
			b.successCount = 0
		}
	case BreakerHalfOpen:
		b.probeInFlight = false
		b.state = BreakerOpen
		b.openedAt = time.Now()
		b.failureCount = 0
		b.successCount = 0
	}
}

// SnapshotState returns the current state label for observability without
// mutating any counters.
func (b *Breaker) SnapshotState() string {
	if b == nil || !b.policy.Enabled {
		return "closed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}
