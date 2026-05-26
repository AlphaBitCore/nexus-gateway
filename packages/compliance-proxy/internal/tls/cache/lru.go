package cache

import (
	"container/list"
	"crypto/tls"
	"sync"
	"time"
)

// lruEntry holds a cached certificate along with its expiry time.
type lruEntry struct {
	hostname string
	cert     *tls.Certificate
	expiry   time.Time
}

// LRUCache is a thread-safe LRU cache for TLS certificates, keyed by hostname.
// Uses sync.Mutex (not RWMutex) because Get must call MoveToFront which
// mutates the list, requiring an exclusive lock even on the read path.
// This is the standard LRU tradeoff; the L2 Redis cache behind this
// makes the lock duration negligible (map lookup + pointer move).
type LRUCache struct {
	mu      sync.Mutex
	items   map[string]*list.Element
	order   *list.List
	maxSize int
}

// NewLRUCache creates a new LRU cache with the given maximum number of entries.
func NewLRUCache(maxSize int) *LRUCache {
	return &LRUCache{
		items:   make(map[string]*list.Element),
		order:   list.New(),
		maxSize: maxSize,
	}
}

// Get returns the cached certificate for hostname, or nil if not found or expired.
func (c *LRUCache) Get(hostname string) *tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[hostname]
	if !ok {
		return nil
	}

	entry := elem.Value.(*lruEntry)
	if time.Now().After(entry.expiry) {
		// Expired: remove from cache
		c.order.Remove(elem)
		delete(c.items, hostname)
		return nil
	}

	// Move to front (most recently used)
	c.order.MoveToFront(elem)
	return entry.cert
}

// Put stores a certificate with the given TTL. If the cache is at capacity,
// the least recently used entry is evicted.
func (c *LRUCache) Put(hostname string, cert *tls.Certificate, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// If already present, update in place
	if elem, ok := c.items[hostname]; ok {
		entry := elem.Value.(*lruEntry)
		entry.cert = cert
		entry.expiry = time.Now().Add(ttl)
		c.order.MoveToFront(elem)
		return
	}

	// Evict if at capacity
	for c.order.Len() >= c.maxSize {
		back := c.order.Back()
		if back == nil {
			break
		}
		evicted := back.Value.(*lruEntry)
		c.order.Remove(back)
		delete(c.items, evicted.hostname)
	}

	// Insert new entry at front
	entry := &lruEntry{
		hostname: hostname,
		cert:     cert,
		expiry:   time.Now().Add(ttl),
	}
	elem := c.order.PushFront(entry)
	c.items[hostname] = elem
}
