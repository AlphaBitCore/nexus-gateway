package proxy

// proxy_upstream.go holds the upstream-fetch (prepared-body execution) and the
// non-streaming response handling (egress reshape + handleNonStream) split out of
// proxy.go (behavior unchanged) — the response-execution helpers ServeProxy drives
// after routing/quota.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/executor"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/ingress/envelope"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/quota"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	openairesponses "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/responses"
	routingcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/routing/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// fetchUpstreamWithPreparedBody dispatches the request to upstream
// providers via TargetExecutor. The body for the primary target's
// first attempt has already been produced by Adapter.PrepareBody —
// used by the broker leader path so the adapter does not run
// PrepareBody twice (once for the cache key, once inside Execute).
//
// preparedBody MUST be the bytes Adapter.PrepareBody would return for
// routeResult.Targets[0]; preparedRewrites MUST be the matching
// rewrites slice; preparedURLOverride MUST be the matching codec
// URLOverride (so a shape-driven action URL reaches the dispatch).
// Pass nil/nil/"" to fall back to plain Execute behaviour (which
// re-runs PrepareBody internally — idempotent, behaviour-equivalent, just
// one redundant µs-scale encode on the success path).
//
// Returns the execution result, the winning target, the retry count,
// and an error. On failure the error response is already written to w.
// The ingress descriptor's Endpoint and BodyFormat drive the adapter's
// passthrough/translate decision.
func (h *Handler) fetchUpstreamWithPreparedBody(r *http.Request, w http.ResponseWriter, rec *audit.Record, routeResult *routingcore.RouteResult, body []byte, isStream bool, in Ingress, preparedBody []byte, preparedRewrites []string, preparedURLOverride string, start time.Time, logger *slog.Logger) (*executor.ExecutionResult, routingcore.RoutingTarget, int, error) {
	pcCfg := h.payloadCaptureConfig()
	maxResp := pcCfg.MaxResponseBytes
	if maxResp <= 0 {
		maxResp = payloadcapture.DefaultMaxResponseBytes
	}
	policy := h.effectiveRetryPolicy(routeResult.RuleRetryPolicyJSON, logger)
	req := buildProviderRequest(r, in, body, isStream, maxResp)
	req.StickyKey = stickyKeyFromCtx(r.Context())
	var execResult *executor.ExecutionResult
	if preparedBody != nil {
		execResult = h.deps.Executor.ExecuteWithPreparedBody(r.Context(), routeResult.Targets, req, policy, preparedBody, preparedRewrites, preparedURLOverride)
	} else {
		execResult = h.deps.Executor.Execute(r.Context(), routeResult.Targets, req, policy)
	}

	// Upstream call count for the response header. 1 means first-try success;
	// 2+ means at least one L2 retry or L3 failover happened. Counts only
	// calls that reached a provider — a target abandoned before dispatch
	// never made one. Defensive floor at 1 — if the executor returned a
	// result, the client asked us to reach upstream at least once.
	attempts := execResult.UpstreamAttempts()
	if attempts < 1 {
		attempts = 1
	}

	// Attribute the audit record to the attempt that actually reached a
	// provider: its credential is the one that saw the outcome, which is the
	// answer to "which key got rate-limited?". Fall back to the last entry
	// only when nothing dispatched at all — it still names the target we were
	// trying to reach, and there is no credential to report.
	// rec.ModelName (requested side) was stamped right after readBody with the
	// literal client model string; only Routed* fields get set here.
	attributed := execResult.Terminal()
	if attributed == nil {
		if n := len(execResult.Attempts); n > 0 {
			attributed = &execResult.Attempts[n-1]
		}
	}
	if attributed != nil {
		rec.CredentialID = attributed.CredentialID
		rec.CredentialName = attributed.CredentialName
		rec.RoutedProviderID = attributed.Target.ProviderID
		rec.RoutedProviderName = attributed.Target.ProviderName
		rec.RoutedModelID = attributed.Target.ModelID
		rec.RoutedModelName = attributed.Target.ModelCode
		rec.TargetHost = upstreamHost(attributed.Target)
	}

	// The executor has returned, so everything below writes to the client after
	// however long the upstream took — up to the full upstream budget across
	// retries and failover. The flat server.writeTimeout was armed when the
	// request header was read and is sized for ordinary responses, so every
	// write past this point (client-closed, rate-limited, all-providers-failed,
	// and the 4xx envelope below) would be severed without this. Arming it once
	// here covers them all: the deadline is absolute and lives on the
	// connection. handleNonStream arms it again for the success body, which it
	// writes from a later stage.
	extendWriteDeadlineToUpstreamBudget(w)

	if execResult.Error != nil {
		// Client gone before we produced a response: the inbound request context
		// is canceled (the client closed the connection / hit its own deadline),
		// which propagated into the upstream call and surfaced as an attempt
		// failure. This is a CLIENT-side cancellation, not a provider fault —
		// record it as 499 CLIENT_CLOSED so provider-availability metrics and
		// dashboards don't blame the upstream for a client disconnect. Checked
		// first: a client that walked away dominates whatever the in-flight
		// attempt was about to report. (The body write is a no-op on a closed
		// connection; the value is the audit/metrics attribution via rec.)
		if ctxErr := r.Context().Err(); errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded) {
			// No errors_total here: the client walked away, so nothing
			// upstream failed and provider-availability alerting must not
			// count it against the provider.
			h.writeDetailedErr(w, rec, statusClientClosedRequest, "CLIENT_CLOSED", "client closed request before upstream responded", "")
			return nil, routingcore.RoutingTarget{}, attempts, execResult.Error
		}

		// The counter is O(1) and rate limits belong in it — an aggregate is
		// exactly the right instrument for a failure mode whose whole shape is
		// its rate. The per-attempt log is not, and stays below the rate-limit
		// return: a rate-limit storm is high-volume by definition, so logging
		// each one would put unbounded event volume on the hot failure path.
		terminal := execResult.Terminal()
		h.recordUpstreamFailure(terminal)

		// A rate limit must reach the client as a 429 so it backs off; an
		// opaque 502 tells it the provider is down, which is both false and
		// the one message that will not produce a backoff.
		//
		// Code and Status are complementary signals here, not alternatives,
		// so either one alone loses rate limits the other catches. Code is
		// normalised from the provider's own error type, which catches a
		// provider reporting "overloaded" on a non-429 status (Anthropic's
		// 529). Status is what the provider actually replied, which catches a
		// 429 whose body typed the cause as something else: the normalizers
		// derive Code from the type and keep Status at the raw value, and the
		// status fallback that would map 429 to rate_limited is skipped once a
		// recognised type has set Code (an Anthropic "api_error" or a Gemini
		// "UNAVAILABLE" on a 429 lands as upstream_error/429).
		if terminal != nil &&
			(terminal.Code == provcore.CodeRateLimited || terminal.StatusCode == http.StatusTooManyRequests) {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "PROVIDER_RATE_LIMITED", "upstream rate limit exceeded", "")
			return nil, routingcore.RoutingTarget{}, attempts, execResult.Error
		}
		logUpstreamFailures(logger, execResult.Attempts)
		h.writeDetailedErr(w, rec, http.StatusBadGateway, "PROVIDER_UNAVAILABLE", "all upstream providers failed", "")
		return nil, routingcore.RoutingTarget{}, attempts, execResult.Error
	}

	target := execResult.Target

	// 4xx from upstream — write the envelope in the ingress format the
	// caller expects, not the upstream's native shape, so cross-format
	// clients (OpenAI SDK calling /v1/chat/completions that the gateway
	// routed to an Anthropic upstream) can parse the body. Also forward
	// the upstream's allowlisted response headers so debugging metadata
	// like x-request-id / retry-after reaches the client on the error
	// path, matching the success-path forwarding at writeForwardedResponseHeaders.
	if execResult.StatusCode >= 400 {
		rec.StatusCode = execResult.StatusCode
		// The upstream's own canonical cause, so this column can answer
		// "the credential is rejected" vs "the request was malformed" vs
		// "the prompt exceeds the window" — three failures an operator acts on
		// differently and which a single blanket code cannot separate.
		// PROVIDER_ERROR stays the value for a terminal 4xx that carries no
		// canonical code: the classifier promises an envelope for this class,
		// but its defensive fallback can reach here without one, and an
		// unclassified failure must not borrow another failure's name.
		rec.ErrorCode = "PROVIDER_ERROR"
		if pe := execResult.ProviderError; pe != nil && pe.Code != "" {
			rec.ErrorCode = pe.Code
		}
		h.recordUpstreamFailure(execResult.Terminal())
		rec.ErrorReason = extractProviderErrorMessage(execResult.Body, execResult.StatusCode)
		rec.RoutedProviderID = target.ProviderID
		rec.RoutedProviderName = target.ProviderName
		rec.RoutedModelID = target.ModelID
		rec.RoutedModelName = target.ModelCode
		rec.TargetHost = upstreamHost(target)
		// Stamp upstream URL on the error path too — same source as
		// the success path below. ToMessage's firstNonEmptyStr fallback
		// covers synthetic transport failures that never reached the
		// network (empty TargetPath → falls back to rec.Path).
		rec.TargetMethod = execResult.TargetMethod
		rec.TargetPath = execResult.TargetPath
		rec.LatencyMs = int(time.Since(start).Milliseconds())

		ingress, _ := IngressFromContext(r.Context())
		upstreamFormat := provcore.Format(target.AdapterType)

		// Stale Gemini cachedContent invalidation. Gemini returns 403
		// "CachedContent not found (or permission denied)" when our
		// Redis-cached name points to content the upstream has already
		// evicted. Fire the per-request invalidate hook (set by
		// ServeProxy on cache HIT) so the next request regenerates
		// instead of looping on the dead ref. The TTL fix in
		// geminicache/manager.go shrinks the stale window but cannot
		// fully eliminate it — Gemini's eviction is best-effort.
		if execResult.StatusCode == http.StatusForbidden &&
			upstreamFormat == provcore.FormatGemini &&
			geminicacheStaleRefError(execResult.Body) {
			if invalidate := GeminiCacheInvalidateFromContext(r.Context()); invalidate != nil {
				invalidate()
				logger.Warn("geminicache: upstream reported stale cachedContent — invalidated Redis entry",
					"provider", target.ProviderName)
			}
		}

		errBody := execResult.Body
		if execResult.ProviderError != nil {
			errBody = envelope.EncodeErrorEnvelopeForIngress(ingress.BodyFormat, upstreamFormat, execResult.ProviderError)
		}

		// Stamp the upstream error body to the audit Record so
		// it lands in traffic_event.payloads.response_body. Previously
		// only ErrorReason (extracted message string) was captured —
		// the full body (with provider stack trace, request ID, etc.)
		// was discarded, making 4xx/5xx triage from the Traffic drawer
		// impossible. Mirrors the success-path stamp at line 2176
		// (subject to the same StoreResponseBody payload-capture
		// config so administrators can opt out for compliance reasons).
		pcCfgErr := h.payloadCaptureConfig()
		if len(errBody) > 0 && pcCfgErr.StoreResponseBody {
			rec.ResponseBody = errBody
			rec.ResponseContentType = "application/json"
		}

		writeForwardedResponseHeaders(w, h.deps.Allowlist, upstreamFormat, execResult.Headers, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(execResult.StatusCode)
		_, _ = w.Write(errBody)
		return nil, target, attempts, fmt.Errorf("upstream %dxx", execResult.StatusCode/100)
	}

	rec.RoutedProviderID = target.ProviderID
	rec.RoutedProviderName = target.ProviderName
	rec.RoutedModelID = target.ModelID
	rec.RoutedModelName = target.ModelCode
	rec.TargetHost = upstreamHost(target)
	// Stamp the actual upstream URL the adapter dispatched to
	// (e.g. "/v1/messages" for the Anthropic side of an OpenAI →
	// Anthropic cross-format call). On synthetic transport errors that
	// never reached the network, ExecutionResult.TargetPath is empty
	// and ToMessage's firstNonEmptyStr fallback substitutes rec.Path.
	rec.TargetMethod = execResult.TargetMethod
	rec.TargetPath = execResult.TargetPath

	return execResult, target, attempts, nil
}

