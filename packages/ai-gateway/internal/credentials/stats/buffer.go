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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/credstate"
)

// ThresholdsResolver returns the effective Thresholds for a credential.
// Production wiring resolves: per-credential override (from Credential
// cache) merged on top of Hub-shadow globals merged on top of
// credstate.DefaultThresholds. The Buffer calls the resolver synchronously
// inside RecordAttempt — implementations must be cheap (cache hit, no DB).
type ThresholdsResolver func(credentialID string) credstate.Thresholds

// Metrics owns the Buffer's Prometheus collectors. Names follow the
// nexus_ai_gateway namespace convention. All collectors are no-op
// callable when Metrics is nil so the package stays usable in tests
// without a registry.
type Metrics struct {
	attemptsTotal      *prometheus.CounterVec
	circuitTransitions *prometheus.CounterVec
	authFailIncrements prometheus.Counter
	redisWriteFailures *prometheus.CounterVec
	redisWriteLatencyS prometheus.Histogram
}

// NewMetrics registers the Buffer's collectors on reg. Pass nil to
// disable metrics collection — callers without a registry (tests,
// short-lived tools) can still construct a fully-functional Buffer.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	f := promauto.With(reg)
	return &Metrics{
		attemptsTotal: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_ai_gateway",
			Subsystem: "credstats",
			Name:      "attempts_total",
			Help:      "Number of upstream attempts recorded by the credential stats buffer, labelled by HTTP class.",
		}, []string{"class"}),
		circuitTransitions: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_ai_gateway",
			Subsystem: "credstats",
			Name:      "circuit_transitions_total",
			Help:      "Circuit-breaker state transitions observed by the buffer.",
		}, []string{"to", "reason"}),
		authFailIncrements: f.NewCounter(prometheus.CounterOpts{
			Namespace: "nexus_ai_gateway",
			Subsystem: "credstats",
			Name:      "auth_fail_increments_total",
			Help:      "Number of 401/403 responses that incremented the live auth_fails counter without crossing the open threshold.",
		}),
		redisWriteFailures: f.NewCounterVec(prometheus.CounterOpts{
			Namespace: "nexus_ai_gateway",
			Subsystem: "credstats",
			Name:      "redis_write_failures_total",
			Help:      "Buffer Redis writes that returned an error, labelled by stage.",
		}, []string{"stage"}),
		redisWriteLatencyS: f.NewHistogram(prometheus.HistogramOpts{
			Namespace: "nexus_ai_gateway",
			Subsystem: "credstats",
			Name:      "redis_write_seconds",
			Help:      "End-to-end time spent in the Buffer's Redis pipelines.",
			Buckets:   []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.5},
		}),
	}
}

func (m *Metrics) incAttempt(class string) {
	if m != nil {
		m.attemptsTotal.WithLabelValues(class).Inc()
	}
}

func (m *Metrics) incCircuit(to, reason string) {
	if m != nil {
		m.circuitTransitions.WithLabelValues(to, reason).Inc()
	}
}

func (m *Metrics) incAuthFailIncrement() {
	if m != nil {
		m.authFailIncrements.Inc()
	}
}

func (m *Metrics) incRedisFailure(stage string) {
	if m != nil {
		m.redisWriteFailures.WithLabelValues(stage).Inc()
	}
}

func (m *Metrics) observeWrite(d time.Duration) {
	if m != nil {
		m.redisWriteLatencyS.Observe(d.Seconds())
	}
}

// Buffer is a non-blocking Redis writer for per-credential attempt stats
// and circuit-breaker transitions. A nil Buffer is safe to use — all
// methods are no-ops. Per-credential thresholds are resolved synchronously
// inside RecordAttempt by the injected ThresholdsResolver.
type Buffer struct {
	rdb      redis.Cmdable
	logger   *slog.Logger
	resolver ThresholdsResolver
	metrics  *Metrics
}

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
	if b == nil || b.rdb == nil || credentialID == "" {
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

	// --- Stats pipeline (always runs) ---
	pipe := b.rdb.Pipeline()
	pipe.HIncrBy(ctx, statsKey, credstate.StatsFieldCount, 1)
	pipe.HSet(ctx, statsKey, credstate.StatsFieldUsedAt, nowStr)
	switch {
	case success:
		pipe.HSet(ctx, statsKey, credstate.StatsFieldOkAt, nowStr)
	case authFail:
		pipe.HSet(ctx, statsKey, credstate.StatsFieldFailAt, nowStr)
		if errMsg != "" {
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
		// On success: if currently OPEN/HALF_OPEN, DEL the hash (close)
		// and mark dirty. Otherwise just reset auth_fails (no transition).
		state, err := b.rdb.HGet(ctx, circuitKey, credstate.CircuitFieldState).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			b.warn("circuit state read failed", credentialID, err)
			b.metrics.incRedisFailure("circuit_read")
			return
		}
		if state == credstate.CircuitHalfOpen || state == credstate.CircuitOpen {
			pipe := b.rdb.Pipeline()
			pipe.Del(ctx, circuitKey)
			pipe.SAdd(ctx, credstate.CircuitDirtySet, credentialID)
			if _, err := pipe.Exec(ctx); err != nil {
				b.warn("circuit close failed", credentialID, err)
				b.metrics.incRedisFailure("circuit_close")
				return
			}
			b.metrics.incCircuit(credstate.CircuitClosed, "")
			return
		}
		// No transition; reset the running auth_fails counter.
		if err := b.rdb.HSet(ctx, circuitKey, credstate.CircuitFieldAuthFails, 0).Err(); err != nil && !errors.Is(err, redis.Nil) {
			b.warn("circuit auth-fail reset failed", credentialID, err)
			b.metrics.incRedisFailure("circuit_reset")
		}
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
