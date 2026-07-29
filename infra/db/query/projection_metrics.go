package query

import (
	"sync"
	"time"
)

// Projection-path signals, following the in-repo upstreamMetrics precedent: an
// in-process counter map a production sink polls on a ticker, not a dependency
// on any metrics library.
//
// The counters answer "how much is going wrong". The TIMESTAMPS answer the
// harder question, and they are the reason this type exists at all: a projection
// loop that has stopped does not emit errors. It emits nothing. Alarming on
// failure counts would therefore never fire for the worst outage — the silent
// one — so the loop's health must be judged by STALENESS: how long since it last
// processed anything, and how long since the parking ledger was last swept.
const (
	// MetricProjectionProcessed counts events driven to a successful outcome.
	MetricProjectionProcessed = "processed"
	// MetricProjectionRetried counts individual retry attempts (not events).
	MetricProjectionRetried = "retried"
	// MetricProjectionParked counts events written to the parking ledger after
	// exhausting their retry budget. A non-zero rate here is the signal that
	// convergence now depends on the replay driver or the reconciliation sweep.
	MetricProjectionParked = "parked"
	// MetricProjectionParkFailed counts events that could not even be parked.
	// This is the most severe counter on the path: the event is gone from the
	// stream and absent from the ledger, so only the sweep can still repair it.
	MetricProjectionParkFailed = "park_failed"
	// MetricProjectionReplayed counts parked events successfully replayed.
	MetricProjectionReplayed = "replayed"
	// MetricProjectionHandedBack counts events returned to the transport for
	// redelivery (shutdown, or a pointer cache that could not be refreshed).
	MetricProjectionHandedBack = "handed_back"
	// MetricProjectionSessionRestart counts consumer sessions the supervisor had
	// to reopen. Sustained growth means the loop is flapping rather than running.
	MetricProjectionSessionRestart = "session_restart"
	// MetricProjectionShadowWriteFailed counts exhausted dual-apply writes — the
	// leading indicator of a rebuild about to be abandoned.
	MetricProjectionShadowWriteFailed = "shadow_write_failed"
)

// projectionMetrics is the counter + liveness recorder for one SyncEngine.
type projectionMetrics struct {
	mu            sync.Mutex
	counters      map[string]uint64
	lastProcessed time.Time
	lastSweep     time.Time
	lastReconcile time.Time
}

func newProjectionMetrics() *projectionMetrics {
	return &projectionMetrics{counters: map[string]uint64{}}
}

// inc bumps one counter. Nil-safe so a SyncEngine built by a test without
// wiring costs nothing.
func (m *projectionMetrics) inc(name string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.counters[name]++
	m.mu.Unlock()
}

// processed records a successful outcome and stamps the liveness clock.
func (m *projectionMetrics) processed(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.counters[MetricProjectionProcessed]++
	m.lastProcessed = now
	m.mu.Unlock()
}

// swept stamps the parking-ledger sweep clock — the "who watches the watchman"
// half. The replay driver is what makes a parked event recoverable without the
// sweep; if it stops, parked events silently stop coming back.
func (m *projectionMetrics) swept(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.lastSweep = now
	m.mu.Unlock()
}

// reconciled stamps the reconciliation clock — the "who watches the watchman"
// signal for the backstop itself. The sweep is what both the parking path and
// the durability argument fall back on; if it dies quietly, both become unsound
// with no other signal. It must be alarmed on STALENESS, because a sweep that
// never runs never fails.
func (m *projectionMetrics) reconciled(now time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.lastReconcile = now
	m.mu.Unlock()
}

// ProjectionHealth is the snapshot a readiness endpoint, log line or metrics
// sink reads. Counters are cumulative since boot.
type ProjectionHealth struct {
	// Counters is keyed by the MetricProjection* names.
	Counters map[string]uint64
	// LastProcessed is when an event last reached a successful outcome. The zero
	// value means "nothing has been processed yet" — at boot that is normal, and
	// after a long uptime it is the outage.
	LastProcessed time.Time
	// LastLedgerSweep is when the parked-event replay driver last completed a
	// pass. The zero value means it has not run yet.
	LastLedgerSweep time.Time
	// LastReconcile is when a reconciliation sweep last completed. The zero value
	// means it has not run yet — which is the normal state until a consumer
	// schedules one.
	LastReconcile time.Time
}

func (m *projectionMetrics) snapshot() ProjectionHealth {
	if m == nil {
		return ProjectionHealth{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]uint64, len(m.counters))
	for k, v := range m.counters {
		out[k] = v
	}
	return ProjectionHealth{
		Counters:        out,
		LastProcessed:   m.lastProcessed,
		LastLedgerSweep: m.lastSweep,
		LastReconcile:   m.lastReconcile,
	}
}

// ProjectionHealth exposes the projection loop's counters and liveness clocks.
//
// It is deliberately NOT wired into readiness. A broker outage must not pull the
// pod out of the load balancer — the outbox decouples writes from the broker and
// reads keep serving from the last projected state. What this is for is
// ALARMING: a consumer that never runs never fails, so the operable signal is
// staleness of LastProcessed against the deployment's convergence budget, not an
// error count.
func (s *SyncEngine) ProjectionHealth() ProjectionHealth {
	if s == nil {
		return ProjectionHealth{}
	}
	return s.metrics.snapshot()
}
