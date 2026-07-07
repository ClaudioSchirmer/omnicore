package resilience

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
)

// BackoffStrategy enumerates the retry wait curves. Values match the
// httpclient's historical encoding (iota + 1) so a resolved policy converts
// by plain cast.
type BackoffStrategy int

const (
	BackoffConstant BackoffStrategy = iota + 1
	BackoffLinear
	BackoffExponential
	BackoffExponentialJitter
)

// BackoffPolicy is the neutral slice of a resolved retry policy — only the
// curve inputs. Everything else about retrying (which outcomes trigger it,
// attempt caps, Retry-After honoring) belongs to the transport client.
type BackoffPolicy struct {
	Strategy     BackoffStrategy
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

// backoffRand is the shared random source for jitter. Seeded once with the
// process start time and protected by a mutex because math/rand.Source is
// not safe for concurrent use.
var (
	backoffRandOnce sync.Once
	backoffRandMu   sync.Mutex
	backoffRand     *rand.Rand
)

func initBackoffRand() {
	backoffRandOnce.Do(func() {
		backoffRand = rand.New(rand.NewSource(time.Now().UnixNano()))
	})
}

// Jitter returns a uniformly random duration in [0, maxNS) nanoseconds;
// 0 for non-positive inputs.
func Jitter(maxNS int64) int64 {
	initBackoffRand()
	if maxNS <= 0 {
		return 0
	}
	backoffRandMu.Lock()
	defer backoffRandMu.Unlock()
	return backoffRand.Int63n(maxNS)
}

// Backoff applies the policy's curve for the given attempt (1-indexed; the
// sleep happens AFTER the attempt that just failed). All curves are capped
// at MaxDelay; overflow of the exponential shift falls back to MaxDelay.
func Backoff(policy BackoffPolicy, attempt int) time.Duration {
	if policy.InitialDelay <= 0 {
		return 0
	}
	if attempt < 1 {
		attempt = 1
	}
	var d time.Duration
	switch policy.Strategy {
	case BackoffConstant:
		d = policy.InitialDelay
	case BackoffLinear:
		d = policy.InitialDelay * time.Duration(attempt)
	case BackoffExponential:
		d = policy.InitialDelay << uint(attempt-1)
	case BackoffExponentialJitter:
		ceiling := policy.InitialDelay << uint(attempt-1)
		if ceiling > policy.MaxDelay {
			ceiling = policy.MaxDelay
		}
		d = time.Duration(Jitter(int64(ceiling)))
	default:
		d = policy.InitialDelay
	}
	if d > policy.MaxDelay {
		d = policy.MaxDelay
	}
	if d < 0 {
		d = policy.MaxDelay
	}
	return d
}

// SleepCtx waits for d unless ctx is canceled. Returns true when the sleep
// completed, false when the context fired (the caller should abort).
func SleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// NewIdempotencyKey returns a UUIDv7 string suitable as an idempotency key
// — sortable by timestamp, which keeps upstream dedup logs readable.
func NewIdempotencyKey() (string, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
