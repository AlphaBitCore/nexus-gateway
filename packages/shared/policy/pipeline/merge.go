package pipeline

// merge.go — result aggregation: folding the per-hook results into one
// CompliancePipelineResult (decision strictness merge, tag union, redaction
// artifact carry + applicability flag). Split out of pipeline.go along the
// aggregation seam.

import (
	"sort"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

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
	// redactionApplicable drives CarriesRedaction()'s BlockSoft branch: an enforcing hook
	// contributed an artifact the AGGREGATE carries. ASYMMETRIC — the merge carries
	// ModifiedContent only from Modify hooks, so a Modify hook qualifies on ModifiedContent
	// OR an applicable span, a BlockSoft hook on an applicable span ONLY. Advisory
	// Approve/Abstain spans (an approve-webhook's audit-only sentinel) never qualify.
	redactionApplicable := false
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
			// Applicable only via spans — the merge does NOT carry a BlockSoft
			// hook's ModifiedContent into the aggregate.
			if !redactionApplicable && anyApplicableSpan(r.TransformSpans) {
				redactionApplicable = true
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
			// Applicable via the carried ModifiedContent OR an applicable span.
			if !redactionApplicable && (len(r.ModifiedContent) > 0 || anyApplicableSpan(r.TransformSpans)) {
				redactionApplicable = true
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
	// Carry the redaction payload UNCONDITIONALLY, symmetric with TransformSpans above.
	// When a redact hook (Modify + ModifiedContent + spans) co-fires with a soft-block
	// hook, StrictestDecision promotes the aggregate Decision to BlockSoft; previously
	// ModifiedContent was set only in the `else if hasModify` branch and was therefore
	// DROPPED, leaving spans without their replacement content. Downstream consumers
	// then could not apply the redaction and fell back to fail-closed (block) OR
	// replay-original (leak), depending on the path. Carrying it here lets every
	// consumer that keys on CarriesRedaction (not Decision==Modify) redact-and-deliver.
	// lastModifiedContent is non-nil only when a Modify hook captured content; Approve
	// and RejectHard (which short-circuits above) never populate it, so no non-redact
	// decision gains content.
	pr.ModifiedContent = lastModifiedContent

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
	}
	pr.RedactionApplicable = redactionApplicable
	pr.Action = core.ActionFromDecision(pr.Decision)
	return pr
}

// anyApplicableSpan reports whether any span carries an APPLICABLE redaction — one whose
// ContentAddress is not an audit-only sentinel (a DENYLIST: an unknown/future address counts
// as applicable, the fail-safe over-block-never-leak direction). Short-circuits on first hit.
func anyApplicableSpan(spans []normalize.TransformSpan) bool {
	for i := range spans {
		if !normalize.IsAuditOnlySentinelAddress(spans[i].ContentAddress) {
			return true
		}
	}
	return false
}
