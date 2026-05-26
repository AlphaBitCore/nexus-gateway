package ratelimit

import (
	"context"
	"testing"
)

func makeRateLimiterConfig(maxRequests, windowSeconds int, keyType string) *HookConfig {
	return &HookConfig{
		ID:               "rl-1",
		ImplementationID: "rate-limiter",
		Name:             "Test Rate Limiter",
		Config: map[string]any{
			"maxRequests":   float64(maxRequests),
			"windowSeconds": float64(windowSeconds),
			"keyType":       keyType,
		},
	}
}

func TestRateLimiter_WithinLimit(t *testing.T) {
	hook, err := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	input := &HookInput{SourceIP: "10.0.0.1"}

	for i := range 5 {
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute[%d]: %v", i, err)
		}
		if result.Decision != Approve {
			t.Errorf("request %d: expected APPROVE, got %s", i+1, result.Decision)
		}
	}
}

func TestRateLimiter_OverLimit(t *testing.T) {
	hook, err := NewRateLimiter(makeRateLimiterConfig(3, 60, "source_ip"))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	input := &HookInput{SourceIP: "10.0.0.2"}

	// First 3 should be approved
	for i := range 3 {
		result, err := hook.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute[%d]: %v", i, err)
		}
		if result.Decision != Approve {
			t.Errorf("request %d: expected APPROVE, got %s", i+1, result.Decision)
		}
	}

	// 4th should be rejected
	result, err := hook.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute[4]: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("request 4: expected REJECT_HARD, got %s", result.Decision)
	}
	if result.ReasonCode != "RATE_LIMIT_EXCEEDED" {
		t.Errorf("expected RATE_LIMIT_EXCEEDED, got %s", result.ReasonCode)
	}
}

func TestRateLimiter_WindowReset(t *testing.T) {
	hook, err := NewRateLimiter(makeRateLimiterConfig(2, 60, "source_ip"))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}

	rl := hook.(*RateLimiter)
	input := &HookInput{SourceIP: "10.0.0.3"}

	// Use up the limit
	for i := range 2 {
		_, err := rl.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("Execute[%d]: %v", i, err)
		}
	}

	// Should be over limit now
	result, err := rl.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Decision != RejectHard {
		t.Errorf("expected REJECT_HARD before window reset, got %s", result.Decision)
	}

	// Simulate window expiry by resetting the bucket's windowEnd to the past.
	val, ok := rl.buckets.Load("10.0.0.3")
	if !ok {
		t.Fatal("bucket not found for 10.0.0.3")
	}
	b := val.(*bucket)
	b.mu.Lock()
	b.windowEnd = 0 // force window expiry
	b.mu.Unlock()

	// After window reset, the next request should be approved
	result, err = rl.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute after reset: %v", err)
	}
	if result.Decision != Approve {
		t.Errorf("expected APPROVE after window reset, got %s", result.Decision)
	}
}
