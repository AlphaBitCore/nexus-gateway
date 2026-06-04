package proxy

import (
	"context"
	"log/slog"
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/rulepack"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// proxy_hooks.go holds the compliance-hook trace / content-extraction / outcome
// helpers + the audit tag-set + blocking-rule mappers split out of proxy.go (behavior
// unchanged). The orchestrator runRequestHooks stays in proxy.go with the request flow.

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
			Error:      r.Error,
		})
	}
	return out
}

// extractRequestContentForHooks pulls the canonical request content
// blocks out of the ingress body via the format-aware traffic
// adapter. Failures here are non-fatal — hook input is best-effort
// and the pipeline is allowed to run with partial or empty data.
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
		logger.Debug("request extract for hooks failed",
			slog.String("adapter", adapter.ID()),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "error")
		}
		return nil
	}
	if h != nil && h.deps != nil && h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "request", "success")
	}
	return hookcore.PayloadFromTextSegments(extracted.Segments)
}

// extractResponseForHooks pulls the canonical content blocks, model
// name, and finish reason out of a non-streaming response body via the
// active traffic adapter. Failures here are non-fatal — hook input is
// best-effort and the pipeline is allowed to run with partial data.
func (h *Handler) extractResponseForHooks(ctx context.Context, adapter traffic.Adapter, ingressFormat string, body []byte, path string, logger *slog.Logger) (*normcore.NormalizedPayload, string, string) {
	if adapter == nil || len(body) == 0 {
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "skipped")
		}
		return nil, "", ""
	}
	extracted, err := adapter.ExtractResponse(ctx, body, path)
	if err != nil {
		logger.Debug("response extract for hooks failed",
			slog.String("adapter", adapter.ID()),
			slog.String("path", path),
			slog.String("error", err.Error()),
		)
		if h != nil && h.deps != nil && h.deps.Metrics != nil {
			h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "error")
		}
		return nil, "", ""
	}
	if h != nil && h.deps != nil && h.deps.Metrics != nil {
		h.deps.Metrics.RecordTrafficExtract(ingressFormat, "response", "success")
	}
	model := ""
	if extracted.Metadata != nil {
		model = extracted.Metadata["model"]
	}
	return hookcore.PayloadFromTextSegments(extracted.Segments), model, ""
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
//   - RejectHard / BlockSoft → Rejected = hookName, RejectReason = reasonCode (or reason)
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
		case hookcore.RejectHard, hookcore.BlockSoft:
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
