// Package metrics owns the control-plane's L3 business instruments.
//
// Instruments are registered against a shared *metricsreg.Registry which
// both:
//
//  1. registers the underlying Prometheus instruments on
//     prometheus.DefaultRegisterer (so /metrics scrapes keep working), and
//  2. records bindings so the per-tick Sampler.Collect() includes them in
//     metrics_sample messages pushed to Hub via thingclient.
//
// Names follow the dotted opsmetrics convention (spec §6.3 catalog for
// Control Plane). Pre-GA: the old `nexus_control_plane_*` namespace prefix
// is dropped per CLAUDE.md "no backcompat" rule.
//
// route_class label cardinality is bounded by always feeding the Echo route
// template (c.Path()) into the middleware, never the concrete URL — so a
// path like /admin/users/u_abc123 collapses to /admin/users/:id.
package metrics

import (
	metricsreg "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

var (
	// RequestsTotal — counter: http.requests_total{method, route_class, status_class}
	RequestsTotal *metricsreg.Counter
	// RequestDurationMs — histogram: http.duration_ms{route_class}
	RequestDurationMs *metricsreg.Histogram
	// AuthAttemptsTotal — counter: auth.attempts_total{result, method}
	// method ∈ {password, sso, apikey, jwt}; result ∈ {success, missing,
	// invalid_jwt, invalid_api_key, error}.
	AuthAttemptsTotal *metricsreg.Counter
	// IAMEvalTotal — counter: iam.eval_total{decision, cache}
	// decision ∈ {allow, deny}; cache ∈ {hit, miss}.
	IAMEvalTotal *metricsreg.Counter
	// AdminAuditLogFailedTotal — counter: admin.audit_log_failed_total{action}
	// Incremented when audit.Writer cannot publish an admin audit entry
	// (marshal failure, MQ enqueue error). Surfaces an ops-visibility gap
	// — the request itself succeeded at Hub, but the audit row didn't
	// land, so on-call needs the metric to drive alerts. Promauto-registered
	// against prometheus.DefaultRegisterer so /metrics scrapes pick it up.
	AdminAuditLogFailedTotal *metricsreg.Counter

	// --- E90 web-assistant ("Chat with Nexus") instruments (spec §7 / NFR-13) ---
	// These are the base signals from which §7's North Star and counter-metrics
	// are derived in PromQL / analytics. The North Star (cross-page/mitigation
	// tasks completed without a manual redo within ~60s) and the behavioral
	// guardrails (reversal-within-N-min, hallucination/correction, first-question
	// abandonment) need cross-event windowing and a frontend "redid/corrected"
	// signal that does not exist yet — they are NOT single counters and are
	// documented as derivations, not faked here. See e90-s8 §5.

	// AssistantTurnsTotal — counter: assistant.turns_total{result}
	// result ∈ {ok, error, unavailable, unsupported_auth}. The per-turn outcome
	// signal; per-user attribution is via the audit trail, not a label (admin
	// userId would be unbounded cardinality). result="error" is also the
	// raw-error-exposure guardrail signal (it pairs with the SSE `error` event the
	// turn emits). First-question abandonment and the North Star success
	// refinement are NOT derivable from this alone — they need a frontend
	// session-continuation / redo signal that does not exist yet (see e90-s8 §5).
	AssistantTurnsTotal *metricsreg.Counter
	// AssistantToolInvocationsTotal — counter: assistant.tool_invocations_total{tool, result}
	// result ∈ {ok, error}. The bounded internal-call count (NFR-13) and the
	// tool-misfire guardrail (result="error"). `tool` is clamped at the call site
	// to the agent's actual tool set (Agent.ToolNames); a model-emitted name that
	// is not a real tool collapses to tool="unknown" so a hallucinated name can
	// never become an unbounded label.
	AssistantToolInvocationsTotal *metricsreg.Counter
	// AssistantConfirmsTotal — counter: assistant.confirms_total{decision}
	// decision ∈ {allow, deny, timeout, cancelled}. The dangerous-write gate
	// approve/deny rate (NFR-13). The reversal-within-N-min guardrail is derived
	// from {decision="allow"} correlated against later audit undo actions.
	AssistantConfirmsTotal *metricsreg.Counter
	// AssistantNavigationsTotal — counter: assistant.navigations_total
	// Cross-page navigation DIRECTIVES emitted (a single turn can emit more than
	// one) — the North Star "cross-page task" numerator candidate (the
	// no-redo-within-60s success refinement is the derived part needing the
	// frontend signal).
	AssistantNavigationsTotal *metricsreg.Counter
	// AssistantPiiToPromptTotal — counter: assistant.pii_to_prompt_total
	// Tool results in which the web assistant's PII redactor scrubbed at least one
	// match before the output entered the prompt (§8 data governance). The §7
	// guardrail target is 0 — a non-zero rate means raw traffic bodies carrying PII
	// are reaching the assistant's read tools and being redacted at the boundary.
	AssistantPiiToPromptTotal *metricsreg.Counter
	// AssistantConfirmMisrouteTotal — counter: assistant.confirm_misroute_total
	// Confirm POSTs answered 421 because another CP instance owns the session (the
	// multi-replica affinity safety net fired). A sustained rate means ingress
	// session-affinity is misconfigured or churning — the confirm is reaching the
	// wrong replica.
	AssistantConfirmMisrouteTotal *metricsreg.Counter
)

// Register binds the package-level instruments to the supplied opsmetrics
// registry. Must be called once at process startup before the HTTP server
// starts serving traffic. Safe to call again with the same registry
// (registry registration is idempotent on instrument name).
func Register(reg *metricsreg.Registry) {
	if reg == nil {
		return
	}
	RequestsTotal = reg.NewCounter("http.requests_total", []string{"method", "route_class", "status_class"})
	RequestDurationMs = reg.NewHistogram("http.duration_ms", []string{"route_class"})
	AuthAttemptsTotal = reg.NewCounter("auth.attempts_total", []string{"result", "method"})
	IAMEvalTotal = reg.NewCounter("iam.eval_total", []string{"decision", "cache"})
	AdminAuditLogFailedTotal = reg.NewCounter("admin.audit_log_failed_total", []string{"action"})
	AssistantTurnsTotal = reg.NewCounter("assistant.turns_total", []string{"result"})
	AssistantToolInvocationsTotal = reg.NewCounter("assistant.tool_invocations_total", []string{"tool", "result"})
	AssistantConfirmsTotal = reg.NewCounter("assistant.confirms_total", []string{"decision"})
	AssistantNavigationsTotal = reg.NewCounter("assistant.navigations_total", []string{})
	AssistantPiiToPromptTotal = reg.NewCounter("assistant.pii_to_prompt_total", []string{})
	AssistantConfirmMisrouteTotal = reg.NewCounter("assistant.confirm_misroute_total", []string{})
}
