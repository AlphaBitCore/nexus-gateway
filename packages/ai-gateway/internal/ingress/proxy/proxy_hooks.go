package proxy

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// proxy_hooks.go holds the compliance-hook trace / content-extraction / outcome
// helpers + the audit tag-set + blocking-rule mappers split out of proxy.go (behavior
// unchanged). The orchestrator runRequestHooks stays in proxy.go with the request flow.

// hookKey identifies a hook across repeated response-stage scans of one stream
// (live checkpoints / Model A confirms). Keyed by stable identity — NOT the
// per-scan slice index Order — so a hook is never split into two rows if the
// per-checkpoint pipeline rebuild ever shifts ordering.
type hookKey struct {
	hookID string
	implID string
}

// responseHookAccumulator folds the per-hook results of multiple response-stage
// scans into ONE record per hook: LatencyMs/LatencyUs are SUMMED (the real scan
// CPU spent across the stream) and decision/reason/order are overwritten by the
// latest scan (the last checkpoint that ran is authoritative). The streaming
// response pipeline runs once per checkpoint/confirm, so appending each scan's
// results directly would write a hook N times and N×-inflate the response hook
// aggregates; this accumulator is the single-append guard. Single-writer (the
// live checkpoint runs on the relay's main goroutine; Model A on its one loop
// goroutine) so it needs no lock, and the map is lazily allocated so a stream
// that produces no response-hook results pays nothing.
type responseHookAccumulator struct {
	order []hookKey
	byKey map[hookKey]hookcore.HookResult
}

func (a *responseHookAccumulator) add(results []hookcore.HookResult) {
	if len(results) == 0 {
		return
	}
	if a.byKey == nil {
		a.byKey = make(map[hookKey]hookcore.HookResult, len(results))
	}
	for _, r := range results {
		k := hookKey{hookID: r.HookID, implID: r.ImplementationID}
		if cur, ok := a.byKey[k]; ok {
			// Latest scan is authoritative for ALL fields; only the two latency
			// counters accumulate. Copying the whole struct (rather than a chosen
			// subset) keeps any future-read field — Tags, Action, BlockingRule — in
			// sync with the latest scan instead of serving a stale first-scan value.
			r.LatencyMs += cur.LatencyMs
			r.LatencyUs += cur.LatencyUs
			a.byKey[k] = r
		} else {
			a.byKey[k] = r
			a.order = append(a.order, k)
		}
	}
}

// finalize returns the folded per-hook results in first-seen order, or nil when
// nothing was accumulated (so appendHookTrace is a no-op).
func (a *responseHookAccumulator) finalize() []hookcore.HookResult {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]hookcore.HookResult, 0, len(a.order))
	for _, k := range a.order {
		out = append(out, a.byKey[k])
	}
	return out
}

func appendHookTrace(existing []audit.HookExecRecord, stage string, results []hookcore.HookResult) []audit.HookExecRecord {
	if len(results) == 0 {
		return existing
	}
	out := existing
	for _, r := range results {
		out = append(out, audit.HookExecRecord{
			Stage:      stage,
			Order:      r.Order,
			HookID:     r.HookID,
			Name:       r.HookName,
			Decision:   string(r.Decision),
			Reason:     r.Reason,
			ReasonCode: r.ReasonCode,
			LatencyMs:  r.LatencyMs,
			LatencyUs:  r.LatencyUs,
			Error:      r.Error,
		})
	}
	return out
}

// extractRequestContentForHooks pulls request content blocks out of the ingress
// body via the format-aware traffic adapter.
//
// It is no longer the chat lane's extractor — canonicalRequestForHooks is, and
// it hands hooks the canonical spec. This one serves the two cases that lane
// does not: endpoint kinds the canonical CHAT shape does not model (embeddings,
// images, rerank, audio, video), and a chat body the chat codecs cannot read.
// Its output model is flat — `Segments []string` — so it cannot represent a tool
// result, a reasoning turn, or a refusal at all. That is a real limitation and
// the reason the chat lane moved off it; keeping it here is deliberate, because
// the alternative for those cases is no scan whatsoever.
//
// Failures here are non-fatal — hook input is best-effort and the pipeline is
// allowed to run with partial or empty data.
//
// The returned blocks are all text segments in the adapter's
// extraction order. Role is left empty because NormalizedContent does
// not carry role information; the hook layer treats role-less blocks
// as caller input, which is the correct behaviour for request-stage
// hooks across all 9 formats.
func (h *Handler) extractRequestContentForHooks(ctx context.Context, adapter traffic.Adapter, ingressFormat string, body []byte, path string, logger *slog.Logger) *normcore.NormalizedPayload {
	if adapter == nil || len(body) == 0 {
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "skipped")
		}
		return nil
	}
	extracted, err := adapter.ExtractRequest(ctx, body, path)
	if err != nil {
		logExtractFailure(logger, "request", adapter.ID(), path, len(body), err)
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "error")
		}
		return nil
	}
	if h != nil && h.deps != nil && h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "success")
	}
	// PayloadFromExtracted (not PayloadFromTextSegments) so assistant-history
	// tool-call arguments echoed in the request enter the redaction pipeline.
	return hookcore.PayloadFromExtracted(extracted.Segments, extracted.ToolCallSegments)
}

