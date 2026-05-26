package wiring

import (
	"testing"
)

// TestInitRateLimiter_nilRdbReturnsLocalOnly verifies that a nil Redis client
// produces the local-only fallback limiter (does not panic or return nil).
func TestInitRateLimiter_nilRdbReturnsLocalOnly(t *testing.T) {
	limiter := InitRateLimiter(nil, discardLogger())
	if limiter == nil {
		t.Fatal("expected non-nil limiter for nil rdb (local-only mode)")
	}
}
