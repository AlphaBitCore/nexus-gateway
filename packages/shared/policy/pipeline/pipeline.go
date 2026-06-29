package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// safeHookExecute calls hook.Execute and converts a panic into an error so
// the fail-policy decides what to do. Without this guard a single panicking
// third-party hook would crash the entire data-plane process.
func safeHookExecute(ctx context.Context, h core.Hook, input *core.HookInput) (result *core.HookResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("hook panic: %v", r)
			result = nil
		}
	}()
	return h.Execute(ctx, input)
}

// boundHook pairs a Hook instance with its declarative configuration.
type boundHook struct {
	hook   core.Hook
	config *core.HookConfig
}

// Pipeline executes hooks in priority order with timeout and fail behavior.
type Pipeline struct {
	hooks              []boundHook
	perHookTimeout     time.Duration
	totalTimeout       time.Duration
	parallel           bool // true for compliance-proxy (no MODIFY => hooks independent)
	allowModify        bool // when true, MODIFY decisions pass through (for ai-gateway)
	clearSoftOnApprove bool // when true, APPROVE clears pending BLOCK_SOFT (for ai-gateway)
	logger             *slog.Logger

	// strictFailClosed is the per-service fail posture threaded from BuildPipeline
	// (the same flag that makes an UNBUILDABLE fail-closed hook refuse rather than
	// skip). When true, a hook that ERRORS/TIMES-OUT/PANICS and is ENFORCING (its
	// onMatch action is redact or block) FAILS CLOSED even when its FailBehavior
	// was never seeded "fail-closed" — a transient error on an enforcing hook
	// breaks the guaranteed-execution contract, so on the non-packet path it must
	// reject rather than leak. false (the host-network packet-path callers: agent
	// NE proxy / tlsbump / compliance packet path) preserves availability-first
	// fail-open — closing those paths would take down host DNS/DHCP/outbound.
	strictFailClosed bool

	// unionPrescan, when non-nil, is a single matcher folding every content
	// hook's anchor-stripped prefilter (built+cached per resolved hook set by the
	// PolicyResolver). MayMatchRawContent scans it ONCE instead of looping one
	// cgo scan per hook. nil => use the per-hook loop. See unionprescan.go.
	unionPrescan matcher.Matcher
}

// SetAllowModify enables MODIFY decision passthrough (for ai-gateway).
// When false (default), MODIFY is downgraded to APPROVE.
func (p *Pipeline) SetAllowModify(allow bool) {
	p.allowModify = allow
}

// SetClearSoftOnApprove makes APPROVE clear any pending BLOCK_SOFT.
// When false (default), any BLOCK_SOFT is sticky.
func (p *Pipeline) SetClearSoftOnApprove(clear bool) {
	p.clearSoftOnApprove = clear
}

// NewPipeline creates a pipeline from bound core. Hooks are sorted by ascending
// priority (lower number = higher priority = runs first).
func NewPipeline(hooks []boundHook, perHookTimeout, totalTimeout time.Duration, parallel bool, logger *slog.Logger) *Pipeline {
	// hooks are already sorted by priority in PolicyResolver.resolve().
	// Make a defensive copy without re-sorting.
	sorted := make([]boundHook, len(hooks))
	copy(sorted, hooks)

	if perHookTimeout <= 0 {
		perHookTimeout = 5 * time.Second
	}
	if totalTimeout <= 0 {
		totalTimeout = 30 * time.Second
	}

	return &Pipeline{
		hooks:          sorted,
		perHookTimeout: perHookTimeout,
		totalTimeout:   totalTimeout,
		parallel:       parallel,
		logger:         logger,
	}
}

// MayMatchRawContent reports whether any content-scanning hook in this pipeline
// could match the raw request body — a cheap prefilter the caller uses to decide
// whether the expensive structured extraction can be skipped.
//
// It returns false ONLY when every hook is accounted for AND none can match:
//   - a hook implementing core.RawContentPrescanner with ScansContent()==true
//     contributes its MayMatchRaw(body) verdict (an anchor-stripped superset
//     scan over the raw bytes — see the interface's soundness contract);
//   - a content-independent hook (ScansContent()==false: rate limit, IP, size)
//     never forces extraction;
//   - ANY hook that does not implement the interface forces true, because the
//     caller cannot prove that hook would find nothing without extracting.
//
// A false result is only safe to act on for bodies with no JSON backslash
// escape (so the extracted content appears verbatim in the raw bytes); enforcing
// that precondition is the caller's responsibility.
func (p *Pipeline) MayMatchRawContent(body []byte) bool {
	// Fast path: one shared scan over the folded prefilter. The union is attached
	// ONLY when it provably represents every hook this loop would consult — every
	// hook is a RawContentPrescanner (no unaccounted hook forcing true) and every
	// content-scanning hook contributed its anchor-stripped patterns
	// (buildUnionPrescan returns ok=false otherwise). So union-no-match is
	// equivalent to the loop finding no match: same skip-extraction decision, one
	// cgo scan instead of one per hook.
	if p.unionPrescan != nil {
		if len(body) == 0 {
			return false
		}
		return len(p.unionPrescan.Scan([]string{bytesView(body)}, true)) > 0
	}
	for i := range p.hooks {
		pre, ok := p.hooks[i].hook.(core.RawContentPrescanner)
		if !ok {
			return true
		}
		if pre.ScansContent() && pre.MayMatchRaw(body) {
			return true
		}
	}
	return false
}

