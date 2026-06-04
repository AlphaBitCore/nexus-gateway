package geminicache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// KeyResolver resolves an API key and base URL for a given (providerID, modelID)
// pair. The production implementation wraps provtarget.PgResolver. Defined here
// as an interface so the geminicache package does not depend on provtarget.
type KeyResolver interface {
	Resolve(ctx context.Context, providerID, modelID string) (apiKey, baseURL string, err error)
}

// InjectResult describes the outcome of a single Inject call.
type InjectResult struct {
	// Injected is true when the body was rewritten to use a cachedContent.
	Injected bool
	// CachedContentName is the "cachedContents/xxx" name used on a hit.
	CachedContentName string
	// Invalidate, when non-nil, removes the Redis entry that produced this
	// hit. The proxy is expected to call it when the upstream returns
	// HTTP 403 with "CachedContent not found (or permission denied)" —
	// that signals the Gemini-side cache was evicted while Redis still
	// pointed at it. Calling Invalidate ensures the next request
	// regenerates instead of looping on the stale ref. Safe to call
	// from any goroutine; nil on miss.
	Invalidate func()
}

// Manager is the Gemini cachedContent lifecycle manager.
// Inject is safe for concurrent use.
type Manager struct {
	cfg     *configHolder
	rdb     redis.UniversalClient
	api     *apiClient
	res     KeyResolver
	metrics *Metrics
	logger  *slog.Logger

	// circuit breaker state (updated atomically)
	cbFailures  atomic.Int64
	cbOpenUntil atomic.Int64 // Unix nanoseconds; 0 = closed
}

// New constructs a Manager. rdb may be nil; when nil all Redis operations are
// skipped and the manager behaves as if every lookup is a miss (no body
// rewrite, async creates are still attempted if KeyResolver is non-nil but
// the result cannot be cached). Passing a nil KeyResolver disables async
// cache creation.
func New(rdb redis.UniversalClient, res KeyResolver, metrics *Metrics, cfg Config, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	if metrics == nil {
		metrics = NewMetrics(nil)
	}
	return &Manager{
		cfg:     newConfigHolder(cfg),
		rdb:     rdb,
		api:     newAPIClient(),
		res:     res,
		metrics: metrics,
		logger:  logger,
	}
}

// Reload hot-swaps the configuration. Safe to call concurrently.
func (m *Manager) Reload(cfg Config) {
	m.cfg.set(cfg)
	m.logger.Info("geminicache config reloaded", "enabled", cfg.Enabled, "min_system_chars", cfg.MinSystemChars)
}

