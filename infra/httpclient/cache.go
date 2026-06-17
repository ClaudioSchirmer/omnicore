package httpclient

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"time"
)

// Cache is the storage port for the GET cache middleware. The framework
// ships two canonical implementations selected via YAML
// (defaults.cache.store: memory | redis); consumers wanting a different
// backend (Memcached, Valkey, Hazelcast, etc.) implement this interface
// and inject the store at New time via WithCacheStore — `cache.store:
// custom` is required in YAML so the configuration declares the intent.
// Implementations MUST be safe for concurrent use.
//
// The (context, error) signatures support backends with network I/O —
// timeouts propagate through Get/Set, and transport failures surface
// without forcing the implementation to swallow them silently. Network
// backends typically resolve `failMode` (open / closed) internally:
// fail-open swallows transport errors + slog.Warn + returns (nil, false,
// nil) so the call proceeds without caching; fail-closed propagates the
// error so the middleware aborts.
type Cache interface {
	Get(ctx context.Context, key string) (*CacheEntry, bool, error)
	Set(ctx context.Context, key string, entry *CacheEntry) error
}

// CacheEntry is the materialized response stored per key. Body is
// buffered in memory because subsequent reads need to be replayable;
// Headers and Status reconstruct the original wire shape; ExpiresAt
// drives TTL eviction.
//
// The type is exported because consumer-side Cache implementations need
// to construct entries on Get (deserializing from their backend) and to
// inspect entries on Set (serializing to their backend). All fields are
// primitives or stdlib types, so a JSON / gob / protobuf round-trip
// preserves the contract verbatim.
type CacheEntry struct {
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
	entry *CacheEntry
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
// entries are removed eagerly so the LRU bound reflects live size. The
// in-memory backend never returns an error — the signature matches the
// Cache interface so a Redis adapter / consumer adapter can plug in
// without changes at the middleware.
func (c *memoryCache) Get(_ context.Context, key string) (*CacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	item := el.Value.(*memoryItem)
	if !item.entry.ExpiresAt.IsZero() && time.Now().After(item.entry.ExpiresAt) {
		c.order.Remove(el)
		delete(c.entries, key)
		return nil, false, nil
	}
	c.order.MoveToFront(el)
	return item.entry, true, nil
}

// Set stores entry under key. When the cache is at capacity, the least
// recently used entry is evicted before insertion. The in-memory backend
// never returns an error.
func (c *memoryCache) Set(_ context.Context, key string, entry *CacheEntry) error {
	if entry == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		el.Value.(*memoryItem).entry = entry
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
	el := c.order.PushFront(&memoryItem{key: key, entry: entry})
	c.entries[key] = el
	return nil
}

// len reports the current entry count (test-only helper, not part of the
// public Cache interface).
func (c *memoryCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}
