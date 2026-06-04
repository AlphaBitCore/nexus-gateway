// Package cache: semantic_feedback.go — negative-feedback channel for
// the L2 semantic cache.
//
// POST /api/admin/cache/semantic-feedback
//
//	Body:   {"entryKey": "<key>", "vkScope": "<scope>", "reason": "<text>"}
//	Auth:   IAM admin:semantic-cache.update
//	Effect: Adds the (entryKey, vkScope) pair to the Redis poison list so
//	        Reader.Read treats future FT.SEARCH hits against that entry as
//	        misses. Emits an admin audit row.
//
// GET /api/admin/cache/semantic-feedback?limit=100
//
//	Auth:   IAM admin:semantic-cache.read
//	Effect: Lists recent feedback entries from the in-process ring buffer
//	        (capped at 1000). Suitable for the Traffic Audit Drawer "recent
//	        feedback" panel — no DB query required.

package cache

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"

	cpaudit "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
)

// Narrow interface for poison list

// PoisonAdder is the narrow seam the feedback handler uses to write to the
// Redis-backed poison list. The ai-gateway semantic.RedisPoisonList satisfies
// this interface; tests can inject a simple in-memory double.
//
// TTL is the remaining TTL of the cache entry (PTTL result from ai-gateway);
// when 0, Add uses a 24-hour default before the 10× multiplier.
type PoisonAdder interface {
	Add(ctx context.Context, entryKey, vkScope string, ttl time.Duration) error
}

// In-process ring buffer for recent feedback

const maxFeedbackHistory = 1000

// FeedbackEntry is a single admin negative-feedback record stored in the
// ring buffer (not persisted to DB — this is a lightweight operational log).
type FeedbackEntry struct {
	EntryKey  string    `json:"entryKey"`
	VKScope   string    `json:"vkScope"`
	Reason    string    `json:"reason"`
	ActorID   string    `json:"actorId"`
	CreatedAt time.Time `json:"createdAt"`
}

// feedbackRing is a process-scoped ring buffer of recent feedback entries.
// Shared across handler instances (handler is a singleton per process).
var feedbackRing struct {
	mu      sync.RWMutex
	entries []FeedbackEntry
}

func recordFeedback(e FeedbackEntry) {
	feedbackRing.mu.Lock()
	defer feedbackRing.mu.Unlock()
	feedbackRing.entries = append(feedbackRing.entries, e)
	if len(feedbackRing.entries) > maxFeedbackHistory {
		feedbackRing.entries = feedbackRing.entries[len(feedbackRing.entries)-maxFeedbackHistory:]
	}
}

func recentFeedback(limit int) []FeedbackEntry {
	feedbackRing.mu.RLock()
	defer feedbackRing.mu.RUnlock()
	all := feedbackRing.entries
	if limit <= 0 || limit >= len(all) {
		out := make([]FeedbackEntry, len(all))
		copy(out, all)
		return out
	}
	out := make([]FeedbackEntry, limit)
	copy(out, all[len(all)-limit:])
	return out
}

// Request / response shapes

// semanticFeedbackRequest is the JSON body for POST /api/admin/cache/semantic-feedback.
type semanticFeedbackRequest struct {
	EntryKey string `json:"entryKey"`
	VKScope  string `json:"vkScope"`
	Reason   string `json:"reason"`
	// TTL is the remaining TTL of the cache entry in seconds (0 = use default).
	// The UI obtains this from the traffic_event detail row's cache metadata.
	TTLSeconds int64 `json:"ttlSeconds,omitempty"`
}

// Handler methods

// RegisterSemanticFeedbackRoutes mounts the two feedback endpoints under the
// caller-supplied admin group. iamMW gates each route.
func (h *SemanticCacheHandler) RegisterSemanticFeedbackRoutes(
	g *echo.Group,
	iamMW func(action string) echo.MiddlewareFunc,
) {
	g.POST("/cache/semantic-feedback", h.PostSemanticFeedback,
		iamMW(iam.ResourceSemanticCache.Action(iam.VerbUpdate)))
	g.GET("/cache/semantic-feedback", h.GetSemanticFeedback,
		iamMW(iam.ResourceSemanticCache.Action(iam.VerbRead)))
}

