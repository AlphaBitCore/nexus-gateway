// generative_block.go — the generative prompt hard-block escalation (D5).
//
// Image / video generation return a binary artifact that no content hook can
// scan, so the request prompt is the ONLY inspectable control point for
// illegal-content prevention. A content-rule match on such a prompt therefore
// escalates to a hard 403 even when the matching hook is configured
// observe-only — an observe posture on an uninspectable-output endpoint would
// mean zero prevention.
//
// The escalation is AI-Gateway-local and endpoint-scoped: the shared rule-pack
// engine, its severity gating, and onMatch semantics are untouched, so
// Compliance-Proxy / Agent behavior and every other endpoint kind keep the
// operator-configured posture.
package proxy

import (
	"log/slog"
	"net/http"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/audit"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// reasonGenerativePromptBlocked is stamped on the audit record when a content
// match on a generative prompt is escalated to a hard block. It is an
// AI-Gateway-local reason code, not part of the shared decision vocabulary:
// the escalation is gateway policy layered on top of the pipeline verdict.
// Unlike the redact-inflight-unsupported arm (which leaves the pipeline's
// MODIFY decision on the record), the escalation also stamps the aggregate
// HookDecision as a hard block so the request is counted as blocked by every
// audit consumer that keys on request_hook_decision — the per-hook trace still
// carries each hook's real (approve/observe) vote.
const reasonGenerativePromptBlocked = "GENERATIVE_PROMPT_BLOCKED"

// Per-hook ReasonCode markers emitted by the content-scanning hooks. These
// mirror the string literals the validators stamp (they define no exported
// constants). content-safety and keyword-filter stamp their ReasonCode on a
// match even under an observe (approve) action, so they are visible in
// HookResults after an observe-only match — which is exactly the case the
// generative escalation must catch. The rule-pack engine, by contrast, leaves
// ReasonCode empty on an observe match (it only stamps RULEPACK_MATCH when it
// enforces, and an enforcing match exits the pipeline before this point), so
// observe-only rule-pack matches are detected via their "rule:" tag instead.
const (
	reasonContentSafetyViolation = "CONTENT_SAFETY_VIOLATION"
	reasonKeywordBlocked         = "KEYWORD_BLOCKED"
	tagRulePrefix                = "rule:"
)

// maybeBlockGenerativePrompt applies the generative hard-block after the
// pipeline ran. It escalates to a 403 when the endpoint produces an
// uninspectable artifact (image/video) AND a content hook matched the prompt
// (even observe-only). The per-hook trace keeps each hook's real approve/observe
// vote, but the aggregate HookDecision is stamped as a hard block so every audit
// consumer that keys on request_hook_decision counts this as blocked. Returns
// true when it wrote the 403 (the caller must stop).
func (h *Handler) maybeBlockGenerativePrompt(
	w http.ResponseWriter,
	rec *audit.Record,
	endpointType typology.EndpointKind,
	hookResult *hookcore.CompliancePipelineResult,
	logger *slog.Logger,
) bool {
	if !generativeHardBlockKind(endpointType) {
		return false
	}
	matchedHook, matched := generativePromptContentMatch(hookResult)
	if !matched {
		return false
	}
	logger.Warn("generative prompt content match escalated to hard block",
		slog.String("endpoint", string(endpointType)),
		slog.String("hook", matchedHook),
	)
	rec.HookDecision = string(hookcore.RejectHard)
	rec.RequestAction = hookcore.ActionBlock
	rec.HookReasonCode = reasonGenerativePromptBlocked
	rec.HookReason = "generative prompt content match escalated to block"
	traffic.PrependVia(w.Header(), "ai-gateway")
	w.Header().Set("X-Nexus-Hook", traffic.FormatHookOutcome(traffic.HookOutcomeInput{
		Rejected:     matchedHook,
		RejectReason: reasonGenerativePromptBlocked,
	}))
	w.Header().Set("X-Nexus-Mode", "")
	traffic.SetExposeHeaders(w.Header())
	h.writeError(w, rec, http.StatusForbidden, "generative prompt blocked by content policy")
	return true
}

// generativeHardBlockKind reports whether the endpoint produces a binary
// generative artifact that cannot be content-scanned, making the prompt the
// only enforcement point (D5: image + video generation). Image rides
// ServeProxy's JSON extraction (multimodalUserTextPaths); video is a
// multipart parallel handler that builds its hook input DIRECTLY from the
// parsed prompt (ServeVideoSubmit → scanVideoPrompt) — the JSON-path table
// never sees multipart wires, so the video escalation's reachability rests
// on that direct build, not on a table entry.
// TTS is deliberately absent: a speech waveform of the prompt text is not the
// illegal-content generation surface D5 targets, so the TTS prompt keeps the
// operator-configured posture (its redact posture already fails closed on the
// un-rewritable multimodal wire).
func generativeHardBlockKind(kind typology.EndpointKind) bool {
	return kind == typology.EndpointKindImageGeneration ||
		kind == typology.EndpointKindVideoGeneration
}

// generativePromptContentMatch reports whether the pipeline result carries a
// content-rule match, naming the matched hook when identifiable. Signals, in
// order:
//
//   - aggregate BlockingRule attribution (defensive: an enforcing match
//     normally exits in the block / redact arms before escalation is reached);
//   - a per-hook "rule:" tag — the rule-pack engine's observe-match marker;
//   - a per-hook ReasonCode of CONTENT_SAFETY_VIOLATION or KEYWORD_BLOCKED —
//     the content-safety detector and keyword filter stamp these even on an
//     observe (approve) match, so an illegal-content keyword ban-list protects
//     generative prompts exactly like a rule pack does.
//
// PII is deliberately NOT a signal: PII in a prompt is a privacy / redaction
// concern, not generative abuse — escalating an observe-only PII match would
// false-block legitimate image workflows while its redact posture already fails
// closed on this wire. The PII detector's markers (compliance:pii tags, and it
// stamps no CONTENT_SAFETY_VIOLATION / KEYWORD_BLOCKED code) are not in the set
// above, so PII matches pass through untouched.
func generativePromptContentMatch(r *hookcore.CompliancePipelineResult) (hookName string, matched bool) {
	if r == nil {
		return "", false
	}
	if r.BlockingRule != nil {
		// Aggregate attribution carries no hook name; report the policy
		// marker so the X-Nexus-Hook header stays non-empty.
		return "generative-prompt-policy", true
	}
	for _, hr := range r.HookResults {
		if hr.ReasonCode == reasonContentSafetyViolation || hr.ReasonCode == reasonKeywordBlocked {
			return hr.HookName, true
		}
		for _, tag := range hr.Tags {
			if len(tag) >= len(tagRulePrefix) && tag[:len(tagRulePrefix)] == tagRulePrefix {
				return hr.HookName, true
			}
		}
	}
	return "", false
}
