package ratelimit

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// DistributedLimiter is an optional interface for Redis-backed rate limiting.
// Implementations must be safe for concurrent use.
type DistributedLimiter interface {
	Allow(key string, limit int, windowMs int64) (allowed bool, retryAfterSec int)
}

// rateLimiterCleanupInterval controls how often expired buckets are swept.
const rateLimiterCleanupInterval = 1000

// bucket tracks request counts within a sliding window.
// Uses a mutex for cross-field atomicity on reset.
type bucket struct {
	mu        sync.Mutex
	count     int64
	windowEnd int64 // unix-nano timestamp when the current window expires
}

// RateLimiter enforces per-key request rate limits.
//
// Rate limiting is process-local (sync.Map). Under horizontal scaling the
// effective limit is approximately N × configured_limit where N is the
// replica count; cross-instance accuracy is ~10%. When a DistributedLimiter
// is wired via SetDistributed, it replaces the local counter.
//
// Applies to all endpoints and modalities via AnyEndpointAnyModality.
type RateLimiter struct {
	core.AnyEndpointAnyModality
	cfg           *core.HookConfig
	maxRequests   int64
	windowSeconds int64
	keyType       string // "source_ip" or "target_host"
	buckets       sync.Map
	execCount     atomic.Int64
	distributed   DistributedLimiter // optional, set externally
}

// NewRateLimiter constructs a RateLimiter from declarative config.
//
// Expected config shape:
//
//	{
//	  "maxRequests": 100,
//	  "windowSeconds": 60,
//	  "keyType": "source_ip|target_host"
//	}
func NewRateLimiter(cfg *core.HookConfig) (core.Hook, error) {
	maxReq, ok := core.ToInt64(cfg.Config["maxRequests"])
	if !ok || maxReq <= 0 {
		return nil, fmt.Errorf("rate-limiter: 'maxRequests' must be a positive integer")
	}
	windowSec, ok := core.ToInt64(cfg.Config["windowSeconds"])
	if !ok || windowSec <= 0 {
		return nil, fmt.Errorf("rate-limiter: 'windowSeconds' must be a positive integer")
	}

	keyType, _ := cfg.Config["keyType"].(string)
	keyType = strings.ToLower(keyType)
	if keyType == "" {
		keyType = "source_ip"
	}
	if keyType != "source_ip" && keyType != "target_host" {
		return nil, fmt.Errorf("rate-limiter: unknown keyType %q (expected source_ip or target_host)", keyType)
	}

	return &RateLimiter{
		cfg:           cfg,
		maxRequests:   maxReq,
		windowSeconds: windowSec,
		keyType:       keyType,
	}, nil
}

// SetDistributed replaces the process-local limiter with a distributed one.
// Must be called before concurrent use.
func (rl *RateLimiter) SetDistributed(d DistributedLimiter) {
	rl.distributed = d
}

// cleanExpiredBuckets removes buckets whose window has expired.
// Uses CompareAndDelete to avoid removing a freshly-created bucket
// that a concurrent LoadOrStore just stored for the same key.
func (rl *RateLimiter) cleanExpiredBuckets() {
	now := time.Now().UnixNano()
	rl.buckets.Range(func(key, value any) bool {
		b := value.(*bucket)
		b.mu.Lock()
		expired := now >= b.windowEnd
		b.mu.Unlock()
		if expired {
			rl.buckets.CompareAndDelete(key, value)
		}
		return true
	})
}

func (rl *RateLimiter) Execute(_ context.Context, input *core.HookInput) (*core.HookResult, error) {
	start := time.Now()

	// Periodic cleanup of expired buckets to prevent unbounded memory growth.
	if rl.execCount.Add(1)%rateLimiterCleanupInterval == 0 {
		rl.cleanExpiredBuckets()
	}

	result := &core.HookResult{
		HookID:           rl.cfg.ID,
		ImplementationID: rl.cfg.ImplementationID,
		HookName:         rl.cfg.Name,
	}

	key := input.SourceIP
	if rl.keyType == "target_host" {
		key = input.TargetHost
	}

	// Fast path: delegate to distributed limiter if available.
	if rl.distributed != nil {
		allowed, _ := rl.distributed.Allow(key, int(rl.maxRequests), rl.windowSeconds*1000)
		if !allowed {
			result.Decision = core.RejectHard
			result.Reason = fmt.Sprintf("rate limit exceeded: distributed limiter blocked for key %s", key)
			result.ReasonCode = "RATE_LIMIT_EXCEEDED"
			result.LatencyMs = int(time.Since(start).Milliseconds())
			return result, nil
		}
		result.Decision = core.Approve
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	val, ok := rl.buckets.Load(key)
	if !ok {
		val, _ = rl.buckets.LoadOrStore(key, &bucket{})
	}
	b := val.(*bucket)

	now := time.Now().UnixNano()
	windowDur := rl.windowSeconds * int64(time.Second)

	b.mu.Lock()
	if now >= b.windowEnd {
		b.count = 1
		b.windowEnd = now + windowDur
		b.mu.Unlock()
		result.Decision = core.Approve
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}
	b.count++
	count := b.count
	b.mu.Unlock()

	if count > rl.maxRequests {
		result.Decision = core.RejectHard
		result.Reason = fmt.Sprintf("rate limit exceeded: %d/%d requests in %ds window", count, rl.maxRequests, rl.windowSeconds)
		result.ReasonCode = "RATE_LIMIT_EXCEEDED"
		result.LatencyMs = int(time.Since(start).Milliseconds())
		return result, nil
	}

	result.Decision = core.Approve
	result.LatencyMs = int(time.Since(start).Milliseconds())
	return result, nil
}
