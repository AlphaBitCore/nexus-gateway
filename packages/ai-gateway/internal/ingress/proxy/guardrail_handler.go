package proxy

// guardrail_handler.go — the parallel compliance-verdict handler for
// POST /v1/guardrail. It runs the deployment's configured compliance pipeline
// (rule-pack + PII + AI-Guard judge) over caller-supplied text and returns an
// allow/block/redact verdict — WITHOUT relaying an LLM completion.
//
// Like the STT handler, guardrail does NOT go through the ServeProxy stage chain
// (no routing, no executor, no codec, no response cache). It reuses the shared
// admission subset (overload gate, VK auth, per-VK RPM, per-VK generative-caps
// concurrency) and the SAME compliance pipeline the inline path runs
// (resolver.BuildPipeline + Pipeline.Execute), then maps the aggregate to a JSON
// verdict via the pure internal/policy/guardrail package. Chat core
// (provcore.Request / executor / spec_adapter) is UNCHANGED.
//
// The endpoint ALWAYS returns 200 with the verdict — a block/redact disposition
// is DATA in the body (action=block), not an HTTP error, matching the
// ApplyGuardrail / Moderations category. The raw evaluated text is NEVER
// captured to the payload store (the input is the caller's sensitive material);
// the audit row records only the decision, tags, coverage, and reason.

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/middleware"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/generativecaps"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/policy/guardrail"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// guardrailMaxBytesDefault bounds the request body when the generative-caps row
// carries no explicit ceiling. 1 MiB matches the classify endpoint: a guardrail
// payload may be a full completion but is bounded (and the judge prompt is
// separately truncated by the classify path).
const guardrailMaxBytesDefault = 1 << 20