// Inject inspects the Gemini-format body for a large systemInstruction and
// either rewrites the body to reference a cached content object (on a Redis
// hit) or fires an async goroutine to create one (on a miss).
//
// Parameters:
//   - providerID: the provider UUID (used in the content hash).
//   - modelID: the provider model ID, e.g. "gemini-2.0-flash" (no "models/" prefix).
//   - body: the Gemini-wire body produced by spec_gemini codec.EncodeRequest.
//
// Returns the (possibly rewritten) body, an InjectResult, and a non-nil error
// only on internal failures that should be logged. The caller must always use
// the returned body regardless of error; the function is fail-open.
func (m *Manager) Inject(ctx context.Context, providerID, modelID string, body []byte) ([]byte, InjectResult, error) {
	cfg := m.cfg.get()
	if !cfg.Enabled {
		m.metrics.recordSkipped("disabled")
		return body, InjectResult{}, nil
	}

	// Extract the systemInstruction block from the Gemini body.
	sysInstr := gjson.GetBytes(body, "systemInstruction")
	if !sysInstr.Exists() || sysInstr.Raw == "" {
		m.metrics.recordSkipped("no_system")
		return body, InjectResult{}, nil
	}
	systemJSON := sysInstr.Raw

	if len(systemJSON) < cfg.minSystemChars() {
		m.metrics.recordSkipped("below_threshold")
		return body, InjectResult{}, nil
	}

	// Gemini rejects a request that references a cachedContent AND also sets
	// tools / toolConfig — they must live INSIDE the cache. Capture them so they
	// are folded into the cachedContent at create time, keyed into the hash (so a
	// hit guarantees the cached tools match this request), and stripped from the
	// wire on a hit. Empty when the caller sends no tools (the hash/body are then
	// unchanged from the system-only form, preserving existing cache entries).
	toolsJSON := rawIfPresent(body, "tools")
	toolConfigJSON := rawIfPresent(body, "toolConfig")

	rk := contentHash(providerID, modelID, systemJSON, toolsJSON, toolConfigJSON)

	// Redis lookup.
	if m.rdb != nil {
		val, err := m.rdb.Get(ctx, rk).Result()
		if err == nil {
			// Cache HIT: rewrite body.
			var rec cachedRecord
			if jsonErr := json.Unmarshal([]byte(val), &rec); jsonErr != nil {
				m.logger.Warn("geminicache: corrupt Redis record, treating as miss",
					"key", rk, "error", jsonErr)
			} else if rec.Name != "" {
				rewritten, rewriteErr := rewriteBody(body, rec.Name)
				if rewriteErr != nil {
					m.logger.Warn("geminicache: body rewrite failed, pass-through",
						"error", rewriteErr)
					m.metrics.recordMiss(modelID)
					return body, InjectResult{}, nil
				}
				m.metrics.recordHit(modelID)
				// Closure captures the redis key + provider/model context
				// so the caller can invalidate without re-deriving the key.
				// Detached background context — proxy invokes from a request
				// scope that may already be cancelled by the time we run.
				invalidate := func() {
					if m.rdb == nil {
						return
					}
					bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if delErr := m.rdb.Del(bgCtx, rk).Err(); delErr != nil {
						m.logger.Warn("geminicache: stale-ref invalidation failed",
							"key", rk, "name", rec.Name, "error", delErr)
						return
					}
					m.metrics.recordSkipped("invalidated_stale_ref")
					m.logger.Info("geminicache: invalidated stale Redis entry",
						"key", rk, "name", rec.Name, "provider", providerID, "model", modelID)
				}
				return rewritten, InjectResult{
					Injected:          true,
					CachedContentName: rec.Name,
					Invalidate:        invalidate,
				}, nil
			}
		} else if !errors.Is(err, redis.Nil) {
			// Redis error (not a miss) — log and fall through.
			m.logger.Warn("geminicache: Redis GET error, treating as miss",
				"key", rk, "error", err)
		}
	}

	// Cache MISS: fire async creation.
	m.metrics.recordMiss(modelID)
	m.asyncCreate(providerID, modelID, systemJSON, toolsJSON, toolConfigJSON, rk, cfg)
	return body, InjectResult{}, nil
}

// rawIfPresent returns the raw JSON of body[path] when it exists and is
// non-empty, else "". Used to fold optional tools / toolConfig blocks into the
// cache key + create payload without changing behaviour when they are absent.
func rawIfPresent(body []byte, path string) string {
	if r := gjson.GetBytes(body, path); r.Exists() && r.Raw != "" {
		return r.Raw
	}
	return ""
}

