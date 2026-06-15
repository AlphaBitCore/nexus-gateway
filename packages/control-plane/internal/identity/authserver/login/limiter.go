package login

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Default limiter policy. Matches the old handler's protection budget: ten
// attempts per (ip, email) pair inside a five-minute sliding window is tight
// enough to blunt online password-guessing while leaving room for legitimate
// retries on typos and MFA confusion.
const (
	defaultLimiterWindow = 5 * time.Minute
	defaultLimiterBudget = 10
	// perIPMultiplier sets the per-IP global cap relative to the per-pair
	// budget. Without it, one IP spraying a single password across N distinct
	// emails opens N independent (ip,email) buckets and faces effectively no
	// total ceiling. At the defaults the global cap is 10*5 = 50
	// attempts / 5 min — generous enough for a shared NAT egress yet bounding a
	// single source to five accounts' worth of full budget before it is locked
	// out across ALL emails.
	perIPMultiplier = 5

	// redisKeyPrefix namespaces the login limiter's Redis keys. It sits under
	// the repo-wide rate-limit namespace ("nexus:rl:") used by the AI Gateway
	// limiter (packages/ai-gateway/internal/policy/ratelimit) so all rate-limit
	// state is greppable / flushable under one prefix.
	redisKeyPrefix = "nexus:rl:login:"
	// redisOpTimeout bounds every Redis call so a cache outage degrades to the
	// local limiter in well under a second rather than stalling the login
	// request on the client's default dial/read timeout. Mirrors the 500ms
	// bound used by the IAM cache and the assistant owner registry.
	redisOpTimeout = 500 * time.Millisecond
)

// loginLimiterScript is an atomic two-budget sliding-window check executed as a
// single EVALSHA (Run falls back to EVAL on NOSCRIPT). It enforces the per-pair
// and per-IP budgets together against shared Redis state, so the budget holds
// across every Control Plane replica and survives a process restart.
//
//	KEYS[1] = per-(ip,email) sorted set   KEYS[2] = per-ip sorted set
//	ARGV[1] = pair budget   ARGV[2] = per-ip budget
//	ARGV[3] = window (ms)   ARGV[4] = now (ms)   ARGV[5] = unique member
//
// Returns 1 when the attempt is permitted (and records it against BOTH sets),
// 0 when EITHER budget is already exhausted (recording NOTHING — a blocked
// attacker cannot lengthen their own window by retrying, matching the local
// limiter's semantics). PEXPIRE bounds each key to one window so abandoned
// keys self-evict and Redis memory stays bounded.
var loginLimiterScript = redis.NewScript(`
local pairKey = KEYS[1]
local ipKey = KEYS[2]
local pairBudget = tonumber(ARGV[1])
local ipBudget = tonumber(ARGV[2])
local windowMs = tonumber(ARGV[3])
local now = tonumber(ARGV[4])
local member = ARGV[5]
local cutoff = now - windowMs

redis.call('ZREMRANGEBYSCORE', pairKey, '-inf', cutoff)
redis.call('ZREMRANGEBYSCORE', ipKey, '-inf', cutoff)

local pairCount = redis.call('ZCARD', pairKey)
local ipCount = redis.call('ZCARD', ipKey)
if pairCount >= pairBudget or ipCount >= ipBudget then
	return 0
end

redis.call('ZADD', pairKey, now, member)
redis.call('PEXPIRE', pairKey, windowMs)
redis.call('ZADD', ipKey, now, member)
redis.call('PEXPIRE', ipKey, windowMs)
return 1
`)

// Limiter enforces two login-attempt budgets in tandem: a per-(ip, email)
// budget that blunts guessing against one account, and a per-IP global budget
// that bounds a single source spraying one password across many usernames.
//
// Distribution model: when a Redis handle is wired the limiter counts
// in Redis (atomic sliding window via loginLimiterScript), so the budget is
// shared across Control Plane replicas and survives a restart. On ANY Redis
// error — outage, timeout, script failure — it degrades GRACEFULLY to the
// in-memory limiter below: login is never left unprotected (the local
// per-process cap still applies) and never locked out (a cache blip cannot
// fail closed into a login DoS). When Redis recovers, distributed counting
// resumes automatically. With no Redis handle (nil) the limiter is local-only
// — the single-process / pool-less dev path.
//
// The in-memory side is a sliding-window map keyed by (ip,email) and by ip.
// Old timestamps are evicted inline on each Allow call; in addition an
// opportunistic full sweep (once per window) drops keys whose history has
// fully aged out, bounding the local maps to the active working set rather
// than letting a never-revisited key linger forever.
type Limiter struct {
	window      time.Duration
	budget      int
	perIPBudget int

	// rdb is the optional distributed backend. nil = local-only.
	rdb redis.UniversalClient

	mu        sync.Mutex
	history   map[string][]time.Time // keyed by ip + ":" + normalized email
	ipHistory map[string][]time.Time // keyed by ip
	lastSweep time.Time              // last opportunistic full prune of the maps
	now       func() time.Time       // overridden in tests
}

// NewLimiter returns a local-only Limiter with the default policy (10 attempts
// / 5 minutes per (ip, email) pair; 50 attempts / 5 minutes per IP). Use
// NewLimiterWithRedis to make the budget distributed across replicas.
func NewLimiter() *Limiter {
	return newLimiterWith(defaultLimiterWindow, defaultLimiterBudget, time.Now)
}

