// Package cache: semantic_prewarm.go — admin API for FAQ pre-warm of the
// L2 semantic cache from a corpus batch.
//
// POST /api/admin/semantic-cache/prewarm
//
// IAM: admin:semantic-cache.update
//
// The handler validates the request, checks that the semantic cache is
// enabled, and forwards the corpus batch to the AI Gateway's internal
// endpoint POST /internal/semantic-prewarm for embedding + Valkey HSET.
// All hot-path embedding+write logic lives inside the AI Gateway where
// the live ConfigCache, singleflight, and Valkey client are available.
//
// Rate limit: ≤500 entries per POST call. For larger corpora the admin
// pipelines multiple calls.
//
// dryRun=true → the CP forwards the flag to the AI GW; the AI GW embeds
// but skips HSET and returns planned writes. The CP echoes the response verbatim.

package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// prewarmMaxEntries is the maximum number of entries accepted per call.
const prewarmMaxEntries = 500

// prewarmMinTTLSeconds and prewarmMaxTTLSeconds bound the per-entry TTL.
// prewarmDefaultTTLSeconds is applied when the caller omits ttlSeconds
// (i.e. the field arrives as the Go zero value 0).
// prewarmCorpusScope is the reserved, non-VK vk_scope tag every prewarmed entry
// is forced to. The live L2 read resolves a lookup's scope to
// "vk:<id>" (or "" under varyBy=none) — never "corpus" — so a prewarmed entry
// can never be returned under a real VK's scope. This makes prewarm a shared,
// non-targetable corpus rather than a primitive for planting attacker-chosen
// responses into a victim VK's cache lane. (A future opt-in that lets a VK
// consult the corpus is a feature, not a security regression.)
const prewarmCorpusScope = "corpus"

const (
	prewarmMinTTLSeconds     = 60
	prewarmMaxTTLSeconds     = 7 * 86400 // 7 days
	prewarmDefaultTTLSeconds = 86400     // 1 day — matches the TS service docstring
)

// prewarmEntryInput is one Q→A corpus entry from the admin request body.
type prewarmEntryInput struct {
	// Prompt is the question/prompt text to embed and index.
	Prompt string `json:"prompt"`
	// Response is the pre-crafted answer stored as the cache payload.
	Response string `json:"response"`
	// Model is the upstream model name stored in the entry tag (e.g. "gpt-4o").
	// May be empty — stored as an empty tag.
	Model string `json:"model"`
	// VKScope is IGNORED on input and server-forced to the reserved corpus
	// scope — a prewarm caller may not target a virtual key's cache
	// lane. Retained in the struct only so an old client that still sends the
	// field decodes cleanly; PrewarmCache overwrites it before forwarding.
	VKScope string `json:"vkScope"`
	// TTLSeconds is the cache entry TTL in seconds. Must be in [60, 604800].
	// Optional — when omitted (Go zero value 0) the handler substitutes
	// prewarmDefaultTTLSeconds (86400, 1 day) before validation.
	TTLSeconds int `json:"ttlSeconds"`
}

// prewarmRequest is the POST body for
// POST /api/admin/semantic-cache/prewarm.
type prewarmRequest struct {
	// Entries is the corpus batch. ≤500 entries per call. Rate-limit note:
	// for corpora > 500 entries the admin pipelines multiple calls.
	Entries []prewarmEntryInput `json:"entries"`
	// DryRun when true embeds but skips the Valkey HSET write. Returns
	// planned writes and cost estimates without mutating cache state.
	DryRun bool `json:"dryRun"`
}

