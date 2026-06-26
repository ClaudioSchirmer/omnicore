package httpclient

import (
	"sync"
	"time"
)

// breakerStateKind enumerates the three states of the circuit breaker
// state machine. Transitions are driven by observed outcomes.
type breakerStateKind int

const (
	breakerClosed breakerStateKind = iota
	breakerOpen
	breakerHalfOpen
)

// String renders the state for slog observation; matches the labels used
// in the observation field and in YAML defaults.
func (k breakerStateKind) String() string {
	switch k {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

// breakerPolicy is the resolved runtime shape consumed by breakerState.
// Built once from CircuitBreakerConfig + framework defaults at New.
type breakerPolicy struct {
	enabled          bool
	failureThreshold int
	successThreshold int
	openFor          time.Duration
}

// breakerState is the per-(service, endpoint) state machine. Concurrent
// callers are serialized through mu; half-open admits one probe at a time
// by claiming the probeInFlight flag — subsequent half-open requests are
// rejected as if the breaker were open, so a thundering herd cannot flood
// a recovering upstream.
type breakerState struct {
	mu            sync.Mutex
	policy        breakerPolicy
	state         breakerStateKind
	failureCount  int
	successCount  int
	openedAt      time.Time
	probeInFlight bool
}

// newBreakerState constructs a breaker in the closed state.
func newBreakerState(policy breakerPolicy) *breakerState {
	return &breakerState{policy: policy, state: breakerClosed}
}

// allow consults the current state and decides whether the next call may
// proceed. Returns (true, state-label) when the call is admitted and a
// (false, "open") otherwise. The state-label is the observation value the
// middleware records on the slog line.
//
// State transitions inside allow:
//
//   - closed → admit
//   - open → check openFor: if elapsed, transition to half-open and admit
//   - open → if not elapsed, reject
//   - half-open → admit only when no probe is in flight
func (b *breakerState) allow() (bool, string) {
	if b == nil || !b.policy.enabled {
		return true, "closed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true, "closed"
	case breakerOpen:
		if time.Since(b.openedAt) >= b.policy.openFor {
			b.state = breakerHalfOpen
			b.successCount = 0
			b.failureCount = 0
			b.probeInFlight = true
			return true, "half-open"
		}
		return false, "open"
	case breakerHalfOpen:
		if b.probeInFlight {
			return false, "open"
		}
		b.probeInFlight = true
		return true, "half-open"
	}
	return true, "closed"
}

// recordSuccess records a successful upstream outcome. In half-open it
// increments successCount and closes the circuit once successThreshold is
// reached; in closed it resets failureCount.
func (b *breakerState) recordSuccess() {
	if b == nil || !b.policy.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerHalfOpen:
		b.probeInFlight = false
		b.successCount++
		if b.successCount >= b.policy.successThreshold {
			b.state = breakerClosed
			b.successCount = 0
			b.failureCount = 0
		}
	case breakerClosed:
		b.failureCount = 0
	}
}

// recordFailure records a failing upstream outcome (transport error or
// non-2xx status). Closed → may transition to open; half-open transitions
// straight back to open (single failure resets the probe window).
func (b *breakerState) recordFailure() {
	if b == nil || !b.policy.enabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		b.failureCount++
		if b.failureCount >= b.policy.failureThreshold {
			b.state = breakerOpen
			b.openedAt = time.Now()
			b.failureCount = 0
			b.successCount = 0
		}
	case breakerHalfOpen:
		b.probeInFlight = false
		b.state = breakerOpen
		b.openedAt = time.Now()
		b.failureCount = 0
		b.successCount = 0
	}
}

// snapshotState returns the current state for observability without
// mutating any counters.
func (b *breakerState) snapshotState() string {
	if b == nil || !b.policy.enabled {
		return "closed"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}
