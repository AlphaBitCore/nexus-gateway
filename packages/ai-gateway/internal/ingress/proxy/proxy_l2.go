package proxy

// proxy_l2.go — L2 semantic cache read and write-back integration.
//
// After an L1 miss the handler invokes tryL2Lookup which:
//  1. Decodes the per-route SemanticCacheSettings from response_cache_policy JSONB.
//  2. Enforces the per-route daily embedding budget via BudgetTracker.
//  3. Builds the embedding input string via inputstaging.Plan.
//  4. Calls semantic.Reader.Read (embedding + FT.SEARCH).
//  5. On a hit: stamps rec, serves the response via the shared handleStreamHit /
//     handleNonStreamHit path (GatewayCacheKind=semantic), and returns hit=true.
//  6. On a miss / skip: stamps rec.EmbeddingCostUsd and returns hit=false so the
//     caller proceeds to broker dispatch.
//
// After a successful broker dispatch the handler calls scheduleL2Write which
// fires semantic.Writer.Write in a detached goroutine with a 5-second deadline.
// The write is best-effort: any error is logged inside Writer and never surfaces
// to the response path.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/cache/semantic"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/inputstaging"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// semanticCachePolicy is the in-process L2 semantic cache settings used by
// tryL2Lookup + scheduleL2Write. All four fields come from the fleet singleton
// (semantic_cache_config) — ConfigCache.Set normalizes each at the in-process
// boundary so a stale snapshot can't produce invalid values.
type semanticCachePolicy struct {
	Threshold       float32
	EmbedStrategy   string
	VaryBy          string
	AllowCrossModel bool
}

// fleetSemanticPolicy returns the L2 policy assembled from the fleet-wide
// semantic_cache_config snapshot. Returns (semanticCachePolicy{}, false) when
// the snapshot is missing, disabled, or not yet populated by the Hub shadow
// push.
func fleetSemanticPolicy(cc *semantic.ConfigCache) (semanticCachePolicy, bool) {
	if cc == nil || !cc.EffectiveEnabled() {
		return semanticCachePolicy{}, false
	}
	snap := cc.Get()
	return semanticCachePolicy{
		Threshold:       snap.Threshold,
		EmbedStrategy:   snap.EmbedStrategy,
		VaryBy:          snap.VaryBy,
		AllowCrossModel: snap.AllowCrossModel,
	}, true
}

// resolveL2VKScope converts the per-route varyBy field + audit record into the
// VK-scope string used as a Redis tag filter.  The scope isolates entries by
// the dimension chosen by the admin:
//   - "vk"   → rec.VirtualKeyID (strict, most common default)
//   - "user" → rec.UserID
//   - "org"  → rec.OrgID
//   - "none" → "" (cross-tenant)
func resolveL2VKScope(rec *audit.Record, varyBy string) string {
	switch varyBy {
	case "user":
		return rec.UserID
	case "org":
		return rec.OrganizationID
	case "none":
		return ""
	default: // "vk" and anything else → strict VK isolation
		return rec.VirtualKeyID
	}
}

// canonicalMsgsToInputStaging converts []normcore.Message (from the canonical
// NormalizedPayload) to []inputstaging.Message, joining all text content blocks
// into a single string per message.  Images, tool calls, and tool results are
// omitted — inputstaging only reasons over text content.
func canonicalMsgsToInputStaging(msgs []normcore.Message) []inputstaging.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]inputstaging.Message, 0, len(msgs))
	for _, m := range msgs {
		text := joinTextBlocksL2(m.Content)
		if text == "" {
			continue
		}
		out = append(out, inputstaging.Message{
			Role:    string(m.Role),
			Content: text,
		})
	}
	return out
}

