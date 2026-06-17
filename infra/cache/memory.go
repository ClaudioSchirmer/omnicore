package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// frameworkMemoryMaxEntries is the fallback cap when neither YAML nor
// caller specifies a maxEntries value at construction. Picked so the
// default Memory instance does not grow unbounded under a forgetful
// operator but stays large enough to be useful — 10k entries covers
// most read paths.
const frameworkMemoryMaxEntries = 10000

// NewMemory constructs an in-process Cache backed by an LRU+TTL store.
// maxEntries == 0 falls back to the framework default; negative values
// are clamped to 0 so the call cannot panic. The store is safe for
// concurrent use; consumers receive it as the Cache interface and
// don't depend on the concrete type.
func NewMemory(maxEntries int) Cache {
	if maxEntries == 0 {
		maxEntries = frameworkMemoryMaxEntries
	}
	if maxEntries < 0 {
		maxEntries = 0
	}
	return &memoryCache{
		maxLen:  maxEntries,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

type memoryCache struct {
	mu      sync.Mutex
	maxLen  int
	order   *list.List
	entries map[string]*list.Element
}

type memoryItem struct {
	key       string
	value     []byte
	expiresAt time.Time // zero == no expiration
}

func (c *memoryCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	item := el.Value.(*memoryItem)
	if !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		c.order.Remove(el)
		delete(c.entries, key)
		return nil, false, nil
	}
	c.order.MoveToFront(el)
	// Return a fresh slice so callers cannot mutate the cached bytes.
	out := make([]byte, len(item.value))
	copy(out, item.value)
	return out, true, nil
}

func (c *memoryCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return ErrInvalidTTL
	}
	expires := time.Time{}
	if ttl > 0 {
		expires = time.Now().Add(ttl)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Defensive copy on write — the caller may reuse the slice after Set.
	stored := make([]byte, len(value))
	copy(stored, value)
	if el, ok := c.entries[key]; ok {
		item := el.Value.(*memoryItem)
		item.value = stored
		item.expiresAt = expires
		c.order.MoveToFront(el)
		return nil
	}
	if c.maxLen > 0 && c.order.Len() >= c.maxLen {
		back := c.order.Back()
		if back != nil {
			delete(c.entries, back.Value.(*memoryItem).key)
			c.order.Remove(back)
		}
	}
	el := c.order.PushFront(&memoryItem{key: key, value: stored, expiresAt: expires})
	c.entries[key] = el
	return nil
}

func (c *memoryCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.Remove(el)
		delete(c.entries, key)
	}
	return nil
}

// len reports the current entry count (test-only helper, not part of
// the Cache interface).
func (c *memoryCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