// Execute runs all hooks and returns the aggregated result. If parallel=true,
// hooks run concurrently; otherwise they run sequentially with short-circuit
// on REJECT_HARD.
func (p *Pipeline) Execute(ctx context.Context, input *core.HookInput) *core.CompliancePipelineResult {
	start := time.Now()
	defer func() {
		PipelineDuration.Observe(time.Since(start).Seconds())
	}()

	totalCtx, totalCancel := context.WithTimeout(ctx, p.totalTimeout)
	defer totalCancel()

	var results []core.HookResult
	if p.parallel {
		results = p.executeParallel(totalCtx, input)
	} else {
		results = p.executeSequential(totalCtx, input)
	}

	merged := p.mergeResults(results)
	PipelineDecisionTotal.WithLabelValues(string(merged.Decision)).Inc()
	return merged
}

// executeParallel runs all hooks concurrently and collects results.
// Cancels remaining hooks via context when a RejectHard is observed.
func (p *Pipeline) executeParallel(ctx context.Context, input *core.HookInput) []core.HookResult {
	pCtx, pCancel := context.WithCancel(ctx)
	defer pCancel()

	var mu sync.Mutex
	results := make([]core.HookResult, 0, len(p.hooks))
	var wg sync.WaitGroup

	for i := range p.hooks {
		bh := &p.hooks[i]
		if !hookAppliesToKind(bh, input) {
			continue
		}
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			hr := p.executeOneHook(pCtx, bh, input)
			hr.Order = idx
			mu.Lock()
			results = append(results, hr)
			if hr.Decision == core.RejectHard {
				pCancel()
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return results
}

// executeSequential runs hooks in priority order, short-circuiting on REJECT_HARD.
func (p *Pipeline) executeSequential(ctx context.Context, input *core.HookInput) []core.HookResult {
	results := make([]core.HookResult, 0, len(p.hooks))
	for i := range p.hooks {
		bh := &p.hooks[i]
		if !hookAppliesToKind(bh, input) {
			// Skipped per applicableTrafficKinds — do not append to results
			// so the audit row does not show a phantom hook execution.
			continue
		}
		hr := p.executeOneHook(ctx, bh, input)
		hr.Order = i
		results = append(results, hr)
		if hr.Decision == core.RejectHard {
			break
		}
		// When the hook emitted TransformSpans (or the transitional
		// ModifiedContent), apply them so subsequent hooks see the
		// redacted version. Prefer TransformSpan over ModifiedContent.
		if hr.Decision == core.Modify {
			if len(hr.TransformSpans) > 0 && input.Normalized != nil {
				patched, _ := normalize.ApplySpans(*input.Normalized, hr.TransformSpans)
				input.Normalized = &patched
			} else if len(hr.ModifiedContent) > 0 {
				input.Normalized = applyModifiedContentToNormalized(input.Normalized, hr.ModifiedContent)
			}
		}
		// Accumulate tags so the next hook observes the full upstream set.
		// Parallel executor intentionally does NOT do this — parallel pipelines
		// run independently and cannot share upstream state.
		if len(hr.Tags) > 0 {
			input.UpstreamTags = mergeSortedDedup(input.UpstreamTags, hr.Tags)
		}
	}
	return results
}

// mergeSortedDedup returns the sorted, deduplicated union of a and b.
// Both inputs may contain duplicates or be unsorted. Safe for small
// tag sets; O((m+n) log (m+n)).
func mergeSortedDedup(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	for _, s := range a {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	for _, s := range b {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// hookAppliesToKind reports whether bh should run against the kind in
// input.Normalized. HookConfig.ApplicableTrafficKinds defaults to ["ai"]
// when nil/empty, so content-touching hooks run only on AI traffic unless
// explicitly broadened (e.g. ["ai", "http-json"]).
//
// A nil Normalized payload (connection-stage hooks, empty captures) is
// treated as "any kind". Content-scanning hooks handle the nil case
// themselves and ABSTAIN naturally.
func hookAppliesToKind(bh *boundHook, input *core.HookInput) bool {
	if input == nil || input.Normalized == nil {
		return true
	}
	kinds := bh.config.ApplicableTrafficKinds
	if len(kinds) == 0 {
		kinds = []string{"ai"}
	}
	payloadKind := string(input.Normalized.Kind)
	for _, k := range kinds {
		if k == "all" || k == "*" {
			return true
		}
		if k == payloadKind {
			return true
		}
		// "ai" matches any ai-* kind; "http" matches any http-* kind.
		if k == "ai" && input.Normalized.Kind.IsAI() {
			return true
		}
		if k == "http" && input.Normalized.Kind.IsHTTP() {
			return true
		}
	}
	return false
}

// executeOneHook runs a single hook with per-hook timeout and fail behavior handling.
//
// Hook implementations are shipped by third parties (rulepack-engine,
// content-safety calling out to remote AI guard, custom rules, etc.) and
// process arbitrary user input, so a buggy hook panicking here would
// otherwise crash the entire data-plane process. We wrap Execute in a
// recover and translate any panic into a normal error so the fail-policy
// (fail_open / fail_closed) can decide what to do — exactly the same as
// for an Execute returning an error.
func (p *Pipeline) executeOneHook(ctx context.Context, bh *boundHook, input *core.HookInput) core.HookResult {
	timeout := p.perHookTimeout
	if bh.config.TimeoutMs > 0 {
		timeout = time.Duration(bh.config.TimeoutMs) * time.Millisecond
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	result, err := safeHookExecute(hookCtx, bh.hook, input)
	elapsed := time.Since(start)

	hookName := bh.config.Name
	if hookName == "" {
		hookName = bh.config.ImplementationID
	}

	HookDuration.WithLabelValues(hookName).Observe(elapsed.Seconds())

	if err != nil {
		HookErrorTotal.WithLabelValues(hookName).Inc()

		if hookCtx.Err() == context.DeadlineExceeded {
			HookTimeoutTotal.WithLabelValues(hookName).Inc()
		}

		p.logger.Warn("compliance hook error",
			"hook", hookName,
			"hookId", bh.config.ID,
			"error", err,
			"failBehavior", bh.config.FailBehavior,
			"elapsed_ms", elapsed.Milliseconds(),
		)

		hr := core.HookResult{
			HookID:           bh.config.ID,
			ImplementationID: bh.config.ImplementationID,
			HookName:         hookName,
			LatencyMs:        int(elapsed.Milliseconds()),
			Error:            err.Error(),
		}

		// Fail-posture on hook ERROR / TIMEOUT / PANIC. See failClosedOnError
		// (enforcement.go) for the precedence: explicit fail-closed/fail-open
		// win; otherwise a strict (non-packet-path) caller fails an ENFORCING
		// hook closed so a transient error cannot leak PII/secrets on the very
		// rule meant to stop them, while packet-path callers stay fail-open.
		if failClosedOnError(p.strictFailClosed, bh.config) {
			hr.Decision = core.RejectHard
			hr.Reason = fmt.Sprintf("hook error (fail-closed): %v", err)
			hr.ReasonCode = "HOOK_ERROR_FAIL_CLOSED"
		} else {
			// Fail-open (availability-first; see comment above).
			// A sustained nonzero rate on this counter means a hook is silently
			// degraded — the traffic path is unprotected by that hook while the
			// gateway keeps serving. Operators alert on hook_fail_open_total.
			hr.Decision = core.Approve
			hr.Reason = fmt.Sprintf("hook error (fail-open): %v", err)
			hr.ReasonCode = "HOOK_ERROR_FAIL_OPEN"
			HookFailOpenTotal.WithLabelValues(hookName).Inc()
		}
		HookDecisionTotal.WithLabelValues(hookName, string(hr.Decision)).Inc()
		return hr
	}

	if result == nil {
		result = &core.HookResult{
			Decision: core.Abstain,
		}
	}

	// Fill in metadata if the hook didn't set them.
	result.HookID = bh.config.ID
	result.ImplementationID = bh.config.ImplementationID
	if result.HookName == "" {
		result.HookName = hookName
	}
	result.LatencyMs = int(elapsed.Milliseconds())

	// MODIFY passes through unconditionally. The downstream caller applies
	// TransformSpans via TrafficAdapter.RewriteRequestBody; protocols that
	// cannot reverse-encode return ErrRewriteUnsupported, which the caller
	// maps to redacted-storage with ReasonRedactInflightUnsupported.

	// Stamp the hook's action from its decision when the hook matched but did
	// not set one explicitly (rulepack/webhook set it; the simple detectors
	// rely on this fallback). The pipeline aggregates the strictest action.
	if result.Decision != core.Approve && result.Action == "" {
		result.Action = core.ActionFromDecision(result.Decision)
	}

	HookDecisionTotal.WithLabelValues(hookName, string(result.Decision)).Inc()
	return *result
}

// mergeResults aggregates individual hook results into a single pipeline result.
//
// Decision merging:
//   - First REJECT_HARD wins overall
//   - Any BLOCK_SOFT (with no REJECT_HARD) => BLOCK_SOFT
//   - All APPROVE/ABSTAIN => APPROVE
//
// "First" is by hook priority (HookResult.Order), not by arrival order.
// executeParallel appends results in goroutine-completion order, so the
// raw slice can be in any order; sort up front by Order so the BLOCK_SOFT
// / Modify Reason+ReasonCode tie-breaks are deterministic across runs.
//
// Tags: union of all hook-emitted tags, sorted alphabetically and deduplicated.
func (p *Pipeline) mergeResults(results []core.HookResult) *core.CompliancePipelineResult {
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Order < results[j].Order
	})

	pr := &core.CompliancePipelineResult{
		Decision:    core.Approve,
		HookResults: results,
	}

	// Merge tags from every executed hook (set union, sorted, deduped) up front,
	// so tags from earlier hooks survive even if a later hook short-circuits
	// the decision loop below via REJECT_HARD.
	tagSet := make(map[string]struct{})
	for i := range results {
		for _, tag := range results[i].Tags {
			if tag == "" {
				continue
			}
			tagSet[tag] = struct{}{}
		}
	}
	merged := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		merged = append(merged, tag)
	}
	sort.Strings(merged)
	pr.Tags = merged

	hasSoftReject := false
	var softRejectReason, softRejectCode string
	var softBlockingRule *core.BlockingRule
	hasModify := false
	// First Modify hook's Reason / ReasonCode wins so a specific
	// reason (e.g. ReasonAIGuardSuggestedVsPolicy stamped at the
	// webhook-forward reconcile) propagates to CompliancePipelineResult
	// instead of being clobbered by the generic "CONTENT_MODIFIED" default.
	var modifyReason, modifyReasonCode string
	var lastModifiedContent []core.ContentBlock
	var allSpans []normalize.TransformSpan

	for i := range results {
		r := &results[i]
		// Aggregate spans from every hook regardless of terminal decision —
		// even Approve hooks may emit informational transforms (e.g.
		// cache-normaliser strips through a hook integration). Storage
		// rewrite at the audit-write stage walks this aggregate.
		if len(r.TransformSpans) > 0 {
			allSpans = append(allSpans, r.TransformSpans...)
		}

		switch r.Decision {
		case core.RejectHard:
			pr.Decision = core.RejectHard
			pr.Reason = r.Reason
			pr.ReasonCode = r.ReasonCode
			pr.BlockingRule = r.BlockingRule
			pr.TransformSpans = allSpans
			pr.Action = core.ActionFromDecision(core.RejectHard)
			return pr
		case core.BlockSoft:
			hasSoftReject = true
			softRejectReason = r.Reason
			softRejectCode = r.ReasonCode
			if softBlockingRule == nil {
				softBlockingRule = r.BlockingRule
			}
		case core.Modify:
			if !hasModify {
				// First Modify hook's reason wins so a hook-stamped
				// ReasonCode (e.g. ReasonAIGuardSuggestedVsPolicy) is not
				// silently replaced by the generic "CONTENT_MODIFIED".
				modifyReason = r.Reason
				modifyReasonCode = r.ReasonCode
			}
			hasModify = true
			if len(r.ModifiedContent) > 0 {
				lastModifiedContent = r.ModifiedContent
			}
		case core.Approve:
			if p.clearSoftOnApprove {
				hasSoftReject = false
				softRejectReason = ""
				softRejectCode = ""
				softBlockingRule = nil
			}
		}
	}

	pr.TransformSpans = allSpans

	if hasSoftReject {
		pr.Decision = core.BlockSoft
		pr.Reason = softRejectReason
		pr.ReasonCode = softRejectCode
		pr.BlockingRule = softBlockingRule
	} else if hasModify {
		pr.Decision = core.Modify
		if modifyReason != "" {
			pr.Reason = modifyReason
		} else {
			pr.Reason = "content modified by hook pipeline"
		}
		if modifyReasonCode != "" {
			pr.ReasonCode = modifyReasonCode
		} else {
			pr.ReasonCode = "CONTENT_MODIFIED"
		}
		pr.ModifiedContent = lastModifiedContent
	}
	pr.Action = core.ActionFromDecision(pr.Decision)
	return pr
}