// joinTextBlocksL2 concatenates all ContentText blocks in a ContentBlock slice,
// separated by a space.  Non-text blocks (images, tool calls, tool results)
// are omitted.
func joinTextBlocksL2(blocks []normcore.ContentBlock) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == normcore.ContentText && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte(' ')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// buildEmbeddingInput runs inputstaging.Plan on the canonical messages and
// joins the result into a single string.  Returns ("", false) when the
// messages are empty or the plan produces no output.
func buildEmbeddingInput(msgs []normcore.Message, strategy inputstaging.Strategy) (string, bool) {
	stagingMsgs := canonicalMsgsToInputStaging(msgs)
	if len(stagingMsgs) == 0 {
		return "", false
	}
	if !strategy.Valid() {
		strategy = inputstaging.StrategySystemPlusLastUser
	}
	// EmbeddingDimension from the ConfigCache informs model context limit.
	// Use a generous fallback (8192) when the fleet singleton is not yet
	// configured; inputstaging.Plan hard-fails only on ModelContextLimit<1.
	const fallbackContextLimit = 8192
	plan, planErr := inputstaging.Plan(inputstaging.PlanInput{
		Messages:          stagingMsgs,
		ModelContextLimit: fallbackContextLimit,
		Strategy:          strategy,
	})
	if planErr != nil || len(plan.Messages) == 0 {
		return "", false
	}
	var sb strings.Builder
	for i, m := range plan.Messages {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(m.Content)
	}
	return sb.String(), true
}

// l2ReadParams bundles the parameters for tryL2Lookup so the call site stays
// readable (the function would otherwise have 20+ parameters).
type l2ReadParams struct {
	r             *http.Request
	w             http.ResponseWriter
	rec           *audit.Record
	routeResult   *routingcore.RouteResult
	primary       routingcore.RoutingTarget
	isStream      bool
	resolved      Ingress
	reqHookResult *hookcore.CompliancePipelineResult
	quotaInPrice  float64
	quotaOutPrice float64
	quotaDecision *quota.Decision
	endpointType  string
	requestID     string
	start         time.Time
	logger        *slog.Logger
	canonicalMsgs []normcore.Message
}

