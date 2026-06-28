package debug

// semantic_prewarm_endpoint.go — AI Gateway internal handler for
// POST /internal/semantic-prewarm (FAQ pre-warm L2 from corpus).
//
// Called by the Control Plane admin API handler
// (packages/control-plane/internal/ai/cache/handler/semantic_prewarm.go)
// when an admin imports a Q→A corpus. For each entry the handler:
//  1. Resolves the embedding provider URL + decrypted API key from the
//     live ConfigSnapshot + CredManager — same pattern as the hot path
//     in packages/ai-gateway/internal/ingress/proxy/proxy_l2.go. The CP
//     never forwards credentials; the AI GW already has them in-process.
//  2. Calls Writer.Write (existing singleflight + HSET hot-path).
//  3. Collects per-entry status (written | skipped | error).
//  4. Returns a summary: {written, skipped, errors, embeddingCalls, durationMs}.
//
// Rate limit: ≤500 entries per call (enforced in the CP handler; the
// AI GW handler trusts the CP to enforce this and applies the same cap
// as a defensive guard).
//
// dryRun=true → skip the Writer entirely (returns skipReason="dry_run")
// for every entry.

import (
	"context"
	"github.com/goccy/go-json"
	"log/slog"
	"net/http"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
)

// semanticWriterIface is the narrow interface the prewarm handler uses.
// *semantic.Writer satisfies it in production; test stubs satisfy it in tests.
// Defined here so the handler can be tested without a live Valkey client.
type semanticWriterIface interface {
	Write(ctx context.Context, req semantic.WriteRequest) (semantic.WriteResult, error)
}

// credResolverIface is the narrow seam onto credmanager.Manager used by
// the prewarm endpoint: given a provider ID, return the decrypted API key.
// *credmanager.Manager satisfies this contract (its GetForProvider returns
// (apiKey, credentialID, credentialName, error)).
type credResolverIface interface {
	GetForProvider(ctx context.Context, providerID string) (string, string, string, error)
}

// configSnapshotGetter is the narrow seam onto *semantic.ConfigCache.
// Defined here so test stubs can supply a fixed snapshot without a real
// ConfigCache.
type configSnapshotGetter interface {
	Get() semantic.ConfigSnapshot
}

// maxPrewarmEntriesPerCall is the defensive cap applied by the AI GW handler
// in addition to the CP-side validation.
const maxPrewarmEntriesPerCall = 500

// SemanticPrewarmEntry is one Q→A corpus entry forwarded from the CP handler.
type SemanticPrewarmEntry struct {
	// Prompt is the question/prompt text to embed and index.
	Prompt string `json:"prompt"`
	// Response is the pre-crafted answer stored as the cache payload.
	Response string `json:"response"`
	// Model is the upstream model name stored in the cache entry
	// (e.g. "gpt-4o"). Used for upstream_model tag filtering.
	Model string `json:"model"`
	// VKScope is the virtual-key scope tag (e.g. "v1:vk:nvk_xxx").
	// May be empty — stored as an empty tag so the entry is visible
	// to all scopes unless the lookup filters on vk_scope.
	VKScope string `json:"vkScope"`
	// TTLSeconds is the entry TTL in seconds [60, 604800].
	TTLSeconds int `json:"ttlSeconds"`
}

// SemanticPrewarmRequest is the POST /internal/semantic-prewarm body.
// Credentials are NOT carried on this envelope — the AI GW resolves the
// embedding provider URL + decrypted key from its live ConfigSnapshot +
// CredManager (same path as proxy_l2.go). The CP forwarder intentionally
// strips any caller-supplied credential fields.
type SemanticPrewarmRequest struct {
	// Entries is the corpus batch. ≤500 entries per call.
	Entries []SemanticPrewarmEntry `json:"entries"`
	// DryRun when true skips both the embedding call and the HSET write.
	DryRun bool `json:"dryRun"`
}

// SemanticPrewarmEntryResult is the per-entry outcome.
type SemanticPrewarmEntryResult struct {
	// Index is the 0-based position of this entry in the request slice.
	Index int `json:"index"`
	// Written is true when the entry was successfully embedded and stored.
	Written bool `json:"written,omitempty"`
	// Skipped is true when the entry was skipped (L2 disabled, dim mismatch, etc.)
	// or dry-run mode is active.
	Skipped bool `json:"skipped,omitempty"`
	// SkipReason is the machine-readable skip reason when Skipped=true.
	SkipReason string `json:"skipReason,omitempty"`
	// EmbeddingCostUSD is the embedding cost for this entry in USD.
	EmbeddingCostUSD float64 `json:"embeddingCostUsd,omitempty"`
	// Error is set when the entry failed due to an unexpected error.
	Error string `json:"error,omitempty"`
}

