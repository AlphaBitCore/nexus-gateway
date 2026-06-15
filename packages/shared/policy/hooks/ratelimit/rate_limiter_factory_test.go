package ratelimit

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// --- Factory error paths ----------------------------------------------------

func TestRateLimiter_Factory_MissingMaxRequestsRejected(t *testing.T) {
	_, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"windowSeconds": float64(60),
	}})
	if err == nil {
		t.Fatal("missing maxRequests should error")
	}
	if !strings.Contains(err.Error(), "maxRequests") {
		t.Errorf("error should mention maxRequests: %v", err)
	}
}

func TestRateLimiter_Factory_NegativeMaxRequestsRejected(t *testing.T) {
	_, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests":   float64(-1),
		"windowSeconds": float64(60),
	}})
	if err == nil {
		t.Fatal("negative maxRequests should error")
	}
}

func TestRateLimiter_Factory_MissingWindowSecondsRejected(t *testing.T) {
	_, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests": float64(10),
	}})
	if err == nil {
		t.Fatal("missing windowSeconds should error")
	}
	if !strings.Contains(err.Error(), "windowSeconds") {
		t.Errorf("error should mention windowSeconds: %v", err)
	}
}

func TestRateLimiter_Factory_NegativeWindowSecondsRejected(t *testing.T) {
	_, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests":   float64(10),
		"windowSeconds": float64(0),
	}})
	if err == nil {
		t.Fatal("zero windowSeconds should error")
	}
}

func TestRateLimiter_Factory_UnknownKeyTypeRejected(t *testing.T) {
	_, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests":   float64(10),
		"windowSeconds": float64(60),
		"keyType":       "user_id",
	}})
	if err == nil {
		t.Fatal("unknown keyType should error")
	}
	if !strings.Contains(err.Error(), "unknown keyType") {
		t.Errorf("error should mention unknown keyType: %v", err)
	}
}

func TestRateLimiter_Factory_DefaultKeyTypeIsSourceIP(t *testing.T) {
	// Absent keyType must default to source_ip (the safer choice — uniquely
	// limits per-client).
	h, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests":   float64(10),
		"windowSeconds": float64(60),
	}})
	if err != nil {
		t.Fatalf("default keyType: %v", err)
	}
	rl := h.(*RateLimiter)
	if rl.keyType != "source_ip" {
		t.Errorf("default keyType: %q want source_ip", rl.keyType)
	}
}

func TestRateLimiter_Factory_KeyTypeCaseInsensitive(t *testing.T) {
	h, err := NewRateLimiter(&HookConfig{Config: map[string]any{
		"maxRequests":   float64(10),
		"windowSeconds": float64(60),
		"keyType":       "TARGET_HOST",
	}})
	if err != nil {
		t.Fatalf("uppercase keyType: %v", err)
	}
	rl := h.(*RateLimiter)
	if rl.keyType != "target_host" {
		t.Errorf("uppercase normalisation: %q", rl.keyType)
	}
}

// --- target_host keying -----------------------------------------------------

func TestRateLimiter_Execute_TargetHostKeyType(t *testing.T) {
	// Two requests from different SourceIPs but the same TargetHost
	// must share a bucket when keyType=target_host.
	h, err := NewRateLimiter(makeRateLimiterConfig(2, 60, "target_host"))
	if err != nil {
		t.Fatalf("NewRateLimiter: %v", err)
	}
	a := &HookInput{SourceIP: "10.0.0.1", TargetHost: "api.example.com"}
	b := &HookInput{SourceIP: "10.0.0.2", TargetHost: "api.example.com"}

	r1, _ := h.Execute(context.Background(), a)
	r2, _ := h.Execute(context.Background(), b)
	r3, _ := h.Execute(context.Background(), a) // 3rd hit on same host
	if r1.Decision != Approve || r2.Decision != Approve {
		t.Errorf("first two requests: %s %s", r1.Decision, r2.Decision)
	}
	if r3.Decision != RejectHard {
		t.Errorf("3rd request on same host: got %s want RejectHard", r3.Decision)
	}
}

// --- cleanExpiredBuckets ----------------------------------------------------

func TestRateLimiter_CleanExpiredBuckets_RemovesExpired(t *testing.T) {
	h, _ := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	rl := h.(*RateLimiter)

	// Populate the bucket map with one expired and one fresh entry.
	_, _ = rl.Execute(context.Background(), &HookInput{SourceIP: "10.0.0.10"})
	_, _ = rl.Execute(context.Background(), &HookInput{SourceIP: "10.0.0.11"})

	// Force the first one's window into the past.
	val, ok := rl.buckets.Load("10.0.0.10")
	if !ok {
		t.Fatal("bucket 10.0.0.10 missing")
	}
	b := val.(*bucket)
	b.mu.Lock()
	b.windowEnd = 0
	b.mu.Unlock()

	// Verify cleanup removes only the expired one.
	rl.cleanExpiredBuckets()
	if _, exists := rl.buckets.Load("10.0.0.10"); exists {
		t.Error("expired bucket should have been deleted")
	}
	if _, exists := rl.buckets.Load("10.0.0.11"); !exists {
		t.Error("fresh bucket should remain")
	}
}