// ServeGuardrail returns the HTTP handler for POST /v1/guardrail.
func (h *Handler) ServeGuardrail() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// In-flight admission gate: reject-fast before any per-request setup.
		if h.gate != nil {
			if !h.gate.acquire() {
				writeOverloaded(w, provcore.FormatOpenAI)
				return
			}
			defer h.gate.release()
		}

		start := time.Now().UTC()
		requestID := r.Header.Get("X-Nexus-Request-Id")
		rec := &audit.Record{
			RequestID:       requestID,
			ClientRequestID: r.Header.Get("x-request-id"),
			TraceID:         requestID,
			Timestamp:       start,
			Method:          r.Method,
			Path:            r.URL.Path,
			SourceIP:        middleware.ClientIP(r),
			EndpointType:    string(typology.EndpointKindGuardrail),
		}

		// Owned, panic-safe audit tail: the generative-caps slot release and the
		// audit enqueue run on EVERY exit path (success, error, panic-unwind).
		var releaseCap func()
		defer func() {
			if releaseCap != nil {
				releaseCap()
			}
			if h.deps.AuditWriter != nil {
				h.finalize(rec, start)
			}
		}()

		// VK auth.
		vkMeta, err := h.authenticate(r)
		if err != nil {
			h.writeAuthError(w, rec, err)
			return
		}
		rec.ApplyVKMeta(vkMeta)

		// Per-VK RPM rate limit (sets Retry-After on rejection).
		if err := h.checkRateLimit(w, vkMeta); err != nil {
			h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "RATE_LIMITED",
				"per-key rate limit exceeded", "Reduce request rate or retry after the Retry-After interval")
			return
		}

		// Per-(VK, guardrail) generative-caps concurrency — the primary
		// judge-budget-exhaustion bound (v1 has no hard spend ceiling). Release
		// is wired into the panic-safe tail above.
		caps, capped := generativecaps.Lookup(typology.EndpointKindGuardrail)
		if capped && caps.MaxConcurrentPerVK > 0 && h.genConcurrency != nil {
			if !h.genConcurrency.Acquire(typology.EndpointKindGuardrail, vkMeta.ID, caps.MaxConcurrentPerVK) {
				generativeCapShedTotal.WithLabelValues(string(typology.EndpointKindGuardrail)).Inc()
				w.Header().Set("Retry-After", "1")
				h.writeDetailedErr(w, rec, http.StatusTooManyRequests, "GENERATIVE_CONCURRENCY_LIMIT",
					"too many concurrent guardrail requests for this key",
					"Reduce concurrency or retry shortly")
				return
			}
			vkID := vkMeta.ID
			releaseCap = func() { h.genConcurrency.Release(typology.EndpointKindGuardrail, vkID) }
		}

		// Bound + read the request body. Read explicitly (rather than decoding
		// straight off the reader) so a MaxBytesReader trip surfaces as a
		// *http.MaxBytesError that errors.As matches — the JSON decoder does not
		// reliably preserve that error type through its own wrapping.
		maxBytes := caps.MaxRequestBytes
		if maxBytes <= 0 {
			maxBytes = guardrailMaxBytesDefault
		}
		raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
		if readErr != nil {
			var mbe *http.MaxBytesError
			if errors.As(readErr, &mbe) {
				h.writeDetailedErr(w, rec, http.StatusRequestEntityTooLarge, "GUARDRAIL_BODY_TOO_LARGE",
					"request body exceeds the guardrail size ceiling", "Reduce the content size")
				return
			}
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "INVALID_REQUEST",
				"could not read the request body", "Retry the request")
			return
		}

		// Parse + validate (DisallowUnknownFields so a typo'd field 400s loudly).
		var req guardrail.Request
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if decErr := dec.Decode(&req); decErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "INVALID_REQUEST",
				"malformed JSON body: "+decErr.Error(), "Send a valid guardrail request body")
			return
		}
		stage, stageErr := guardrail.NormalizeStage(req.Stage)
		if stageErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "INVALID_REQUEST",
				stageErr.Error(), "Set stage to \"input\" or \"output\", or omit it (defaults to input)")
			return
		}
		if valErr := req.Validate(); valErr != nil {
			h.writeDetailedErr(w, rec, http.StatusBadRequest, "INVALID_REQUEST",
				valErr.Error(), "Provide exactly one of content or messages")
			return
		}
		segs := req.Segments()

		// Build the SAME pipeline the inline path runs, for the requested stage.
		pipelineStage := "request"
		if stage == guardrail.StageOutput {
			pipelineStage = "response"
		}
		resolver := h.deps.HookConfigCache.Resolver(r.Context())
		pl, buildErr := resolver.BuildPipeline(
			pipelineStage, "AI_GATEWAY",
			typology.EndpointKindGuardrail,
			[]hookcore.Modality{hookcore.ModalityText},
			5*time.Second, 15*time.Second, perfParallelHooks(), true, /* strictFailClosed */
			h.deps.Logger,
		)
		if buildErr != nil {
			h.writeDetailedErr(w, rec, http.StatusInternalServerError, "GUARDRAIL_PIPELINE_ERROR",
				"failed to build the compliance pipeline", "Retry; contact an operator if it persists")
			return
		}

		// Execute (nil pipeline = no hooks bound for this stage → nothing scanned).
		pipeStart := time.Now()
		var result *hookcore.CompliancePipelineResult
		if pl != nil {
			pl.SetAllowModify(true)
			pl.SetClearSoftOnApprove(true)
			result = pl.Execute(r.Context(), &hookcore.HookInput{
				RequestID:     requestID,
				Stage:         pipelineStage,
				Normalized:    guardrail.BuildNormalized(segs),
				IngressType:   "AI_GATEWAY",
				Method:        r.Method,
				Path:          r.URL.Path,
				EndpointType:  typology.EndpointKindGuardrail,
				InputModality: []hookcore.Modality{hookcore.ModalityText},
			})
		}
		latencyMs := int(time.Since(pipeStart).Milliseconds())

		coverage := guardrail.DeriveCoverage(result)
		resp := guardrail.MapResult(result, segs, coverage, req.WantRedactedContent(), latencyMs)

		// Stamp the audit row (decision / tags / coverage only — the raw evaluated
		// text is NEVER captured). StatusCode is 200: a block/redact verdict is a
		// successful evaluation, not an HTTP error.
		stampGuardrailAudit(rec, result, coverage, pipelineStage)

		writeGuardrailJSON(w, http.StatusOK, resp)
	}
}

// stampGuardrailAudit records the verdict onto the audit Record so guardrail
// decisions land in the same compliance dashboards as inline traffic. It sets
// NO request/response body — the sensitive evaluated text is not persisted.
// pipelineStage ("request" / "response") labels the hook-trace stage so an
// output-stage guardrail call is not mis-recorded as request-stage.
func stampGuardrailAudit(rec *audit.Record, result *hookcore.CompliancePipelineResult, coverage, pipelineStage string) {
	rec.StatusCode = http.StatusOK
	rec.ComplianceCoverage = coverage
	if result == nil {
		rec.HookDecision = string(hookcore.Approve)
		rec.RequestAction = hookcore.ActionFromDecision(hookcore.Approve)
		return
	}
	rec.HookDecision = string(result.Decision)
	rec.HookReason = result.Reason
	rec.HookReasonCode = result.ReasonCode
	rec.ComplianceTags = mergeTagSets(rec.ComplianceTags, result.Tags)
	rec.BlockingRule = mapBlockingRule(result.BlockingRule)
	rec.HooksPipeline = appendHookTrace(rec.HooksPipeline, pipelineStage, result.HookResults)
	rec.RequestAction = hookcore.ActionFromDecision(result.Decision)
}

// writeGuardrailJSON writes a JSON body with the given status.
func writeGuardrailJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
