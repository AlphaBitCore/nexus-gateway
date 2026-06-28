// Package credstats provides a fire-and-forget Redis buffer for credential
// usage statistics and circuit breaker state management.
//
// Usage statistics are written under cred:stats:{id} on every upstream
// attempt; circuit-breaker state under cred:circuit:{id} on transitions only.
// All Redis keys, field names, state enums, and threshold defaults come from
// packages/shared/schemas/credstate — the single source of truth.
//
// Dirty-set semantics:
//
//   - cred:stats:dirty receives credID after every attempt
//     (counter / timestamp deltas always need persisting).
//   - cred:circuit:dirty receives credID only on a state transition
//     (closed → open, open → half_open, half_open → closed). Increments
//     of the live auth_fails counter below the threshold do not mark
//     dirty — that counter is read live from Redis by the admin API and
//     never persists to the Credential table.
//
// All writes are pipelined with a 200 ms deadline. A nil Buffer is safe
// to use (every method is a no-op); construction does not require Redis
// to be reachable.
package credstats

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// perfNoRedis is a THROWAWAY experiment switch (NEXUS_PERF_NO_REDIS=1) that
// skips the per-attempt credential-stats Redis writes so an A/B run can measure
// "what if Redis were free". NOT for production — see usage_cache.go.
var perfNoRedis = os.Getenv("NEXUS_PERF_NO_REDIS") == "1"

// maxFailReasonLen caps the stored upstream error text (fail_reason field).
// Provider errors can be arbitrarily large; the field is operator-display
// only, so truncating keeps the per-credential Redis hash bounded.
const maxFailReasonLen = 256

// ThresholdsResolver returns the effective Thresholds for a credential.
// Production wiring resolves: per-credential override (from Credential
// cache) merged on top of Hub-shadow globals merged on top of
// credstate.DefaultThresholds. The Buffer calls the resolver synchronously
// inside RecordAttempt — implementations must be cheap (cache hit, no DB).
type ThresholdsResolver func(credentialID string) credstate.Thresholds

// Buffer is a non-blocking Redis writer for per-credential attempt stats
// and circuit-breaker transitions. A nil Buffer is safe to use — all
// methods are no-ops. Per-credential thresholds are resolved synchronously
// inside RecordAttempt by the injected ThresholdsResolver.
type Buffer struct {
	rdb      redis.Cmdable
	logger   *slog.Logger
	resolver ThresholdsResolver
	metrics  *Metrics

	// agg is the T1b write-behind stats accumulator. When non-nil, a successful
	// RecordAttempt accumulates the count/timestamps in-process instead of the
	// per-attempt Redis stats pipeline; a background flusher drains to Redis.
	// nil = legacy synchronous path. Failures (rare) always write synchronously.
	agg *statsAggregator

	// clean is the T1b circuit clean-set: credentials confirmed to have NO
	// circuit hash (never failed → definitely closed, auth_fails absent). A
	// success for a clean credential skips the circuit HGet + auth_fails reset
	// entirely. A failure removes the credential (it now has a hash). Enforcement
	// still reads Redis at credpool selection, so a stale clean entry on one
	// instance is harmless (that instance won't be routed an OPEN credential).
	cleanMu sync.Mutex
	clean   map[string]struct{}
}

// cleanSetCap bounds the circuit clean-set so a long-lived process with many
// distinct credentials cannot grow it unbounded; on overflow it is cleared and
// credentials re-confirm clean on their next success (one HGet each).
const cleanSetCap = 100_000

// New constructs a Buffer. Pass nil rdb to disable Redis (every method
// becomes a no-op). Pass nil resolver to fall back to
// credstate.DefaultThresholds for every credential. Pass nil metrics to
// disable Prometheus collection.
func New(rdb redis.Cmdable, logger *slog.Logger, resolver ThresholdsResolver, metrics *Metrics) *Buffer {
	if resolver == nil {
		resolver = func(string) credstate.Thresholds { return credstate.DefaultThresholds }
	}
	return &Buffer{
		rdb:      rdb,
		logger:   logger,
		resolver: resolver,
		metrics:  metrics,
	}
}