func TestRateLimiter_CleanExpiredBuckets_EmptyMap(t *testing.T) {
	// Cleanup must be a safe no-op when the bucket map is empty.
	h, _ := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	rl := h.(*RateLimiter)
	rl.cleanExpiredBuckets() // must not panic
}

// --- Distributed limiter ----------------------------------------------------

// fakeDistributedLimiter is a test double for the DistributedLimiter
// interface. Records every Allow call and returns canned decisions.
type fakeDistributedLimiter struct {
	mu         sync.Mutex
	calls      []fakeCall
	allow      bool
	retryAfter int
}

type fakeCall struct {
	key      string
	limit    int
	windowMs int64
}

func (f *fakeDistributedLimiter) Allow(key string, limit int, windowMs int64) (bool, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{key: key, limit: limit, windowMs: windowMs})
	return f.allow, f.retryAfter
}

func TestRateLimiter_SetDistributed_Allow(t *testing.T) {
	h, _ := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	rl := h.(*RateLimiter)

	fake := &fakeDistributedLimiter{allow: true}
	rl.SetDistributed(fake)

	res, err := rl.Execute(context.Background(), &HookInput{SourceIP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != Approve {
		t.Errorf("distributed allow: got %s want Approve", res.Decision)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 distributed call, got %d", len(fake.calls))
	}
	// Arguments correctly passed through: limit + window in ms.
	if fake.calls[0].key != "1.2.3.4" || fake.calls[0].limit != 5 || fake.calls[0].windowMs != 60_000 {
		t.Errorf("call args: %+v want {1.2.3.4 5 60000}", fake.calls[0])
	}
}

func TestRateLimiter_SetDistributed_Deny(t *testing.T) {
	h, _ := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	rl := h.(*RateLimiter)

	fake := &fakeDistributedLimiter{allow: false, retryAfter: 30}
	rl.SetDistributed(fake)

	res, err := rl.Execute(context.Background(), &HookInput{SourceIP: "1.2.3.4"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Decision != RejectHard {
		t.Errorf("distributed deny: got %s want RejectHard", res.Decision)
	}
	if res.ReasonCode != "RATE_LIMIT_EXCEEDED" {
		t.Errorf("reasonCode: %q", res.ReasonCode)
	}
	if !strings.Contains(res.Reason, "distributed") {
		t.Errorf("reason should mention distributed: %q", res.Reason)
	}
}

func TestRateLimiter_SetDistributed_BypassesLocalBuckets(t *testing.T) {
	// Once a DistributedLimiter is set, the process-local sync.Map path
	// must NOT be touched (no bucket created).
	h, _ := NewRateLimiter(makeRateLimiterConfig(5, 60, "source_ip"))
	rl := h.(*RateLimiter)
	fake := &fakeDistributedLimiter{allow: true}
	rl.SetDistributed(fake)
	_, _ = rl.Execute(context.Background(), &HookInput{SourceIP: "1.2.3.4"})

	if _, exists := rl.buckets.Load("1.2.3.4"); exists {
		t.Error("distributed path created a local bucket; should have bypassed")
	}
}

// --- Periodic cleanup triggered from Execute --------------------------------

func TestRateLimiter_Execute_PeriodicCleanupTriggered(t *testing.T) {
	// Every rateLimiterCleanupInterval calls trigger cleanExpiredBuckets.
	// Drive enough calls to hit the threshold, and ensure no panic occurs.
	h, _ := NewRateLimiter(makeRateLimiterConfig(10000, 60, "source_ip"))
	in := &HookInput{SourceIP: "10.0.0.99"}
	// rateLimiterCleanupInterval = 1000; drive 1001 calls to ensure
	// cleanExpiredBuckets fires at least once.
	for i := range rateLimiterCleanupInterval + 1 {
		_, err := h.Execute(context.Background(), in)
		if err != nil {
			t.Fatalf("Execute[%d]: %v", i, err)
		}
	}
}

// --- toInt64 ----------------------------------------------------------------

func TestToInt64_AcceptsKnownNumericTypes(t *testing.T) {
	cases := []struct {
		in   any
		want int64
		ok   bool
	}{
		{float64(42), 42, true},
		{int(7), 7, true},
		{int64(123), 123, true},
		{"42", 0, false},     // string rejected
		{nil, 0, false},      // nil rejected
		{[]int{1}, 0, false}, // slice rejected
		{true, 0, false},     // bool rejected
	}
	for _, c := range cases {
		got, ok := toInt64(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("toInt64(%v): got (%d,%v) want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