// PostSemanticFeedback handles POST /api/admin/cache/semantic-feedback.
// It validates the body, calls PoisonAdder.Add to mark the entry, records the
// feedback in the ring buffer, and emits an admin audit row.
//
// The TTL sent in the request body determines the poison marker lifetime:
// poison TTL = min(ttlSeconds * 10, 30 days). A zero or absent ttlSeconds
// causes Add to use its built-in 24 h × 10 default.
func (h *SemanticCacheHandler) PostSemanticFeedback(c echo.Context) error {
	var req semanticFeedbackRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "malformed_json", "detail": err.Error(),
		})
	}
	if req.EntryKey == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "entryKey is required",
		})
	}
	if len(req.Reason) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "reason is required",
		})
	}

	// Resolve the poison adder from the handler's optional field.
	// If not wired (no Redis client available), return 503 with a clear message.
	if h.poison == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": "poison list not available: Redis client not configured on Control Plane",
		})
	}

	entryTTL := time.Duration(req.TTLSeconds) * time.Second

	if err := h.poison.Add(c.Request().Context(), req.EntryKey, req.VKScope, entryTTL); err != nil {
		h.logger.Error("semantic-feedback: poison Add failed",
			"entryKey", req.EntryKey, "vkScope", req.VKScope, "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": fmt.Sprintf("failed to record feedback: %v", err),
		})
	}

	actor := actorFromContext(c)
	fe := FeedbackEntry{
		EntryKey:  req.EntryKey,
		VKScope:   req.VKScope,
		Reason:    req.Reason,
		ActorID:   actor.UserID,
		CreatedAt: time.Now().UTC(),
	}
	recordFeedback(fe)

	// Emit admin audit row.
	if h.audit != nil {
		e := cpaudit.EntryFor(c, iam.ResourceSemanticCache, iam.VerbUpdate)
		e.EntityID = req.EntryKey
		e.AfterState = map[string]any{
			"action":   "poisoned",
			"vkScope":  req.VKScope,
			"reason":   req.Reason,
			"entryKey": req.EntryKey,
		}
		h.audit.LogObserved(c.Request().Context(), e)
	}

	return c.JSON(http.StatusOK, map[string]any{
		"status":    "poisoned",
		"entryKey":  req.EntryKey,
		"vkScope":   req.VKScope,
		"createdAt": fe.CreatedAt,
	})
}

// GetSemanticFeedback handles GET /api/admin/cache/semantic-feedback?limit=N.
// Returns the most recent `limit` feedback entries from the ring buffer.
// Default limit = 100; maximum = 1000 (capped by ring buffer size).
func (h *SemanticCacheHandler) GetSemanticFeedback(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxFeedbackHistory {
		limit = maxFeedbackHistory
	}

	entries := recentFeedback(limit)
	if entries == nil {
		entries = []FeedbackEntry{}
	}
	return c.JSON(http.StatusOK, map[string]any{
		"entries": entries,
		"total":   len(entries),
	})
}

// Redis-backed PoisonAdder

const (
	cpPoisonKeyPrefix     = "nexus:l2:poison:"
	cpPoisonTTLMultiplier = 10
	cpPoisonMaxTTL        = 30 * 24 * time.Hour
)

// redisPoisonAdder is the control-plane Redis-backed implementation of
// PoisonAdder. It matches the ai-gateway's RedisPoisonList key scheme so both
// services share the same Redis namespace.
type redisPoisonAdder struct {
	rdb redis.UniversalClient
}

// NewRedisPoisonAdder constructs a PoisonAdder backed by the given Redis client.
// rdb must not be nil.
func NewRedisPoisonAdder(rdb redis.UniversalClient) PoisonAdder {
	return &redisPoisonAdder{rdb: rdb}
}

func (p *redisPoisonAdder) Add(ctx context.Context, entryKey, vkScope string, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	poisonTTL := ttl * cpPoisonTTLMultiplier
	if poisonTTL > cpPoisonMaxTTL {
		poisonTTL = cpPoisonMaxTTL
	}
	k := fmt.Sprintf("%s%s:%s", cpPoisonKeyPrefix, vkScope, entryKey)
	return p.rdb.Set(ctx, k, "1", poisonTTL).Err()
}