// extractResponseForHooks decodes a CANONICAL response body into the canonical
// payload the hooks scan, plus the model name and finish reason.
//
// The body reaching this function is already canonical — the caller
// canonicalized it. Decoding it with the normalize registry is what makes the
// hook input the canonical spec rather than a second, weaker shape.
//
// It used to run the canonical body back through the format-aware TRAFFIC
// adapter, whose output model is flat: `Segments []string` plus a
// `ReasoningSegments` list nothing read. Rebuilt through PayloadFromExtracted
// that yields only ContentText and ContentToolUse, so reasoning and refusal —
// both delivered to the caller — were invisible to every scanning hook on this
// service, while the compliance-proxy, which decodes properly, saw them. The
// canonical structure was being discarded one line after it was obtained.
//
// A nil registry or a decode failure returns nil, which the pipeline reads as
// "no content available": content hooks abstain, metadata hooks are unaffected.
// There is deliberately no fallback to the flat model — that fallback IS the
// defect, and a quiet downgrade to a shape that cannot represent two delivered
// channels is worse than a hook that abstains visibly.
func (h *Handler) extractResponseForHooks(ctx context.Context, ingressFormat string, body []byte, path string, logger *slog.Logger) (*normcore.NormalizedPayload, string, string) {
	if h == nil || h.deps == nil || h.deps.NormalizeRegistry == nil || len(body) == 0 {
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "skipped")
		}
		return nil, "", ""
	}
	payload, err := h.deps.NormalizeRegistry.Normalize(ctx, body, normcore.Meta{
		AdapterType:  string(provcore.FormatOpenAI),
		Direction:    normcore.DirectionResponse,
		EndpointPath: path,
		ContentType:  "application/json",
	})
	// A decode error and a decode by the WRONG codec are the same outcome, and in
	// practice only the second happens: the registry sniffs, and its last resort
	// is the generic HTTP-JSON codec, which succeeds on any object. A body the
	// chat codec cannot read comes back as kind "http-json" with no messages — a
	// decode that succeeded while decoding nothing this lane can use. Accepting
	// it hands a hook something that is not the canonical spec, and worse, makes
	// it NON-NIL, so the fail-closed guard downstream (which keys on a nil
	// payload) reads the most dangerous case as a healthy one.
	if err == nil && payload.Protocol != openAIChatProtocol && payload.Protocol != openAIResponsesProtocol {
		err = fmt.Errorf("registry decoded the canonical response as %q, which is neither %q nor %q",
			payload.Protocol, openAIChatProtocol, openAIResponsesProtocol)
	}
	if err != nil {
		logExtractFailure(logger, "response", "canonical", path, len(body), err)
		if h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "error")
		}
		return nil, "", ""
	}
	if h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "success")
	}
	// Model and finish reason come off the canonical payload itself; the codec
	// already resolved the first choice's terminal reason, so there is no
	// comma-joined multi-choice string to split any more.
	return &payload, payload.Model, payload.FinishReason
}

// usageInt returns the pointer's dereferenced value, or 0 when nil.
func usageInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// aigwHookOutcomeFromResult converts a request-side CompliancePipelineResult
// into a HookOutcomeInput suitable for traffic.FormatHookOutcome. The mapping
// follows spec §4.5:
//   - RejectHard → Rejected = hookName, RejectReason = reasonCode (or reason)
//   - Modify → appended to Passed + Transformed = true
//   - Approve / Abstain → appended to Passed
//   - Any reject halts iteration (later hooks are not reported).
//
// Returns an empty HookOutcomeInput (→ "none") when r is nil or has no hook
// results.
func aigwHookOutcomeFromResult(r *hookcore.CompliancePipelineResult) traffic.HookOutcomeInput {
	if r == nil || len(r.HookResults) == 0 {
		return traffic.HookOutcomeInput{}
	}
	in := traffic.HookOutcomeInput{}
	for _, hr := range r.HookResults {
		switch hr.Decision {
		case hookcore.RejectHard:
			// Reject halts the pipeline: discard any previously-accumulated
			// Passed hooks and return only the reject attribution (spec §4.5).
			reason := hr.ReasonCode
			if reason == "" {
				reason = hr.Reason
			}
			return traffic.HookOutcomeInput{
				Rejected:     hr.HookName,
				RejectReason: reason,
			}
		case hookcore.Modify:
			in.Passed = append(in.Passed, hr.HookName)
			in.Transformed = true
		default:
			in.Passed = append(in.Passed, hr.HookName)
		}
	}
	return in
}

// mergeTagSets returns the sorted, deduplicated union of a and b. The audit
// record accumulates compliance tags across request- and response-stage hook
// pipelines, so the merger must be stable, deterministic (sorted output),
// and de-duplicating (the same tag emitted on both stages appears once).
// Callers supply the current rec.ComplianceTags as a and the freshly emitted
// hookResult.Tags as b; the result replaces rec.ComplianceTags.
func mergeTagSets(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, t := range a {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range b {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// mapBlockingRule narrows the hook-layer BlockingRule (which carries
// category/severity/labels for logging) to the JSONB audit shape that
// gets persisted on traffic_event.blocking_rule.
func mapBlockingRule(br *hookcore.BlockingRule) *rulepack.BlockingRule {
	if br == nil {
		return nil
	}
	return &rulepack.BlockingRule{
		Pack:        br.Pack,
		PackVersion: br.PackVersion,
		RuleID:      br.RuleID,
	}
}