// tryL2Lookup attempts an L2 semantic cache lookup after an L1 miss.
//
// It returns hit=true when the lookup succeeded and a cached response was
// served to the client (the caller must return immediately).  It returns
// hit=false on miss, skip, or any error — the caller proceeds to broker dispatch.
//
// The EmbeddingCostUSD is always stamped on rec regardless of the L2 outcome
// so the budget accounting in traffic_event is accurate.
func (h *Handler) tryL2Lookup(p l2ReadParams) (hit bool) {
	if h.deps.SemanticReader == nil {
		return false
	}

	// Fleet-wide gate: L2 only fires when an admin has enabled the
	// semantic_cache_config singleton AND the embedding provider/model are
	// configured.
	pol, ok := fleetSemanticPolicy(h.deps.SemanticConfigCache)
	if !ok {
		return false
	}

	// Build the embedding input via inputstaging.Plan.
	embInput, ok2 := buildEmbeddingInput(p.canonicalMsgs, inputstaging.Strategy(pol.EmbedStrategy))
	if !ok2 {
		// No text content or plan failed — skip L2.
		p.rec.GatewayCacheSkipReason = audit.GatewayCacheSkipReasonOversizeForEmbedding
		return false
	}

	vkScope := resolveL2VKScope(p.rec, pol.VaryBy)
	responseKind := "response"
	if p.isStream {
		responseKind = "stream"
	}

	// Pull provider join fields straight from the Hub-pushed snapshot —
	// CP's SemanticCacheStore.Get LEFT JOINs Provider + Model so the
	// gateway never has to look them up per-request on the L2 hot path.
	// The decrypted API key still comes from the credentials snapshot
	// (CredManager is itself Hub-fed and in-memory).
	snap := h.deps.SemanticConfigCache.Get()
	if h.deps.CredManager == nil {
		// Defensive — boot-time wiring guarantees CredManager, but tests
		// that hand-construct Handler may omit it. Without credentials we
		// cannot call the embedding upstream, so skip L2 quietly.
		return false
	}
	embAPIKey, _, _, credErr := h.deps.CredManager.GetForProvider(p.r.Context(), snap.EmbeddingProviderID)
	if credErr != nil || snap.EmbeddingProviderBaseURL == "" || snap.EmbeddingProviderModelID == "" {
		p.logger.Warn("l2: missing embedding creds or provider join fields; skipping L2",
			slog.Any("credErr", credErr),
			slog.String("baseURL", snap.EmbeddingProviderBaseURL),
			slog.String("wireModel", snap.EmbeddingProviderModelID))
		return false
	}

	// Per-token cost = USD-per-million / 1e6. Snapshot-resident, no DB hit.
	costPerInputToken := snap.EmbeddingInputPricePerMillion / 1_000_000.0
	readReq := semantic.ReadRequest{
		VKScope:              vkScope,
		UpstreamProvider:     p.primary.ProviderID,
		UpstreamModel:        p.primary.ProviderModelID,
		ResponseKind:         responseKind,
		EmbeddingInput:       embInput,
		ProviderBaseURL:      snap.EmbeddingProviderBaseURL,
		EmbeddingAPIKey:      embAPIKey,
		EmbeddingWireModel:   snap.EmbeddingProviderModelID,
		CostPerInputTokenUSD: costPerInputToken,
		Threshold:            pol.Threshold,
		AllowCrossModel:      pol.AllowCrossModel,
	}

	result, readErr := h.deps.SemanticReader.Read(p.r.Context(), readReq)
	if readErr != nil {
		p.logger.Warn("l2: semantic read error; proceeding to broker", slog.Any("error", readErr))
		return false
	}

	// Always stamp the embedding cost and model regardless of outcome (T4.1).
	p.rec.EmbeddingCostUsd = result.EmbeddingCostUSD
	if result.EmbeddingModelID != "" {
		p.rec.EmbeddingModelID = result.EmbeddingModelID
	}

	if result.Entry == nil {
		// Miss or skip — stamp the skip reason when set and proceed to broker.
		if result.SkipReason != "" {
			p.rec.GatewayCacheSkipReason = result.SkipReason
		}
		return false
	}

	// L2 HIT — serve the cached response.
	p.rec.GatewayCacheStatus = audit.GatewayCacheHit
	p.rec.GatewayCacheKind = audit.GatewayCacheKindSemantic
	// Stamp the Redis HASH key of the served entry so the admin UI's
	// "Mark as bad cache hit" thumbs-down can post the real poison-list
	// key. The entry key (not traffic_event.id) is what the Reader's
	// IsPoisoned check compares against.
	p.rec.GatewayCacheL2EntryKey = result.Entry.EntryKey
	p.rec.ProviderCacheStatus = audit.ProviderCacheNA

	if p.isStream {
		entry, convErr := semantic.ToCacheStreamEntry(result.Entry)
		if convErr != nil {
			p.logger.Warn("l2: stream entry conversion failed; proceeding to broker", slog.Any("error", convErr))
			// Reset L2 stamps so the broker path re-stamps correctly.
			p.rec.GatewayCacheStatus = ""
			p.rec.GatewayCacheKind = ""
			p.rec.GatewayCacheL2EntryKey = ""
			p.rec.ProviderCacheStatus = ""
			return false
		}
		h.handleStreamHit(p.r, p.w, p.rec, p.primary, p.routeResult, p.reqHookResult,
			entry, p.quotaInPrice, p.quotaOutPrice, p.quotaDecision,
			p.endpointType, p.requestID, p.start, p.logger)
		return true
	}

	entry, convErr := semantic.ToCacheResponseEntry(result.Entry)
	if convErr != nil {
		p.logger.Warn("l2: response entry conversion failed; proceeding to broker", slog.Any("error", convErr))
		// Reset L2 stamps.
		p.rec.GatewayCacheStatus = ""
		p.rec.GatewayCacheKind = ""
		p.rec.GatewayCacheL2EntryKey = ""
		p.rec.ProviderCacheStatus = ""
		return false
	}
	h.handleNonStreamHit(p.r, p.w, p.rec, p.primary, p.routeResult, p.reqHookResult,
		entry, p.quotaInPrice, p.quotaOutPrice, p.quotaDecision,
		p.endpointType, p.requestID, p.start, p.logger)
	return true
}

