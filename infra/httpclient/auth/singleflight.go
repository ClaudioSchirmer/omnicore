package auth

import "sync"

// singleFlight collapses concurrent invocations of fn into a single call.
// The first caller runs fn; concurrent callers wait on the condition
// variable and share the result. Designed for the token endpoint refresh
// path — a stampede of expired-cache misses should generate one upstream
// request, not N.
//
// A fresh singleFlight is reusable: each successful or failed run resets
// for the next call, with no allocations beyond the initial sync primitives.
type singleFlight struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inFlight bool
	result   string
	err      error
}

// Do invokes fn when no call is in flight; concurrent callers wait and
// receive the same (result, err) as the one that actually ran.
func (s *singleFlight) Do(fn func() (string, error)) (string, error) {
	s.mu.Lock()
	if s.cond == nil {
		s.cond = sync.NewCond(&s.mu)
	}
	if s.inFlight {
		for s.inFlight {
			s.cond.Wait()
		}
		res, err := s.result, s.err
		s.mu.Unlock()
		return res, err
	}
	s.inFlight = true
	s.mu.Unlock()

	res, err := fn()

	s.mu.Lock()
	s.result = res
	s.err = err
	s.inFlight = false
	s.cond.Broadcast()
	s.mu.Unlock()
	return res, err
}