// classify maps a status code to a Prometheus label.
func classify(statusCode int) string {
	switch {
	case statusCode == 0:
		return "network"
	case statusCode >= 200 && statusCode < 300:
		return "2xx"
	case statusCode == 401 || statusCode == 403:
		return "auth_fail"
	case statusCode == 429:
		return "rate_limit"
	case statusCode >= 400 && statusCode < 500:
		return "4xx"
	case statusCode >= 500 && statusCode < 600:
		return "5xx"
	default:
		return "other"
	}
}

// luaOpenCircuitAuthFail atomically increments auth_fails and opens the
// circuit when the configured threshold is reached. Marks the credential
// dirty only on the transition to OPEN; sub-threshold increments stay
// quiet (no DB writes). Returns the new auth_fails value so the caller
// can attribute Prometheus increments.
var luaOpenCircuitAuthFail = redis.NewScript(`
local key       = KEYS[1]
local dirtySet  = KEYS[2]
local credID    = KEYS[3]
local threshold = tonumber(ARGV[1])
local now       = ARGV[2]
local fails     = redis.call('HINCRBY', key, 'auth_fails', 1)
if fails >= threshold then
  redis.call('HSET', key,
    'state',       'open',
    'opened_at',   now,
    'open_reason', 'auth_fail')
  redis.call('SADD', dirtySet, credID)
end
return fails
`)

// luaReadAndResetCount atomically reads and zeroes the stats hash counter.
// Used by the Hub credential-stats-flush job.
var luaReadAndResetCount = redis.NewScript(`
local cnt = tonumber(redis.call('HGET', KEYS[1], 'cnt') or '0')
if cnt and cnt > 0 then
  redis.call('HSET', KEYS[1], 'cnt', 0)
end
return cnt or 0
`)