// scheduleL2Write fires a semantic cache write in a background goroutine after a
// successful live upstream dispatch.  The write is bounded by a 5-second deadline
// so it never delays response delivery on a slow embedding provider.
//
// responseBody is the raw upstream response bytes; usage is the parsed token map.
// Only non-streaming responses are written here; streaming write-back is handled
// by the broker persisting the SSE timeline.
func (h *Handler) scheduleL2Write(
	routeResult *routingcore.RouteResult,
	primary routingcore.RoutingTarget,
	canonicalMsgs []normcore.Message,
	responseBody []byte,
	usage map[string]any,
	vkScope string,
	isStream bool,
	origin Ingress,
	logger *slog.Logger,
) {
	if h.deps.SemanticWriter == nil || len(responseBody) == 0 || isStream {
		return
	}
	pol, ok := fleetSemanticPolicy(h.deps.SemanticConfigCache)
	if !ok {
		return
	}

	embInput, ok2 := buildEmbeddingInput(canonicalMsgs, inputstaging.Strategy(pol.EmbedStrategy))
	if !ok2 {
		return
	}

	// Same snapshot-driven setup as tryL2Lookup — no per-request DB hits.
	snap := h.deps.SemanticConfigCache.Get()
	if h.deps.CredManager == nil {
		// Same defensive guard as tryL2Lookup — tests that hand-construct
		// Handler may omit CredManager. Production wiring always supplies one.
		return
	}
	embAPIKey, _, _, credErr := h.deps.CredManager.GetForProvider(
		context.Background(), snap.EmbeddingProviderID)
	if credErr != nil || snap.EmbeddingProviderBaseURL == "" || snap.EmbeddingProviderModelID == "" {
		logger.Warn("l2: write skipped — missing embedding creds or provider join fields",
			slog.Any("credErr", credErr))
		return
	}

	writeReq := semantic.WriteRequest{
		VKScope:            vkScope,
		UpstreamProvider:   primary.ProviderID,
		UpstreamModel:      primary.ProviderModelID,
		ResponseKind:       "response",
		EmbeddingInput:     embInput,
		ResponseBody:       responseBody,
		Usage:              usage,
		TTL:                24 * time.Hour,
		ProviderBaseURL:    snap.EmbeddingProviderBaseURL,
		EmbeddingAPIKey:    embAPIKey,
		EmbeddingWireModel: snap.EmbeddingProviderModelID,
		OriginWireShape:    origin.WireShape,
	}

	go func() {
		wCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := h.deps.SemanticWriter.Write(wCtx, writeReq); err != nil {
			logger.Warn("l2: semantic write error (best-effort)", slog.Any("error", err))
		}
	}()
}


// provcoreUsageToMap converts a provcore.Usage to the map[string]any shape
// the semantic Writer persists into Valkey. Without this conversion the L2
// HIT path served the response with all token counters NULL on the row —
// every analytics rollup and budget check that assumes "tokens on every
// row" was getting silent zeros. Both broker and non-broker call sites
// go through here so the two paths stamp identical fields. Nil-input
// returns nil (Writer treats nil as "no usage", which is still better
// than storing a misleading zero-filled map).
func provcoreUsageToMap(u *provcore.Usage) map[string]any {
	if u == nil {
		return nil
	}
	m := map[string]any{}
	if u.PromptTokens != nil {
		m["prompt_tokens"] = *u.PromptTokens
	}
	if u.CompletionTokens != nil {
		m["completion_tokens"] = *u.CompletionTokens
	}
	if u.TotalTokens != nil {
		m["total_tokens"] = *u.TotalTokens
	}
	if u.ReasoningTokens != nil {
		m["reasoning_tokens"] = *u.ReasoningTokens
	}
	if u.CacheReadTokens != nil {
		m["cache_read_tokens"] = *u.CacheReadTokens
	}
	if u.CacheCreationTokens != nil {
		m["cache_creation_tokens"] = *u.CacheCreationTokens
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