// NewLimiterWithRedis returns a Limiter with the default policy backed by the
// shared Redis client for cross-replica / restart-durable counting. When rdb
// is nil it is equivalent to NewLimiter (local-only) — callers can pass the
// CP's Redis handle unconditionally and get graceful local fallback when Redis
// is not configured.
func NewLimiterWithRedis(rdb redis.UniversalClient) *Limiter {
	l := newLimiterWith(defaultLimiterWindow, defaultLimiterBudget, time.Now)
	l.rdb = rdb
	return l
}

// newLimiterWith is the internal constructor used by tests to inject a
// deterministic clock and a shorter window. The per-IP budget is derived from
// the per-pair budget via perIPMultiplier.
func newLimiterWith(window time.Duration, budget int, now func() time.Time) *Limiter {
	return newLimiterFull(window, budget, budget*perIPMultiplier, now, nil)
}

// newLimiterFull is the fully-parameterized constructor; it lets tests set the
// per-IP global budget independently of the per-pair budget and inject a Redis
// handle for the distributed path.
func newLimiterFull(window time.Duration, budget, perIPBudget int, now func() time.Time, rdb redis.UniversalClient) *Limiter {
	return &Limiter{
		window:      window,
		budget:      budget,
		perIPBudget: perIPBudget,
		rdb:         rdb,
		history:     make(map[string][]time.Time),
		ipHistory:   make(map[string][]time.Time),
		lastSweep:   now(),
		now:         now,
	}
}

// normalizeEmail lower-cases and trims the email so case/whitespace variants
// share one bucket on both the local and Redis paths.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// Allow reports whether a login attempt for (ip, email) is permitted under both
// the per-pair and per-IP budgets. An attempt is permitted only if NEITHER
// budget is exhausted. Every call that returns true records the attempt;
// calls that return false record nothing, so a blocked attacker cannot
// lengthen their own window by retrying.
//
// When a Redis handle is wired the decision is made against shared Redis state;
// any Redis error falls through to the local in-memory limiter (fail-to-local)
// so login stays protected without ever locking out on a cache blip.
func (l *Limiter) Allow(ip, email string) bool {
	if l.rdb != nil {
		if allowed, ok := l.allowRedis(ip, email); ok {
			return allowed
		}
		// Redis unavailable/erroring: fall through to the local limiter.
	}
	return l.allowLocal(ip, email)
}

// allowRedis runs the atomic two-budget sliding-window script. The second
// return is false when Redis is unreachable or the script errors, signalling
// the caller to fall back to the local limiter.
func (l *Limiter) allowRedis(ip, email string) (allowed bool, ok bool) {
	pairKey := redisKeyPrefix + "pair:" + ip + ":" + normalizeEmail(email)
	ipKey := redisKeyPrefix + "ip:" + ip
	nowMs := l.now().UnixMilli()
	member := strconv.FormatInt(nowMs, 10) + ":" + randomSuffix()

	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()

	res, err := loginLimiterScript.Run(ctx, l.rdb,
		[]string{pairKey, ipKey},
		l.budget, l.perIPBudget, l.window.Milliseconds(), nowMs, member,
	).Int()
	if err != nil {
		return false, false
	}
	return res == 1, true
}

// allowLocal is the in-memory fallback. It enforces the same two budgets over
// process-local sliding windows.
func (l *Limiter) allowLocal(ip, email string) bool {
	pairKey := ip + ":" + normalizeEmail(email)
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()

	l.maybeSweepLocked(now, cutoff)

	pairHist := pruneWindow(l.history[pairKey], cutoff)
	ipHist := pruneWindow(l.ipHistory[ip], cutoff)

	if len(pairHist) >= l.budget || len(ipHist) >= l.perIPBudget {
		// Persist the pruned slices so later calls do not re-scan evicted
		// entries, but do NOT record this blocked attempt.
		l.history[pairKey] = pairHist
		l.ipHistory[ip] = ipHist
		return false
	}

	pairHist = append(pairHist, now)
	ipHist = append(ipHist, now)
	l.history[pairKey] = pairHist
	l.ipHistory[ip] = ipHist
	return true
}

// maybeSweepLocked drops fully-aged-out keys from both maps at most once per
// window. Inline pruning in allowLocal only reclaims a key when that key is
// queried again; a key that is never revisited would otherwise linger forever
// (unbounded map growth). The sweep bounds the maps to the set of
// keys seen within one window — the working set — at O(n) once per window.
// Must be called with l.mu held.
func (l *Limiter) maybeSweepLocked(now, cutoff time.Time) {
	if now.Sub(l.lastSweep) < l.window {
		return
	}
	l.lastSweep = now
	for k, h := range l.history {
		if p := pruneWindow(h, cutoff); len(p) == 0 {
			delete(l.history, k)
		} else {
			l.history[k] = p
		}
	}
	for k, h := range l.ipHistory {
		if p := pruneWindow(h, cutoff); len(p) == 0 {
			delete(l.ipHistory, k)
		} else {
			l.ipHistory[k] = p
		}
	}
}

// pruneWindow returns hist with all timestamps at or before cutoff removed.
// Pruning is in place (reusing hist's backing array) so the caller can store
// the result back without an extra allocation.
func pruneWindow(hist []time.Time, cutoff time.Time) []time.Time {
	pruned := hist[:0]
	for _, ts := range hist {
		if ts.After(cutoff) {
			pruned = append(pruned, ts)
		}
	}
	return pruned
}

// randomSuffix returns a short random hex string used to make a sliding-window
// member unique when multiple attempts share the same millisecond timestamp,
// so concurrent attempts never collide on a single ZSET member.
func randomSuffix() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