// SemanticPrewarmResponse is the POST /internal/semantic-prewarm response body.
type SemanticPrewarmResponse struct {
	// Written is the number of entries successfully embedded and stored.
	Written int `json:"written"`
	// Skipped is the number of entries skipped (includes dry-run skips).
	Skipped int `json:"skipped"`
	// Errors is the number of entries that encountered an unexpected error.
	Errors int `json:"errors"`
	// EmbeddingCalls is the total number of embedding API calls made.
	// When dryRun=true, this counts the calls that would have been made.
	EmbeddingCalls int `json:"embeddingCalls"`
	// EmbeddingCostUSD is the total embedding cost across all entries in USD.
	EmbeddingCostUSD float64 `json:"embeddingCostUsd"`
	// DurationMs is the total wall-clock time for the handler in milliseconds.
	DurationMs int64 `json:"durationMs"`
	// DryRun echoes the input flag.
	DryRun bool `json:"dryRun"`
	// Entries holds per-entry outcomes (always present; one element per input entry).
	Entries []SemanticPrewarmEntryResult `json:"entries"`
}

// SemanticPrewarmHandler returns an http.HandlerFunc that processes a
// prewarm corpus batch using the live semantic.Writer + live ConfigCache
// + live CredManager. It is called by the Control Plane admin API handler
// and must not be exposed publicly.
//
// The writer's Write method is reused verbatim — no new embedding or
// Valkey code paths are introduced.
//
// writer may be nil when the AI GW was started without a Redis client or
// during tests; in that case the handler returns 503 with
// code="semantic_cache_disabled". cfgCache and creds may also be nil
// (defensive — boot wiring guarantees them in production); the handler
// surfaces them as 503 so tests of the nil-writer path keep working
// without forcing those deps.
func SemanticPrewarmHandler(
	writer *semantic.Writer,
	cfgCache *semantic.ConfigCache,
	creds credResolverIface,
	logger *slog.Logger,
) http.HandlerFunc {
	// Unwrap the concrete pointer to avoid the nil-interface trap: a nil
	// *semantic.Writer assigned to a semanticWriterIface produces a non-nil
	// interface (type != nil, value == nil), which would bypass the nil check
	// inside the handler and cause a nil pointer dereference.
	if writer == nil {
		return semanticPrewarmHandler(nil, nil, nil, logger)
	}
	var snapGetter configSnapshotGetter
	if cfgCache != nil {
		snapGetter = cfgCache
	}
	return semanticPrewarmHandler(writer, snapGetter, creds, logger)
}