func upstreamHost(target routingcore.RoutingTarget) string {
	if target.BaseURL == "" {
		return target.ProviderName
	}
	u, err := url.Parse(target.BaseURL)
	if err != nil || u.Host == "" {
		return target.ProviderName
	}
	return u.Host
}

// appendHookTrace converts a pipeline.Execute result's per-hook records into
// audit.HookExecRecord entries and grows the rec slice. Stage names line up
// with the pipeline registration ("request" / "response" / "connection") so
// dashboards can group / filter without re-deriving the bucket.
// handleNonStream handles non-streaming JSON responses from the adapter.
// The adapter's Body is in the request's BodyFormat; Usage is reported
// separately on the ExecutionResult. Story s2 populates Usage from
// per-format SchemaCodec.DecodeResponse.
//
// This is the direct (non-broker) path used when cache is disabled or
// the broker registry is not wired. The broker MISS path uses
// handleNonStreamWithSubscription instead, which shares the cache write
// with the broker leader.
// egressReshapeNonStream reshapes a CANONICAL (OpenAI) non-stream response body
// back to the caller's ingress wire shape — the response leg of the round-trip
// invariant "request: A→canonical→B; response: B→canonical→A"
// (provider-adapter-architecture.md §3).
//
// The body is canonical on BOTH live response paths: the adapter's
// SchemaCodec.DecodeResponse decodes the upstream B-shape to canonical OpenAI
// (specAdapter.Execute returns CanonicalBody), so handleNonStream's result.Body
// is canonical, and the broker collects/serves the same canonical bytes. The
// reshape is therefore driven SOLELY by the ingress shape A — never by
// ingress-vs-target. (The prior per-path gates — direct "ingress != target",
// broker "WireShape==OpenAIChat" — both returned canonical OpenAI for a native
// non-OpenAI ingress: anthropic /v1/messages + gemini /v1beta got `choices[]`
// instead of `content[]`/`candidates[]`.)
//
// NOT for the cache HIT path: handleNonStreamHit reads the L1 entry which is
// stored POST-reshape in the writer's ORIGIN wire shape, so it reshapes via the
// OriginWireShape gate, not this helper.
//
// Two skip cases, both correct because the body is already in shape A:
//   - OpenAI-family chat/embeddings ingress: canonical IS the ingress shape, so
//     this is the identity — short-circuit (avoids a no-op call + preserves the
//     same-format passthrough optimisation).
//   - /v1/responses NATIVE passthrough (target serves responses-api natively):
//     the body is already Responses-shape; re-encoding via EncodeResponsesResponse
//     would double-encode and strip output[].content[].text.
func (h *Handler) egressReshapeNonStream(ingress Ingress, target routingcore.RoutingTarget, body []byte) ([]byte, error) {
	if h.deps.CanonicalBridge == nil || len(body) == 0 {
		return body, nil
	}
	if ingress.WireShape == typology.WireShapeOpenAIResponses {
		// Content-authoritative egress: the wire shape follows the ACTUAL
		// response bytes, never the target Format. A Responses-shape body is
		// already in the client's shape (verbatim — zero-loss for built-in
		// tools); a chat.completion body is canonical and re-encodes to the
		// Responses output[] grammar via EncodeResponsesResponse. An
		// unclassifiable body must NEVER be forwarded verbatim to a
		// /v1/responses client — fail closed with a 502 so a chat-shaped or
		// garbage reply can't leak in the wrong wire shape.
		switch openairesponses.ClassifyNonStreamBody(body) {
		case openairesponses.ClassResponses:
			return body, nil
		case openairesponses.ClassChat:
			return h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, body)
		default:
			return nil, fmt.Errorf("egress: unclassifiable /v1/responses upstream body; refusing verbatim passthrough")
		}
	}
	if ingress.BodyFormat.IsOpenAIFamily() {
		// Canonical == OpenAI shape == the caller's shape. Identity.
		return body, nil
	}
	if typology.KindFromWireShape(ingress.WireShape) == typology.EndpointKindEmbeddings {
		return h.deps.CanonicalBridge.ResponseCanonicalToIngressEmbeddings(ingress.BodyFormat, body)
	}
	return h.deps.CanonicalBridge.ResponseCanonicalToIngress(ingress.BodyFormat, body)
}

