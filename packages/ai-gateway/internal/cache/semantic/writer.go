package semantic

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/embeddings"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// WriteRequest carries the parameters for a single L2 semantic cache write.
// The caller (typically the stream broker writeCache path) populates this
// after a successful upstream response on an L1 miss.
type WriteRequest struct {
	// Scope and routing
	VKScope          string
	UpstreamProvider string
	UpstreamModel    string
	ResponseKind     string // "response" | "stream"

	// Content
	EmbeddingInput string // exact text fed to the embedding model
	ResponseBody   []byte
	Usage          map[string]any
	TTL            time.Duration

	// Embedding provider credentials
	ProviderBaseURL string // pulled by caller from current Provider row
	EmbeddingAPIKey string // pulled by caller from Credential row; never stored

	// EmbeddingWireModel — see ReadRequest.EmbeddingWireModel. Same role:
	// the upstream-facing provider model id (e.g. "text-embedding-3-small"),
	// not the DB UUID.
	EmbeddingWireModel string

	// Cost accounting: cost per input token in USD, pre-computed by the caller.
	// Writer multiplies by PromptTokens from the embed response.
	CostPerInputTokenUSD float64

	// OriginWireShape encodes both the ingress endpoint kind and body
	// format; tagged so cross-ingress reshape can decide whether to
	// re-encode or serve verbatim. See cache/core.ResponseEntry.
	OriginWireShape typology.WireShape
}

// WriteResult summarises the outcome of a Write call.
type WriteResult struct {
	Stored           bool
	Skipped          bool
	SkipReason       audit.GatewayCacheSkipReason
	EmbeddingCostUSD float64 // 0.0 when no embedding call was made (skipped or joiner)
}

// Writer orchestrates the L2 semantic cache write path:
//  1. Check EffectiveEnabled.
//  2. Run EmbeddingSingleflight.Embed.
//  3. Translate embedding errors to skip-reasons.
//  4. Call Client.StoreEntry.
//  5. Translate Valkey errors to skip-reasons.
//
// The write is best-effort: a failure logs at WARN, increments a metric, and
// returns Skipped=true — it must never fail the L1 write or the response
// delivery. Callers are encouraged to run Write in a goroutine bounded by a
// separate context (see S4 wiring note in doc.go).
type Writer struct {
	cache         *ConfigCache
	client        *Client
	sf            *EmbeddingSingleflight
	log           *slog.Logger
	maxEntryBytes int
	metrics       *Metrics
}

// NewWriter constructs a Writer. maxEntryBytes ≤ 0 uses the package default
// (256 KiB). metrics may be nil (no-op in that case).
func NewWriter(
	cache *ConfigCache,
	client *Client,
	sf *EmbeddingSingleflight,
	log *slog.Logger,
	maxEntryBytes int,
	metrics *Metrics,
) *Writer {
	if maxEntryBytes <= 0 {
		maxEntryBytes = defaultMaxEntryBytes
	}
	return &Writer{
		cache:         cache,
		client:        client,
		sf:            sf,
		log:           log,
		maxEntryBytes: maxEntryBytes,
		metrics:       metrics,
	}
}