// RecordAttempt records one upstream attempt against credentialID.
// statusCode is the HTTP response (0 for network errors); errMsg is the
// human-readable failure reason on non-2xx responses.
//
// Circuit transitions (governed by the resolved Thresholds):
//
//   - 401/403 → HINCRBY auth_fails. If the new value reaches
//     AuthFailThreshold the circuit transitions to OPEN with
//     reason=auth_fail and credID is added to cred:circuit:dirty.
//   - 429     → circuit transitions to OPEN with reason=rate_limit,
//     opened_at=now, next_probe_at=now+RateLimitCooldownSeconds.
//     Always marks dirty.
//   - 2xx     → if the circuit is OPEN or HALF_OPEN, the hash is DEL'd
//     (closed) and dirty is marked. Otherwise the auth_fails counter is
//     reset (no transition, no dirty mark).
//   - 5xx / 0 → no circuit change. Stats are still recorded.
//
// The cred:circuit:dirty set is the input queue to the Hub
// credential-circuit-flush job. The live auth_fails counter is read by
// the admin API but is intentionally never persisted to the Credential
// table.
func (b *Buffer) RecordAttempt(credentialID string, statusCode int, errMsg string) {
	if b == nil || b.rdb == nil || credentialID == "" || perfNoRedis {
		return
	}
	thresholds := b.resolver(credentialID)

	t0 := time.Now()
	defer func() { b.metrics.observeWrite(time.Since(t0)) }()

	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(credstate.WriteTimeoutMillis)*time.Millisecond,
	)
	defer cancel()

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	statsKey := credstate.StatsKey(credentialID)
	circuitKey := credstate.CircuitKey(credentialID)

	success := statusCode >= 200 && statusCode < 300
	authFail := statusCode == 401 || statusCode == 403
	rateLimited := statusCode == 429

	b.metrics.incAttempt(classify(statusCode))

	// --- Stats: T1b write-behind for the SUCCESS hot path ---
	// A successful attempt's count/timestamps are operational stats (never
	// billed); accumulate them in-process and let the background flusher persist
	// them, keeping the per-request Redis stats pipeline off the hot path.
	// Failures (rare) fall through to the synchronous pipeline so fail_at /
	// fail_reason and the circuit transitions below stay immediate.
	if b.agg != nil && success {
		b.agg.recordSuccess(credentialID, nowStr)
		// Circuit on success still runs below (closing a recovered circuit and
		// resetting auth_fails are correctness-relevant, not bookkeeping).
		b.recordSuccessCircuit(ctx, circuitKey, credentialID)
		return
	}

	// --- Stats pipeline (synchronous path: write-behind off, or a failure) ---
	pipe := b.rdb.Pipeline()
	pipe.HIncrBy(ctx, statsKey, credstate.StatsFieldCount, 1)
	pipe.HSet(ctx, statsKey, credstate.StatsFieldUsedAt, nowStr)
	switch {
	case success:
		pipe.HSet(ctx, statsKey, credstate.StatsFieldOkAt, nowStr)
	case authFail:
		pipe.HSet(ctx, statsKey, credstate.StatsFieldFailAt, nowStr)
		if errMsg != "" {
			// Cap the upstream error text before persisting: errMsg is the
			// verbatim provider error and can be arbitrarily long (full HTML
			// error pages, stack traces). The fail_reason field is only used
			// for operator display, so 256 chars is ample and keeps the
			// per-credential Redis hash bounded.
			if len(errMsg) > maxFailReasonLen {
				errMsg = errMsg[:maxFailReasonLen]
			}
			pipe.HSet(ctx, statsKey, credstate.StatsFieldFailReason, errMsg)
		}
	}
	pipe.SAdd(ctx, credstate.StatsDirtySet, credentialID)
	if _, err := pipe.Exec(ctx); err != nil {
		b.warn("stats write failed", credentialID, err)
		b.metrics.incRedisFailure("stats")
	}

	// --- Circuit breaker ---
	switch {
	case authFail:
		b.unmarkClean(credentialID) // a failing credential now has a circuit hash
		fails, err := luaOpenCircuitAuthFail.Run(ctx, b.rdb,
			[]string{circuitKey, credstate.CircuitDirtySet, credentialID},
			thresholds.AuthFailThreshold, nowStr,
		).Int64()
		if err != nil {
			b.warn("circuit auth-fail update failed", credentialID, err)
			b.metrics.incRedisFailure("circuit_authfail")
			return
		}
		if fails >= int64(thresholds.AuthFailThreshold) {
			b.metrics.incCircuit(credstate.CircuitOpen, credstate.ReasonAuthFail)
		} else {
			b.metrics.incAuthFailIncrement()
		}

	case rateLimited:
		b.unmarkClean(credentialID) // a rate-limited credential now has a circuit hash
		probeAt := now.Add(time.Duration(thresholds.RateLimitCooldownSeconds) * time.Second).Format(time.RFC3339Nano)
		pipe := b.rdb.Pipeline()
		pipe.HSet(ctx, circuitKey,
			credstate.CircuitFieldState, credstate.CircuitOpen,
			credstate.CircuitFieldOpenedAt, nowStr,
			credstate.CircuitFieldNextProbe, probeAt,
			credstate.CircuitFieldOpenReason, credstate.ReasonRateLimit,
		)
		pipe.SAdd(ctx, credstate.CircuitDirtySet, credentialID)
		if _, err := pipe.Exec(ctx); err != nil {
			b.warn("circuit rate-limit open failed", credentialID, err)
			b.metrics.incRedisFailure("circuit_ratelimit")
			return
		}
		b.metrics.incCircuit(credstate.CircuitOpen, credstate.ReasonRateLimit)

	case success:
		b.recordSuccessCircuit(ctx, circuitKey, credentialID)
	}
}

