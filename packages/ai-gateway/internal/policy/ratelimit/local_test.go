package ratelimit

import (
	"testing"
	"time"
)

func TestLocalLimiter_AllowWithinLimit(t *testing.T) {
	ll := NewLocalLimiter()
	for i := range 5 {
		allowed, _ := ll.Allow("key1", 5, 60000)
		if !allowed {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	allowed, retry := ll.Allow("key1", 5, 60000)
	if allowed {
		t.Fatal("6th request should be blocked")
	}
	if retry < 1 {
		t.Fatalf("retryAfter = %d, want >= 1", retry)
	}
}

func TestLocalLimiter_WindowExpiry(t *testing.T) {
	ll := NewLocalLimiter()
	for range 3 {
		ll.Allow("key1", 3, 50) // 50ms window
	}
	time.Sleep(60 * time.Millisecond)
	allowed, _ := ll.Allow("key1", 3, 50)
	if !allowed {
		t.Fatal("should be allowed after window expiry")
	}
}

func TestLocalLimiter_ZeroLimit(t *testing.T) {
	ll := NewLocalLimiter()
	allowed, _ := ll.Allow("key1", 0, 60000)
	if !allowed {
		t.Fatal("zero limit should always allow")
	}
}

func TestLocalLimiter_Cleanup(t *testing.T) {
	ll := NewLocalLimiter()
	// Add an active entry.
	ll.Allow("active", 10, 60_000)
	// Inject a stale entry with a timestamp well in the past.
	ll.mu.Lock()
	ll.windows["stale"] = &window{timestamps: []int64{0}}
	ll.mu.Unlock()

	ll.Cleanup()

	ll.mu.Lock()
	defer ll.mu.Unlock()
	if _, exists := ll.windows["stale"]; exists {
		t.Error("stale window should be cleaned up")
	}
	if _, exists := ll.windows["active"]; !exists {
		t.Error("active window should still exist")
	}
}