// Write executes the L2 write flow for a single request.
//
// The flow:
//  1. Read ConfigSnapshot. If EffectiveEnabled()==false, return
//     Skipped+SemanticUnavailable.
//  2. Call EmbeddingSingleflight.Embed. Translate errors to skip-reasons:
//     - timeout                → embedding_timeout
//     - circuit open           → embedding_circuit_open
//     - dim mismatch           → embedding_dim_mismatch
//     - provider error (other) → embedding_provider_error
//  3. Verify embedding dimension matches ConfigSnapshot.EmbeddingDimension.
//  4. Call Client.StoreEntry. Translate Valkey errors to skip-reasons:
//     - ErrEntryTooLarge → (skip, no explicit reason — count as ok-but-too-large)
//     - ErrValkeyUnavailable → valkey_unavailable
//     - other              → semantic_search_error
//  5. On success, return Stored=true + cost.
func (w *Writer) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	start := time.Now()

	// Step 1: check fleet-wide enabled + provider/model config.
	if !w.cache.EffectiveEnabled() {
		w.metrics.IncWrite("skip_disabled")
		return WriteResult{
			Skipped:    true,
			SkipReason: audit.GatewayCacheSkipReasonSemanticUnavailable,
		}, nil
	}

	snap := w.cache.Get()

	// Step 2: call embedding via singleflight. Use the wire model code
	// passed in by the caller (Model.providerModelId, not the DB UUID).
	wireModel := req.EmbeddingWireModel
	if wireModel == "" {
		wireModel = snap.EmbeddingModelID
	}
	embedReq := embeddings.Request{
		Model:          wireModel,
		Input:          req.EmbeddingInput,
		Dimensions:     snap.EmbeddingDimension,
		EncodingFormat: "float",
	}

	embedStart := time.Now()
	resp, err := w.sf.Embed(ctx, snap.EmbeddingProviderID, req.ProviderBaseURL, wireModel, req.EmbeddingAPIKey, embedReq)
	embedLatency := time.Since(embedStart).Seconds()

	if err != nil {
		reason := translateEmbedError(err)
		outcome := skipReasonToMetricLabel(reason)
		w.metrics.IncWrite(outcome)
		w.metrics.IncEmbeddingCall(snap.EmbeddingProviderID, snap.EmbeddingModelID, embedErrorToOutcome(err))
		w.log.Warn("cache/semantic: write skipped — embedding error",
			"reason", reason, "error", err,
			"provider", snap.EmbeddingProviderID, "model", snap.EmbeddingModelID)
		return WriteResult{Skipped: true, SkipReason: reason}, nil
	}

	w.metrics.IncEmbeddingCall(snap.EmbeddingProviderID, snap.EmbeddingModelID, "ok")
	w.metrics.ObserveEmbeddingLatency(embedLatency)

	// Step 3: verify embedding dimension.
	if snap.EmbeddingDimension > 0 && len(resp.Embedding) != snap.EmbeddingDimension {
		w.metrics.IncWrite("skip_embedding_dim_mismatch")
		w.log.Warn("cache/semantic: write skipped — embedding dim mismatch",
			"expected", snap.EmbeddingDimension, "got", len(resp.Embedding))
		return WriteResult{
			Skipped:    true,
			SkipReason: audit.GatewayCacheSkipReasonEmbeddingDimMismatch,
		}, nil
	}

	// Compute cost (leader only; joiners share the result and are stamped 0.0
	// by the singleflight — their cost appears on the leader's WriteResult).
	costUSD := float64(resp.PromptTokens) * req.CostPerInputTokenUSD
	w.metrics.AddEmbeddingCost(snap.EmbeddingProviderID, snap.EmbeddingModelID, costUSD)

	// Step 4: write to Valkey.
	storeIn := StoreInput{
		VKScope:          req.VKScope,
		UpstreamProvider: req.UpstreamProvider,
		UpstreamModel:    req.UpstreamModel,
		ResponseKind:     req.ResponseKind,
		Fingerprint:      snap.Fingerprint,
		EmbeddingInput:   req.EmbeddingInput,
		Embedding:        resp.Embedding,
		ResponseBody:     req.ResponseBody,
		Usage:            req.Usage,
		TTL:              req.TTL,
		OriginWireShape:  req.OriginWireShape,
	}

	if err := w.client.StoreEntry(ctx, snap.RedisIndexName, storeIn, w.maxEntryBytes); err != nil {
		reason, outcome := translateStoreError(err)
		w.metrics.IncWrite(outcome)
		w.log.Warn("cache/semantic: write skipped — store error",
			"reason", reason, "error", err,
			"index", snap.RedisIndexName)
		if reason != "" {
			return WriteResult{Skipped: true, SkipReason: reason}, nil
		}
		// ErrEntryTooLarge: count but not a skip-reason audit constant.
		return WriteResult{Skipped: true}, nil
	}

	w.metrics.ObserveEntrySize(len(req.ResponseBody))
	w.metrics.ObserveWriteLatency(time.Since(start).Seconds())
	w.metrics.IncWrite("ok")

	return WriteResult{
		Stored:           true,
		EmbeddingCostUSD: costUSD,
	}, nil
}

// Error translation helpers

// translateEmbedError maps an embedding call error to a GatewayCacheSkipReason.
func translateEmbedError(err error) audit.GatewayCacheSkipReason {
	if errors.Is(err, ErrCircuitOpen) {
		return audit.GatewayCacheSkipReasonEmbeddingCircuitOpen
	}
	if errors.Is(err, embeddings.ErrEmbeddingTimeout) {
		return audit.GatewayCacheSkipReasonEmbeddingTimeout
	}
	if errors.Is(err, embeddings.ErrEmbeddingDimMismatch) {
		return audit.GatewayCacheSkipReasonEmbeddingDimMismatch
	}
	if errors.Is(err, embeddings.ErrEmbeddingProviderError) {
		return audit.GatewayCacheSkipReasonEmbeddingProviderError
	}
	// Ctx cancellation from the caller (caller disconnected).
	return audit.GatewayCacheSkipReasonEmbeddingTimeout
}

// translateStoreError maps a Client.StoreEntry error to a skip-reason and a
// metric outcome label. Returns empty reason for ErrEntryTooLarge (no audit
// constant — counted on the metric only).
func translateStoreError(err error) (audit.GatewayCacheSkipReason, string) {
	if errors.Is(err, ErrEntryTooLarge) {
		return "", "skip_oversize"
	}
	if errors.Is(err, ErrValkeyUnavailable) {
		return audit.GatewayCacheSkipReasonValkeyUnavailable, "skip_valkey_unavailable"
	}
	return audit.GatewayCacheSkipReasonSemanticSearchError, "skip_search_error"
}

// skipReasonToMetricLabel maps a GatewayCacheSkipReason to its metric label.
func skipReasonToMetricLabel(r audit.GatewayCacheSkipReason) string {
	switch r {
	case audit.GatewayCacheSkipReasonSemanticUnavailable:
		return "skip_disabled"
	case audit.GatewayCacheSkipReasonEmbeddingTimeout:
		return "skip_embedding_timeout"
	case audit.GatewayCacheSkipReasonEmbeddingCircuitOpen:
		return "skip_embedding_circuit"
	case audit.GatewayCacheSkipReasonEmbeddingDimMismatch:
		return "skip_embedding_dim_mismatch"
	case audit.GatewayCacheSkipReasonEmbeddingProviderError:
		return "skip_embedding_error"
	case audit.GatewayCacheSkipReasonValkeyUnavailable:
		return "skip_valkey_unavailable"
	case audit.GatewayCacheSkipReasonSemanticSearchError:
		return "skip_search_error"
	default:
		return "skip_unknown"
	}
}

// embedErrorToOutcome returns the embedding call outcome label for metrics.
func embedErrorToOutcome(err error) string {
	if errors.Is(err, ErrCircuitOpen) {
		return "circuit_open"
	}
	if errors.Is(err, embeddings.ErrEmbeddingTimeout) {
		return "error"
	}
	return "error"
}
