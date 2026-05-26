package cache

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"sync"
	"testing"
	"time"
)

// makeDummyCert creates a minimal tls.Certificate for testing (key only, no real cert).
func makeDummyCert(t *testing.T) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &tls.Certificate{PrivateKey: key}
}

func TestLRU_GetPut(t *testing.T) {
	cache := NewLRUCache(10)
	cert := makeDummyCert(t)

	// Get on empty cache returns nil
	if got := cache.Get("example.com"); got != nil {
		t.Error("expected nil for empty cache")
	}

	// Put and Get
	cache.Put("example.com", cert, 10*time.Minute)
	got := cache.Get("example.com")
	if got == nil {
		t.Fatal("expected non-nil cert after Put")
	}
	if got != cert {
		t.Error("returned cert does not match stored cert")
	}
}

func TestLRU_Update(t *testing.T) {
	cache := NewLRUCache(10)
	cert1 := makeDummyCert(t)
	cert2 := makeDummyCert(t)

	cache.Put("example.com", cert1, 10*time.Minute)
	cache.Put("example.com", cert2, 10*time.Minute)

	got := cache.Get("example.com")
	if got != cert2 {
		t.Error("expected updated cert")
	}
	if cache.order.Len() != 1 {
		t.Errorf("cache size = %d, want 1 after update of same key", cache.order.Len())
	}
}

func TestLRU_Eviction(t *testing.T) {
	cache := NewLRUCache(3)

	for i := range 3 {
		cache.Put(fmt.Sprintf("host%d.com", i), makeDummyCert(t), 10*time.Minute)
	}

	// All three should be present
	for i := range 3 {
		if cache.Get(fmt.Sprintf("host%d.com", i)) == nil {
			t.Errorf("host%d.com should be in cache", i)
		}
	}

	// Adding a 4th should evict the LRU entry.
	// After the Gets above, access order is: host2 (front), host1, host0 (back).
	// So host0 gets evicted.
	cache.Put("host3.com", makeDummyCert(t), 10*time.Minute)

	if cache.Get("host0.com") != nil {
		t.Error("host0.com should have been evicted")
	}
	if cache.Get("host3.com") == nil {
		t.Error("host3.com should be in cache")
	}
}

func TestLRU_TTLExpiry(t *testing.T) {
	cache := NewLRUCache(10)
	cert := makeDummyCert(t)

	// Use a very short TTL
	cache.Put("expire.com", cert, 1*time.Millisecond)

	// Wait for expiry
	time.Sleep(5 * time.Millisecond)

	if got := cache.Get("expire.com"); got != nil {
		t.Error("expected nil for expired entry")
	}
}

func TestLRU_Concurrent(t *testing.T) {
	cache := NewLRUCache(100)
	const goroutines = 50
	const opsPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for g := range goroutines {
		go func(id int) {
			defer wg.Done()
			for i := range opsPerGoroutine {
				hostname := fmt.Sprintf("host-%d-%d.com", id, i%10)
				cert := &tls.Certificate{PrivateKey: nil} // minimal for concurrency test
				cache.Put(hostname, cert, 10*time.Minute)
				cache.Get(hostname)
			}
		}(g)
	}

	wg.Wait()
	// If we reach here without data races (run with -race), the test passes.
}