// PrewarmCache is the handler for POST /api/admin/semantic-cache/prewarm.
//
// Validation:
//   - entries ≤500: 413 Request Entity Too Large
//   - prompt/response non-empty for every entry: 400
//   - ttlSeconds ∈ [60, 604800] (default 86400 when omitted): 400 on out-of-range
//
// Error cases:
//   - L2 disabled (semantic cache not enabled): 503 with body explaining cause
//   - AI GW unreachable: 502
//   - Embedding error on one entry: continues; partial-success included in response
//
// The response mirrors the AI GW SemanticPrewarmResponse shape verbatim.
func (h *SemanticCacheHandler) PrewarmCache(c echo.Context) error {
	var req prewarmRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "malformed_json", "detail": err.Error(),
		})
	}

	// Validate entry count.
	if len(req.Entries) == 0 {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "entries must not be empty",
		})
	}
	if len(req.Entries) > prewarmMaxEntries {
		return c.JSON(http.StatusRequestEntityTooLarge, map[string]any{
			"error": fmt.Sprintf("entries exceeds maximum of %d per call", prewarmMaxEntries),
			"code":  "entries_too_many",
		})
	}

	// Validate each entry. Apply default TTL when omitted (Go zero value).
	for i := range req.Entries {
		e := &req.Entries[i]
		if strings.TrimSpace(e.Prompt) == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("entries[%d].prompt must not be empty", i),
			})
		}
		if strings.TrimSpace(e.Response) == "" {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf("entries[%d].response must not be empty", i),
			})
		}
		if e.TTLSeconds == 0 {
			e.TTLSeconds = prewarmDefaultTTLSeconds
		}
		if e.TTLSeconds < prewarmMinTTLSeconds || e.TTLSeconds > prewarmMaxTTLSeconds {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": fmt.Sprintf(
					"entries[%d].ttlSeconds must be in [%d, %d], got %d",
					i, prewarmMinTTLSeconds, prewarmMaxTTLSeconds, e.TTLSeconds,
				),
			})
		}
		// A prewarm caller MUST NOT choose the vk_scope. Letting the
		// caller tag an entry with a victim VK's scope plants attacker-chosen
		// content into that VK's cache lane (cross-VK poisoning), since the read
		// path's only isolation filter is @vk_scope. Force every entry to the
		// reserved, non-VK corpus scope so it can never be served under a real
		// VK's scope. Any caller-supplied scope is dropped; we log it once for
		// visibility rather than silently swallowing an attempted target.
		if e.VKScope != "" && e.VKScope != prewarmCorpusScope {
			h.logger.Warn("semantic prewarm: dropping caller-supplied vkScope (forced to corpus)",
				"suppliedScope", e.VKScope, "entryIndex", i)
		}
		e.VKScope = prewarmCorpusScope
	}

	// Check if semantic cache is enabled by reading the singleton config row.
	// This is a best-effort check: the AI GW's live ConfigCache is the
	// authoritative gate, but giving the admin a clear 503 here avoids a
	// roundtrip to the AI GW when the config is obviously disabled.
	if h.store != nil {
		row, err := h.store.Get(c.Request().Context())
		if err == nil && row != nil && !row.Enabled {
			return c.JSON(http.StatusServiceUnavailable, map[string]any{
				"error": "semantic cache is disabled; enable it on the Cache Embedding settings page before pre-warming",
				"code":  "semantic_cache_disabled",
			})
		}
		// On store error (DB unavailable) we proceed — the AI GW will
		// perform the authoritative check via its live ConfigCache.
	}

	// Guard: AI Gateway URL must be configured.
	if h.aiGatewayURL == "" {
		return c.JSON(http.StatusServiceUnavailable, map[string]any{
			"error": "AI Gateway URL not configured; cannot forward prewarm request",
			"code":  "gateway_unavailable",
		})
	}

	// Forward to AI Gateway internal endpoint.
	gwURL := strings.TrimRight(h.aiGatewayURL, "/") + "/internal/semantic-prewarm"

	// Build the forwarded request body. We forward the entries array and
	// the dryRun flag. Credentials are not forwarded — the AI GW resolves
	// them per-call from its live ConfigCache snapshot (Hub-pushed
	// embedding provider URL) + CredManager (decrypted API key for the
	// snapshot's EmbeddingProviderID). This mirrors the hot-path
	// resolution in packages/ai-gateway/internal/ingress/proxy/proxy_l2.go
	// `tryL2Lookup` so prewarm and the live L2 read/write paths use the
	// same credential surface.
	fwdBody, err := json.Marshal(map[string]any{
		"entries": req.Entries,
		"dryRun":  req.DryRun,
	})
	if err != nil {
		h.logger.Error("semantic prewarm: marshal forward body", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "internal error marshalling request",
		})
	}

	client := nexushttp.New(nexushttp.Config{
		Timeout:        120 * time.Second, // 500 entries × ~200ms embed = up to 100s
		Caller:         "cp-semantic-prewarm",
		PropagateReqID: true,
	})
	fwdReq, err := http.NewRequestWithContext(
		c.Request().Context(),
		http.MethodPost,
		gwURL,
		bytes.NewReader(fwdBody),
	)
	if err != nil {
		h.logger.Error("semantic prewarm: build gateway request", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "failed to build gateway request: " + err.Error(),
		})
	}
	fwdReq.Header.Set("Content-Type", "application/json")
	fwdReq.Header.Set("Authorization", "Bearer "+h.aiGatewayToken)

	resp, err := client.Do(fwdReq)
	if err != nil {
		h.logger.Warn("semantic prewarm: AI Gateway unreachable", "url", gwURL, "error", err)
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error": "AI Gateway unreachable: " + err.Error(),
			"code":  "gateway_unreachable",
		})
	}
	defer resp.Body.Close() //nolint:errcheck

	// AI GW returns 503 when the semantic cache is disabled at the gateway.
	if resp.StatusCode == http.StatusServiceUnavailable {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return c.JSON(http.StatusServiceUnavailable, json.RawMessage(body))
	}

	// Forward the AI GW response verbatim for all 2xx and other status codes.
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	c.Response().Header().Set("Content-Type", "application/json")
	return c.JSON(resp.StatusCode, json.RawMessage(bodyBytes))
}