// recordSuccessCircuit applies the success-path circuit transition: if the
// circuit is currently OPEN/HALF_OPEN, close it (DEL the hash + mark dirty so the
// Hub circuit-flush job persists the recovery); otherwise reset the running
// auth_fails counter. Shared by the synchronous RecordAttempt path and the T1b
// write-behind success path — circuit transitions are correctness-relevant and
// stay on Redis even under write-behind (only the stats bookkeeping is deferred).
// luaSuccessCircuit applies the whole success-path circuit transition in ONE
// atomic round-trip and reports which case fired so the caller can update the
// clean-set + metrics:
//   - "absent": no circuit hash → the credential has never failed; nothing to
//     do (auth_fails is absent = 0). Caller marks it clean to skip Redis next time.
//   - "closed": the circuit was OPEN/HALF_OPEN → DEL'd + dirty-marked (recovery).
//   - "reset":  a sub-threshold auth_fails counter existed → reset to 0.
//
// Replaces the legacy HGet + conditional-HSet (up to 2 round-trips) with one.
var luaSuccessCircuit = redis.NewScript(`
local key      = KEYS[1]
local dirtySet = KEYS[2]
local credID   = KEYS[3]
local fState   = ARGV[1]
local fFails   = ARGV[2]
local sOpen    = ARGV[3]
local sHalf    = ARGV[4]
if redis.call('EXISTS', key) == 0 then return 'absent' end
local state = redis.call('HGET', key, fState)
if state == sOpen or state == sHalf then
  redis.call('DEL', key)
  redis.call('SADD', dirtySet, credID)
  return 'closed'
end
redis.call('HSET', key, fFails, 0)
return 'reset'
`)

func (b *Buffer) recordSuccessCircuit(ctx context.Context, circuitKey, credentialID string) {
	// T1b clean-set fast path: a credential confirmed to have no circuit hash
	// needs no Redis at all on success — skip entirely.
	if b.isClean(credentialID) {
		return
	}
	res, err := luaSuccessCircuit.Run(ctx, b.rdb,
		[]string{circuitKey, credstate.CircuitDirtySet, credentialID},
		credstate.CircuitFieldState, credstate.CircuitFieldAuthFails,
		credstate.CircuitOpen, credstate.CircuitHalfOpen,
	).Text()
	if err != nil && !errors.Is(err, redis.Nil) {
		b.warn("circuit success update failed", credentialID, err)
		b.metrics.incRedisFailure("circuit_success")
		return
	}
	switch res {
	case "absent":
		// Never failed → confirm clean so future successes skip Redis.
		b.markClean(credentialID)
	case "closed":
		b.metrics.incCircuit(credstate.CircuitClosed, "")
	}
}

// MarkCircuitDirty adds credentialID to cred:circuit:dirty. Used by
// callers outside this package that mutate the circuit hash directly
// (today: credpool's auto-promote-to-half_open path on selection). The
// dedicated transitions in RecordAttempt mark dirty inline and do not
// require this helper.
func MarkCircuitDirty(ctx context.Context, rdb redis.Cmdable, credentialID string) error {
	if rdb == nil || credentialID == "" {
		return nil
	}
	return rdb.SAdd(ctx, credstate.CircuitDirtySet, credentialID).Err()
}

func (b *Buffer) warn(msg, credID string, err error) {
	if b.logger != nil {
		b.logger.Warn("credstats: "+msg, "credentialID", credID, "error", err)
	}
}

// Helpers consumed by the Hub credential-stats-flush job.

// ReadAndResetCount atomically reads the cnt field of cred:stats:{id}
// and zeroes it. Returns 0 when the key is absent. The Hub flush job
// drives this; callers from AI Gateway never call it.
func ReadAndResetCount(ctx context.Context, rdb redis.Scripter, credentialID string) (int64, error) {
	n, err := luaReadAndResetCount.Run(ctx, rdb, []string{credstate.StatsKey(credentialID)}).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		return 0, err
	}
	return n, nil
}
