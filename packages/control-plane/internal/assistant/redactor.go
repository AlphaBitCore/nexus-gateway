package assistant

import (
	"context"

	cpmetrics "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/metrics"
	hookcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/validators"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// piiPatternDefinitions mirrors the seeded `nexus/pii-default` builtin
// pii-detector config (the `pii-scanner` HookConfig) so the web assistant scrubs
// the SAME PII classes the compliance pipeline does, using the product's own
// detection engine. Sourcing the deployment's LIVE rule-pack config (so an admin
// can extend the pattern set) is a follow-up; these are the product's canonical
// defaults — email, credit card (Luhn-validated), US SSN, and US phone.
var piiPatternDefinitions = []any{
	map[string]any{"id": "email", "flags": "g", "regex": `\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`},
	map[string]any{"id": "credit_card", "flags": "g", "regex": `\b(?:\d{4}[-\s]?){3}\d{4}\b`, "luhn": true},
	map[string]any{"id": "ssn", "flags": "g", "regex": `\b\d{3}[-\s]?\d{2}[-\s]?\d{4}\b`},
	map[string]any{"id": "phone", "flags": "g", "regex": `\b(?:\+?1[-.\s]?)?(?:\(?\d{3}\)?[-.\s]?)?\d{3}[-.\s]?\d{4}\b`},
}

// bodyReadToolNames is the set of read tools whose output RELAYS raw
// request/response bodies or raw admin records (which can carry PII). It is the
// scope of PII redaction AND the set an operator can disable for governance
// (DisableBodyReads). The aggregate / analysis tools (observe_health,
// analyze_cost, observe_models, route_explain, …) are deliberately EXCLUDED from
// redaction: their output is computed numeric metadata the assistant must reason
// over accurately, and the product's PII patterns — notably the phone pattern —
// would false-match large numbers, corrupting it. `resource_search` is excluded
// too: it returns API-catalog candidates (kind/operationId/path), not raw records.
//
// CAVEAT: scoping spares the aggregate tools, but a body tool's OWN numeric
// metadata (e.g. a 10-digit epoch or token count inside observe_traffic_event's
// JSON) can still be mangled by the phone pattern — redaction runs over the whole
// tool-result text, not per field. The pattern is kept identical to the product's
// canonical nexus/pii-default set (no divergence); the field-scoped redactor
// (parse the JSON, scrub only string body fields, leave numbers) is the follow-up
// that removes this residual.
var bodyReadToolNames = []string{
	"observe_traffic_event", // full TrafficEvent incl. request/response bodies
	"observe_traffic_list",  // recent request rows (may carry content)
	"resource_read",         // raw admin GET responses (users/orgs carry PII)
	"resource_invoke",       // raw admin operation responses (may echo records)
}

// bodyBearingTools is the membership form of bodyReadToolNames for the redaction
// scope check.
var bodyBearingTools = func() map[string]bool {
	m := make(map[string]bool, len(bodyReadToolNames))
	for _, n := range bodyReadToolNames {
		m[n] = true
	}
	return m
}()

// piiRedactor implements agent.Redactor over the product's PiiDetector engine.
// It is the web assistant's data-governance seam (§8): raw bodies returned by the
// body-relaying read tools are scrubbed before the tool output enters the prompt.
// The kernel owns no PII policy — this host-side type supplies it.
type piiRedactor struct {
	hook hookcore.Hook
}

// newPIIRedactor builds the redactor from the canonical PII pattern set running
// in redact mode. It only errors on a malformed pattern definition, which is a
// programming error for the static config above (covered by a unit test).
func newPIIRedactor() (*piiRedactor, error) {
	cfg := &hookcore.HookConfig{
		ID:               "assistant-pii",
		Name:             "assistant-pii",
		ImplementationID: "pii-detector",
		Config: map[string]any{
			// Redact (not block): scrub matches in place rather than refusing the
			// tool result — the assistant still answers, just without the raw PII.
			"onMatch":            map[string]any{"inflightAction": "redact"},
			"patternDefinitions": piiPatternDefinitions,
		},
	}
	h, err := validators.NewPiiDetector(cfg)
	if err != nil {
		return nil, err
	}
	return &piiRedactor{hook: h}, nil
}

// RedactToolOutput scrubs PII from one tool result. It wraps the text as a single
// chat message and runs the PiiDetector in redact mode, returning its rewritten
// content. On any error or a no-match result it returns the input unchanged
// (the detector's own decision), and it counts each tool result that actually
// changed via assistant.pii_to_prompt_total.
func (r *piiRedactor) RedactToolOutput(toolName, text string) string {
	if r == nil || r.hook == nil || text == "" || !bodyBearingTools[toolName] {
		return text
	}
	input := &hookcore.HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind: normalize.KindAIChat,
			Messages: []normalize.Message{{
				Role:    normalize.RoleUser,
				Content: []normalize.ContentBlock{{Type: normalize.ContentText, Text: text}},
			}},
		},
	}
	res, err := r.hook.Execute(context.Background(), input)
	if err != nil || res == nil || res.Decision != hookcore.Modify || len(res.ModifiedContent) == 0 {
		return text
	}
	redacted := res.ModifiedContent[0].Text
	if redacted != text && cpmetrics.AssistantPiiToPromptTotal != nil {
		cpmetrics.AssistantPiiToPromptTotal.With().Inc()
	}
	return redacted
}
