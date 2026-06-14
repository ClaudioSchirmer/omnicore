package httpclient

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// Cache is the storage port for the GET cache middleware. The interface is
// package-private — adding a Redis or distributed adapter is a framework
// change, not a consumer extension surface. Implementations must be safe
// for concurrent use.
type Cache interface {
	Get(key string) (*cacheEntry, bool)
	Set(key string, entry *cacheEntry)
}

// cacheEntry is the materialized response stored per key. Body is buffered
// in memory because subsequent reads need to be replayable; headers and
// status reconstruct the original wire shape; ExpiresAt drives TTL eviction.
type cacheEntry struct {
	Body          []byte
	Headers       http.Header
	Status        int
	ContentType   string
	ContentLength int64
	ExpiresAt     time.Time
}

// memoryCache is the default Cache implementation: an LRU eviction list
// (newest at front) with per-entry TTL. Capacity is fixed at construction;
// 0 disables the cap (debug only — production must bound the size).
type memoryCache struct {
	mu      sync.Mutex
	maxLen  int
	order   *list.List
	entries map[string]*list.Element
}

// memoryItem is the value stored on the LRU list. Holds the key (so Set
// can evict the oldest by walking back) and the entry.
type memoryItem struct {
	key   string
	entry *cacheEntry
}

// newMemoryCache constructs an in-memory LRU+TTL store with the given
// capacity. 0 means unbounded; callers should pick a finite bound for
// production unless growth is otherwise constrained.
func newMemoryCache(maxLen int) *memoryCache {
	if maxLen < 0 {
		maxLen = 0
	}
	return &memoryCache{
		maxLen:  maxLen,
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

// Get returns the entry for key when present and not expired. Expired
// entries are removed eagerly so the LRU bound reflects live size.
func (c *memoryCache) Get(key string) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	item := el.Value.(*memoryItem)
	if !item.entry.ExpiresAt.IsZero() && time.Now().After(item.entry.ExpiresAt) {
		c.order.Remove(el)
		delete(c.entries, key)
		return nil, false
	}
	c.order.MoveToFront(el)
	return item.entry, true
}

// Set stores entry under key. When the cache is at capacity, the least
// recently used entry is evicted before insertion.
func (c *memoryCache) Set(key string, entry *cacheEntry) {
	if entry == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*memoryItem).entry = entry
		c.order.MoveToFront(el)
		return
	}
	if c.maxLen > 0 && c.order.Len() >= c.maxLen {
		back := c.order.Back()
		if back != nil {
			delete(c.entries, back.Value.(*memoryItem).key)
			c.order.Remove(back)
		}
	}
	el := c.order.PushFront(&memoryItem{key: key, entry: entry})
	c.entries[key] = el
}

// len reports the current entry count (test-only helper, not part of the
// public Cache interface).
func (c *memoryCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
