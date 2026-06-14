package auth

import (
	"sync"
	"time"
)

// tokenCache is the per-provider single-entry cache used by OAuth2
// providers. Get respects the configured skew: a token is reported as
// hit only when now+skew < expiresAt. Invalidate forcibly clears the
// entry — used by revocationOnUnauthorized.
type tokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// Get returns the cached token when present and not within the skew
// window of expiry. Returns (_, false) on miss.
func (c *tokenCache) Get(skew time.Duration) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" {
		return "", false
	}
	if !c.expiresAt.IsZero() && time.Now().Add(skew).After(c.expiresAt) {
		return "", false
	}
	return c.token, true
}

// Set stores the new token and its expiry. Empty expiresAt is allowed and
// means "never expire by cache TTL" (typically not used in OAuth2 — every
// provider variant computes a real expiry).
func (c *tokenCache) Set(token string, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	c.expiresAt = expiresAt
}

// Invalidate clears the cached token so the next Get returns a miss.
func (c *tokenCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
	c.expiresAt = time.Time{}
}
