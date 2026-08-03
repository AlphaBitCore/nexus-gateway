package guardrail

import (
	"strconv"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// joinSep is the separator used to join multi-message input into the flat
// evaluated text. It defines the single offset space that redactions[] and
// redacted_content both reference.
const joinSep = "\n"

// Segments returns the ordered text segments the pipeline evaluates: the single
// Content string, or each non-empty message's content in turn order.
//
// Empty ("") message contents are dropped so the redaction offset space stays
// consistent: the normalized payload keeps one content block per segment, but
// the text projection (JoinedText, used to build redacted_content) drops
// empty-text blocks — so an empty segment would contribute a "\n" separator to
// the flat span offsets (blockPrefix) while contributing nothing to
// redacted_content, diverging the two outputs. Dropping empties here makes the
// block index, the flat prefix sum, and the projection all use the same
// non-empty set. Validate() guarantees at least one non-empty segment survives.
func (r *Request) Segments() []string {
	if r.Content != "" {
		return []string{r.Content}
	}
	segs := make([]string, 0, len(r.Messages))
	for i := range r.Messages {
		if r.Messages[i].Content == "" {
			continue
		}
		segs = append(segs, r.Messages[i].Content)
	}
	return segs
}

// BuildNormalized constructs the HookInput.Normalized payload from the evaluated
// segments — one user message whose content blocks are the segments in order.
// Used by the handler (to feed Execute) and by MapResult (to rebuild for the
// redacted-content projection).
func BuildNormalized(segs []string) *normalize.NormalizedPayload {
	return hookcore.PayloadFromTextSegments(segs)
}

// DeriveCoverage reports the scan-completeness of a verdict. Any hook that
// errored (fail-open OR fail-closed) means the content was not cleanly scanned
// by that hook, so the verdict is "degraded" — critical honesty for a guardrail:
// the pipeline's fail-open branch returns Approve with an empty aggregate reason,
// so without this a judge-timeout allow would be indistinguishable from a clean
// allow. Callers must treat degraded as "do not trust this allow as a full scan".
func DeriveCoverage(result *decision.CompliancePipelineResult) string {
	if result == nil {
		return CoverageNone
	}
	for i := range result.HookResults {
		if result.HookResults[i].Error != "" {
			return CoverageDegraded
		}
	}
	return CoverageFull
}

// MapResult maps a pipeline aggregate to the wire verdict. segs is the evaluated
// text (Request.Segments()); coverage is DeriveCoverage's result (or CoverageNone
// for the no-pipeline case); wantRedacted echoes the sanitized text on a redact
// verdict; latencyMs is the handler-measured pipeline wall-clock.
func MapResult(result *decision.CompliancePipelineResult, segs []string, coverage string, wantRedacted bool, latencyMs int) Response {
	resp := Response{
		Coverage: coverage,
		Metadata: Metadata{LatencyMs: latencyMs},
	}
	if result == nil {
		// No compliance hooks bound for this stage: nothing was scanned.
		resp.Action = "allow"
		resp.Decision = "approve"
		return resp
	}

	resp.Action = actionFromDecision(result.Decision)
	resp.Decision = strings.ToLower(string(result.Decision))
	resp.Reason = result.Reason
	resp.ReasonCode = result.ReasonCode
	resp.Labels = result.Tags
	resp.Assessments = projectAssessments(result.HookResults)
	resp.Redactions = flattenSpans(result.TransformSpans, segs)

	if resp.Action == "block" && result.BlockingRule != nil {
		resp.Blocking = &Blocking{
			Category: result.BlockingRule.Category,
			Severity: result.BlockingRule.Severity,
			Labels:   result.BlockingRule.Labels,
		}
	}

	if resp.Action == "redact" && wantRedacted {
		resp.RedactedContent = buildRedactedContent(segs, result.TransformSpans)
	}
	return resp
}

// actionFromDecision maps the pipeline Decision to the caller-facing disposition
// (allow / block / redact). Mirrors hookcore.ActionFromDecision but emits the
// product vocabulary ("allow" not "approve").
func actionFromDecision(d decision.Decision) string {
	switch d {
	case decision.RejectHard, decision.BlockSoft:
		return "block"
	case decision.Modify:
		return "redact"
	}
	return "allow"
}

// projectAssessments returns one entry per hook that FLAGGED the content — a
// non-approve/abstain decision, emitted tags, an attributed rule, or an error.
// A benign request yields no assessments. This surfaces the per-policy
// breakdown (block-for-PII vs block-for-injection) the aggregate merges away.
func projectAssessments(hrs []decision.HookResult) []Assessment {
	var out []Assessment
	for i := range hrs {
		hr := &hrs[i]
		flagged := hr.Error != "" ||
			len(hr.Tags) > 0 ||
			hr.BlockingRule != nil ||
			(hr.Decision != decision.Approve && hr.Decision != decision.Abstain && hr.Decision != "")
		if !flagged {
			continue
		}
		out = append(out, Assessment{
			Check:      hr.HookName,
			Decision:   strings.ToLower(string(hr.Decision)),
			Action:     string(hr.Action),
			Labels:     hr.Tags,
			ReasonCode: hr.ReasonCode,
		})
	}
	return out
}

// flattenSpans converts the pipeline's content-addressed rule-pack/PII spans to
// flat byte offsets into the "\n"-joined evaluated text. AI-Guard judge spans
// (audit-only sentinel address) are dropped — they carry no applicable content
// address and are not reflected in redactions[] or redacted_content.
func flattenSpans(spans []normalize.TransformSpan, segs []string) []Redaction {
	var out []Redaction
	for i := range spans {
		s := &spans[i]
		if normalize.IsAuditOnlySentinelAddress(s.ContentAddress) {
			continue // judge span: audit-only, not applicable
		}
		prefix := blockPrefix(s.ContentAddress, segs)
		out = append(out, Redaction{
			Start:       prefix + s.Start,
			End:         prefix + s.End,
			Replacement: s.Replacement,
			Action:      string(s.Action),
			Reason:      s.Reason,
		})
	}
	return out
}

// blockPrefix returns the byte offset in the "\n"-joined text at which the
// content block addressed by addr begins. The guardrail payload is one message
// (messages.0) with one content block per segment, so the address is
// "messages.0.content.<j>" and the prefix is the sum of the prior segments plus
// their separators. An unparseable address (or index out of range) yields 0 —
// correct for the single-segment case, conservative otherwise.
func blockPrefix(addr string, segs []string) int {
	j := contentBlockIndex(addr)
	if j <= 0 {
		return 0
	}
	prefix := 0
	for k := 0; k < j && k < len(segs); k++ {
		prefix += len(segs[k]) + len(joinSep)
	}
	return prefix
}

// contentBlockIndex parses the trailing content-block index from an address of
// the form "messages.<i>.content.<j>" → j. Returns -1 when the address does not
// carry a "content.<int>" suffix (e.g. the audit-only sentinel, already filtered).
func contentBlockIndex(addr string) int {
	const marker = "content."
	idx := strings.LastIndex(addr, marker)
	if idx < 0 {
		return -1
	}
	n, err := strconv.Atoi(addr[idx+len(marker):])
	if err != nil {
		return -1
	}
	return n
}

// buildRedactedContent applies the pipeline's spans to a fresh payload built
// from the original segments and returns the sanitized "\n"-joined text.
// ApplySpans is the canonical span applier (it resolves content addresses and
// drops the audit-only judge spans), so the result is the reliably-redacted
// text — the primary deliverable of the redact path.
func buildRedactedContent(segs []string, spans []normalize.TransformSpan) string {
	payload := BuildNormalized(segs)
	redacted, _ := normalize.ApplySpans(*payload, spans)
	return redacted.JoinedText(joinSep)
}