// semanticPrewarmHandler is the internal constructor that accepts the narrow
// interfaces so tests can inject stubs without a live Valkey client / live
// credmanager / live ConfigCache.
func semanticPrewarmHandler(
	writer semanticWriterIface,
	cfgCache configSnapshotGetter,
	creds credResolverIface,
	logger *slog.Logger,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		var req SemanticPrewarmRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "invalid request body: " + err.Error(),
			})
			return
		}

		if len(req.Entries) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "entries must not be empty",
			})
			return
		}
		if len(req.Entries) > maxPrewarmEntriesPerCall {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"error": "entries exceeds maximum of 500 per call",
			})
			return
		}

		// When the writer is nil (L2 not wired), return 503 with a
		// machine-readable error so the CP handler can surface it as a
		// proper 503 to the admin UI.
		if writer == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "semantic cache disabled or Redis unavailable; L2 writer not initialised",
				"code":  "semantic_cache_disabled",
			})
			return
		}

		// Resolve the embedding provider URL + decrypted API key once per
		// call from the live ConfigSnapshot + CredManager. This mirrors the
		// canonical hot-path resolution in proxy_l2.go (`tryL2Lookup`).
		// dryRun bypasses this — no upstream call is made.
		var providerBaseURL, apiKey string
		var resolveSkipReason string
		if !req.DryRun {
			providerBaseURL, apiKey, resolveSkipReason = resolveEmbeddingCreds(r.Context(), cfgCache, creds, logger)
		}

		results := make([]SemanticPrewarmEntryResult, len(req.Entries))
		var totalWritten, totalSkipped, totalErrors, totalCalls int
		var totalCostUSD float64

		for i, entry := range req.Entries {
			result := SemanticPrewarmEntryResult{Index: i}

			if req.DryRun {
				// dryRun: validate structure but do not call Write.
				// We still count this as "skipped" with a dry_run reason.
				result.Skipped = true
				result.SkipReason = "dry_run"
				totalSkipped++
				results[i] = result
				continue
			}

			// If credential resolution failed for the batch, stamp every
			// entry with the same observable skipReason — the writer would
			// also skip but with a less-informative provider_error after a
			// 401, whereas this surfaces the real cause (no snapshot yet,
			// no credential for provider, etc.).
			if resolveSkipReason != "" {
				result.Skipped = true
				result.SkipReason = resolveSkipReason
				totalSkipped++
				results[i] = result
				continue
			}

			ttl := time.Duration(entry.TTLSeconds) * time.Second
			writeReq := semantic.WriteRequest{
				VKScope:          entry.VKScope,
				UpstreamProvider: "", // not available at prewarm time; empty is valid
				UpstreamModel:    entry.Model,
				ResponseKind:     "response",
				EmbeddingInput:   entry.Prompt,
				ResponseBody:     []byte(`{"prewarm":true,"response":` + jsonStringEscape(entry.Response) + `}`),
				Usage:            nil,
				TTL:              ttl,
				ProviderBaseURL:  providerBaseURL,
				EmbeddingAPIKey:  apiKey,
			}

			wr, err := writer.Write(r.Context(), writeReq)
			if err != nil {
				logger.Warn("semantic prewarm: write error",
					"index", i,
					"error", err)
				result.Error = err.Error()
				totalErrors++
				results[i] = result
				continue
			}

			totalCalls++
			if wr.Stored {
				result.Written = true
				result.EmbeddingCostUSD = wr.EmbeddingCostUSD
				totalCostUSD += wr.EmbeddingCostUSD
				totalWritten++
			} else {
				result.Skipped = true
				result.SkipReason = string(wr.SkipReason)
				totalSkipped++
			}
			results[i] = result
		}

		resp := SemanticPrewarmResponse{
			Written:          totalWritten,
			Skipped:          totalSkipped,
			Errors:           totalErrors,
			EmbeddingCalls:   totalCalls,
			EmbeddingCostUSD: totalCostUSD,
			DurationMs:       time.Since(start).Milliseconds(),
			DryRun:           req.DryRun,
			Entries:          results,
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// resolveEmbeddingCreds returns the embedding provider base URL + decrypted
// API key from the live ConfigSnapshot + CredManager. Returns a non-empty
// skipReason when resolution fails so the caller can stamp every entry with
// a structured skipReason instead of letting the Writer return an opaque
// transport error.
//
// Failure modes (in priority order):
//   - cfgCache nil or snapshot empty → "semantic_unavailable"
//   - snap.EmbeddingProviderID empty → "semantic_unavailable"
//   - snap.EmbeddingProviderBaseURL empty → "semantic_unavailable"
//   - creds nil → "embedding_provider_error"
//   - creds.GetForProvider error → "embedding_provider_error"
//   - decrypted key empty → "embedding_provider_error"
func resolveEmbeddingCreds(
	ctx context.Context,
	cfgCache configSnapshotGetter,
	creds credResolverIface,
	logger *slog.Logger,
) (baseURL, apiKey, skipReason string) {
	if cfgCache == nil {
		return "", "", "semantic_unavailable"
	}
	snap := cfgCache.Get()
	if snap.EmbeddingProviderID == "" || snap.EmbeddingProviderBaseURL == "" {
		return "", "", "semantic_unavailable"
	}
	if creds == nil {
		logger.Warn("semantic prewarm: CredManager is nil; cannot resolve embedding API key",
			"providerID", snap.EmbeddingProviderID)
		return "", "", "embedding_provider_error"
	}
	key, _, _, err := creds.GetForProvider(ctx, snap.EmbeddingProviderID)
	if err != nil {
		logger.Warn("semantic prewarm: credential lookup failed",
			"providerID", snap.EmbeddingProviderID, "error", err)
		return "", "", "embedding_provider_error"
	}
	if key == "" {
		logger.Warn("semantic prewarm: decrypted credential is empty",
			"providerID", snap.EmbeddingProviderID)
		return "", "", "embedding_provider_error"
	}
	return snap.EmbeddingProviderBaseURL, key, ""
}

// jsonStringEscape wraps s in a JSON string literal. Used to build the
// prewarm response_body payload inline without a full JSON marshal round-trip.
func jsonStringEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