// asyncCreate fires a background goroutine that calls the Gemini cachedContents
// API and stores the result in Redis. Respects the circuit breaker. toolsJSON /
// toolConfigJSON are the raw tool blocks to fold into the cachedContent (empty
// when the request carries none).
func (m *Manager) asyncCreate(providerID, modelID, systemJSON, toolsJSON, toolConfigJSON, redisKey string, cfg Config) {
	// Circuit breaker — open window check.
	if openUntil := m.cbOpenUntil.Load(); openUntil > 0 && time.Now().UnixNano() < openUntil {
		m.metrics.recordSkipped("circuit_open")
		return
	}
	if m.res == nil {
		m.metrics.recordSkipped("no_resolver")
		return
	}

	go func() {
		// Use a generous background timeout so a slow Gemini API does not leak
		// goroutines indefinitely.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		apiKey, baseURL, err := m.res.Resolve(ctx, providerID, modelID)
		if err != nil {
			m.logger.Warn("geminicache: resolve apiKey failed",
				"provider_id", providerID, "model", modelID, "error", err)
			m.recordFailure(cfg)
			m.metrics.recordCreateErr(modelID)
			return
		}
		if baseURL == "" {
			baseURL = "https://generativelanguage.googleapis.com"
		}

		rec, err := m.api.create(ctx, apiKey, baseURL, modelID, systemJSON, toolsJSON, toolConfigJSON, cfg.ttlSeconds())
		if err != nil {
			m.logger.Warn("geminicache: create cachedContent failed",
				"provider_id", providerID, "model", modelID, "error", err)
			m.recordFailure(cfg)
			m.metrics.recordCreateErr(modelID)
			return
		}

		// Store in Redis with TTL strictly SHORTER than the Gemini
		// cachedContent TTL. The previous logic added a 5-minute grace
		// (TTLSeconds + 300) which meant Redis could vend stale names
		// after Gemini had already evicted them — clients got HTTP 403
		// "CachedContent not found (or permission denied)". The 5-min
		// safety margin in the other direction (TTLSeconds - 300)
		// keeps the Redis entry usable for the bulk of the cache's
		// lifetime while leaving a buffer for Gemini's best-effort
		// eviction tolerance. Floor at 60s so a misconfigured tiny
		// TTL still gets some reuse.
		if m.rdb != nil {
			raw, _ := json.Marshal(rec)
			redisTTLSecs := cfg.ttlSeconds() - 300
			if redisTTLSecs < 60 {
				redisTTLSecs = 60
			}
			redisTTL := time.Duration(redisTTLSecs) * time.Second
			if setErr := m.rdb.Set(ctx, redisKey, raw, redisTTL).Err(); setErr != nil {
				m.logger.Warn("geminicache: Redis SET failed",
					"key", redisKey, "error", setErr)
				// Still count as OK since the Gemini object was created.
			}
		}

		m.resetCircuitBreaker()
		m.metrics.recordCreateOK(modelID)
		m.logger.Info("geminicache: cachedContent created",
			"name", rec.Name, "model", modelID, "token_count", rec.TokenCount)
	}()
}

func (m *Manager) recordFailure(cfg Config) {
	failures := m.cbFailures.Add(1)
	if int(failures) >= cfg.cbThreshold() {
		openUntil := time.Now().Add(time.Duration(cfg.cbOpenSecs()) * time.Second).UnixNano()
		m.cbOpenUntil.Store(openUntil)
		m.cbFailures.Store(0)
		m.logger.Warn("geminicache: circuit breaker opened",
			"open_secs", cfg.cbOpenSecs())
	}
}

func (m *Manager) resetCircuitBreaker() {
	m.cbFailures.Store(0)
	m.cbOpenUntil.Store(0)
}

// rewriteBody rewrites a Gemini body to reference a cachedContent. It removes
// the fields Gemini forbids alongside a cachedContent reference — systemInstruction,
// tools, and toolConfig — and sets cachedContent. All three were folded into the
// cache at create time and are part of the cache key, so a hit guarantees the
// cached copies match this request; deleting a field that is absent is a no-op.
// Without stripping tools/toolConfig, Gemini returns 400 "CachedContent can not be
// used with GenerateContent request setting system_instruction, tools or
// tool_config" for every tool-calling request (e.g. the operator agent).
func rewriteBody(body []byte, cachedContentName string) ([]byte, error) {
	out := body
	for _, field := range []string{"systemInstruction", "tools", "toolConfig"} {
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("delete %s: %w", field, err)
		}
	}
	out, err := sjson.SetBytes(out, "cachedContent", cachedContentName)
	if err != nil {
		return nil, fmt.Errorf("set cachedContent: %w", err)
	}
	return out, nil
}