func (h *Handler) handleNonStream(r *http.Request, w http.ResponseWriter, rec *audit.Record, result *executor.ExecutionResult, target routingcore.RoutingTarget, forwardedBody []byte, quotaInPrice, quotaOutPrice float64, quotaDecision *quota.Decision, endpointType, requestID string, start time.Time, logger *slog.Logger) {
	respBody := result.Body
	ingress, _ := IngressFromContext(r.Context())
	// Reverse-decode the upstream's Responses-shape body back into
	// canonical chat-completions JSON before the standard ingress reshape
	// path runs. This is the inverse of the request-side
	// EncodeResponsesRequest applied earlier in the pipeline. On decode
	// failure, surface a 502 since the client expected chat-completions
	// shape; falling back to the raw Responses bytes would break SDKs.
	if ResponsesUpgradeFromContext(r.Context()) && len(respBody) > 0 {
		canonicalBody, _, dErr := openairesponses.DecodeResponsesResponse(respBody)
		if dErr != nil {
			logger.Error("reverse-decode of /v1/responses body failed",
				"error", dErr.Error())
			h.writeError(w, rec, http.StatusBadGateway,
				"upgraded /v1/responses body could not be reverse-decoded to chat-completions shape: "+dErr.Error())
			return
		}
		respBody = canonicalBody
	}
	// Response compliance runs on the CANONICAL body (the OpenAI-shape waist),
	// BEFORE egress reshape: redaction rewrites canonical, then the egress codec
	// forward-encodes it to the ingress wire shape — always supported, so the
	// reverse-encode ErrRewriteUnsupported / fail-closed path is gone.
	redacted, _, blocked := h.runResponseHooksOnCanonical(w, r, rec, ingress, target, respBody, int64(usageInt(result.Usage.TotalTokens)), requestID, logger)
	if blocked {
		return
	}
	respBody = redacted

	// Re-shape the (possibly redacted) canonical response into the caller's
	// ingress shape ("B→canonical→A"). respBody is canonical here, so the reshape
	// is keyed on the ingress shape — see egressReshapeNonStream for the contract.
	if shaped, rerr := h.egressReshapeNonStream(ingress, target, respBody); rerr != nil {
		logger.Error("response hub reshape failed", "error", rerr)
		h.writeError(w, rec, http.StatusBadGateway, "upstream response could not be reshaped for ingress format")
		return
	} else {
		respBody = shaped
	}
	// The reshaped wire body is the redacted, client-consistent copy under a
	// rewrite; hand it to the audit writer as the storage-safe redacted copy so
	// the persisted shape stays wire-shaped (StorageRawBody picks it under redact).
	if rec.ResponseHookRewritten {
		rec.ResponseBodyRedacted = respBody
	}

	usage := metrics.Usage{
		PromptTokens:     int64(usageInt(result.Usage.PromptTokens)),
		CompletionTokens: int64(usageInt(result.Usage.CompletionTokens)),
		TotalTokens:      int64(usageInt(result.Usage.TotalTokens)),
	}
	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	// Embeddings cost/usage fallback: some providers (e.g. Gemini
	// embedContent) return only the vector, no token usage. Back-fill
	// prompt_tokens from the request-side local estimate stamped at
	// ServeProxy time so the per-endpoint cost formula yields a non-zero
	// embedding cost. OpenAI/Azure embeddings report real usage, so this
	// only fires when usage is genuinely absent.
	embeddingEstimated := false
	if pt := embeddingTokenFallback(rec.EndpointType, rec.PromptTokens, rec.Metadata); pt != rec.PromptTokens {
		rec.PromptTokens = pt
		rec.TotalTokens = pt
		usage.PromptTokens = pt
		usage.TotalTokens = pt
		// The token count is a local heuristic estimate, not a
		// provider-reported figure — flag it so usage_extraction_status does
		// not claim "ok" confidence below.
		embeddingEstimated = true
	}
	// Use the per-endpoint cost formula registry so embeddings (prompt
	// only) and other typologies are priced correctly without a switch.
	{
		units := estimator.BillableUnits{
			PromptTokens:     int(rec.PromptTokens),
			CompletionTokens: int(rec.CompletionTokens),
		}
		// Multimodal units (image count / TTS chars) — the fallback the
		// per-kind formulas price when the provider reports no usage tokens.
		// TTS chars come from the FORWARDED request body (always in hand),
		// NOT rec.RequestBody, which is empty when payload capture's
		// storeRequestBody is off (the privacy-conscious default) — reading
		// it there would silently price every TTS request at $0.
		stampModalityUnits(&units, rec.EndpointType, forwardedBody, respBody)
		// Underivable-units guard: a multimodal 2xx that yields neither usage
		// tokens nor a modality unit prices at $0. Surface it (deduped per
		// endpoint) so a truncated / malformed response or a missing field is
		// visible instead of a silent zero — the WARN the cost formula
		// comments and the SDD promise the stamping site owns.
		if warnUnderivableModalityUnits(rec.EndpointType, result.StatusCode, units) {
			warnUnderivableUnitsOnce(logger, rec.EndpointType, target.ModelID, result.Truncated)
		}
		cost := estimator.Lookup(rec.EndpointType)(units, metrics.ModelPrices{
			InputUsdPerM:  &quotaInPrice,
			OutputUsdPerM: &quotaOutPrice,
		})
		rec.EstimatedCostUsd = cost.Total
	}
	// Artifact reference stamp (non-PII): fingerprint byte-bearing
	// artifacts over bytes the buffered non-stream response already holds;
	// URL-return images store the reference only (never dereferenced). The
	// content-type is the upstream response header (audio/mpeg for TTS),
	// never the JSON hint stamped later for the CP reader.
	if rec.ArtifactRefs == "" && result.StatusCode >= 200 && result.StatusCode < 300 {
		rec.ArtifactRefs = buildArtifactRefs(rec.EndpointType, respBody, result.Headers.Get("Content-Type"))
	}
	// Stamp ProviderCacheStatus from upstream usage cache fields.
	// Skip if already set (gateway-served paths stamp NA before reaching here).
	if rec.ProviderCacheStatus == "" {
		rec.ProviderCacheStatus = audit.ClassifyProviderCache(result.Usage.CacheReadTokens, result.Usage.CacheCreationTokens)
	}
	if result.Usage.CacheReadTokens != nil {
		rec.CacheReadTokens = int64(*result.Usage.CacheReadTokens)
	}
	if result.Usage.CacheCreationTokens != nil {
		rec.CacheCreationTokens = int64(*result.Usage.CacheCreationTokens)
	}
	// reasoning_tokens. Populated by codec packages when the provider
	// reports it (Gemini thoughtsTokenCount, OpenAI-compat
	// completion_tokens_details.reasoning_tokens, Anthropic
	// thinking_tokens). Absent = 0 → NULL via omitempty / mq schema.
	if result.Usage.ReasoningTokens != nil {
		rec.ReasoningTokens = int64(*result.Usage.ReasoningTokens)
	}
	// reasoning_cost_usd — subset of EstimatedCostUsd attributable to
	// ReasoningTokens (billed at the output rate). Stamped here BEFORE
	// computeCacheCosts which may recompute EstimatedCostUsd from the Model
	// row's price columns; the ratio is preserved because both numerator and
	// denominator use the same output price. Shared helper so every cost path
	// (broker, streaming, cache HIT) stamps it identically.
	stampReasoningCost(rec, quotaOutPrice)
	h.computeCacheCosts(rec, target)
	switch {
	case embeddingEstimated:
		// Tokens came from the local embedding estimate, not the
		// provider — distinct status so dashboards/alerting don't treat the
		// count as provider-confirmed. The estimate is request-side, so it is
		// honest even when the response body was truncated (checked next).
		rec.UsageExtractionStatus = "estimated"
	case result.Truncated:
		// The upstream response body was clamped at the read cap (or
		// decompressed-size bound) before usage extraction, so any token
		// counts parsed from it are incomplete. Refuse to claim "ok" — a
		// truncated-as-complete buffer silently corrupts billing/analytics.
		// Also mark ResponseTruncated so the spilled audit body is tagged.
		rec.ResponseTruncated = true
		rec.UsageExtractionStatus = "truncated"
	case usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0:
		rec.UsageExtractionStatus = "ok"
	default:
		rec.UsageExtractionStatus = "parse_failed"
	}
	// Update embedding dimension from the live response body.
	// Request-side fields were pre-stamped in ServeProxy; only the
	// response dimension is available here.
	if rec.EndpointType == "embeddings" {
		rec.Metadata = updateEmbeddingDimension(rec.Metadata, respBody)
	}

	// Capture after response hooks so payload mirrors bytes returned to the
	// client. The full body is handed to the audit Writer, which routes it
	// inline (≤ MaxInlineBodyBytes) or to the spill backend (>) at flush time.
	// Network-side bounding for the upstream read happens independently in
	// provcore.LimitedReadAll using MaxResponseBytes. respBody is the reshaped
	// wire body (redacted under a rewrite); StorageRawBody persists
	// ResponseBodyRedacted under redact/block and this captured copy otherwise.
	respBodyForAudit := respBody
	pcCfgPost := h.payloadCaptureConfig()
	if len(respBodyForAudit) > 0 && pcCfgPost.StoreResponseBody {
		rec.ResponseBody = respBodyForAudit
		// Non-stream AI Gateway responses are always JSON-shaped after
		// the canonical bridge; this hint drives the Control Plane
		// reader's inline-vs-string decoding.
		rec.ResponseContentType = "application/json"
	}

	rec.StatusCode = http.StatusOK

	// Record metrics.
	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordRequest(target.ProviderName, target.ModelID, endpointType, rec.StatusCode, time.Since(start), usage)
	}

	// Quota reconcile — new engine (fire-and-forget). Charge the single
	// canonical cache-aware cost the cost pipeline already computed
	// (rec.EstimatedCostUsd = traffic_event.estimated_cost_usd = the rollup's
	// billed_cost_usd base = the Backfill seed) so the live counter, the
	// rollup, and the boot seed price identically. Captured into
	// a local before the goroutine so reconcile cannot race a later rec mutation.
	reconcileCost := rec.EstimatedCostUsd
	if h.deps.QuotaEngine != nil && quotaDecision != nil && quotaDecision.Allowed && rec.StatusCode < 400 {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					h.deps.Logger.Error("quota engine reconcile panic", "panic", r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			h.deps.QuotaEngine.Reconcile(ctx, quotaDecision, quota.ActualUsage{CostUSD: reconcileCost})
		}()
	}

	// Cache writes are owned by the broker on the MISS path
	// (streamcache.Broker.writeCache). The direct path runs when the
	// cache is disabled or no broker is wired, so there is nothing to
	// persist here.

	// This body is written only after the upstream call returned, which for a
	// reasoning model is minutes after the request header was read and armed
	// the flat server.writeTimeout. Lift the deadline to the upstream budget
	// first or the write is severed and the client loses a completed (and
	// already billed) inference. Mirrors the Responses-API path.
	extendWriteDeadlineToUpstreamBudget(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}
