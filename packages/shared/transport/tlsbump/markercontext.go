package tlsbump

import (
	"context"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// cpMarkerCtxKey is the unexported context key type for CPMarker values.
// Using a private struct type prevents collisions with other packages' keys.
type cpMarkerCtxKey struct{}

// CPMarker is the per-request marker state captured by forward_handler.go
// at the point where all three pieces are known (request-id, domain rule UUID,
// request-side hook outcome). Downstream write sites (upstream.go in Task 3.2
// and sse.go in Task 3.3) read it via CPMarkerFromContext to populate the
// x-nexus-cp-* response headers without re-deriving these values.
type CPMarker struct {
	// RequestID is the compliance-proxy per-request UUID (txID), sourced
	// from x-nexus-request-id or generated fresh via uuid.NewString().
	RequestID string
	// DomainRuleID is the UUID of the matched interception-domain rule
	// (Instance.Domain.ID). Empty string when no domain rule matched the
	// target host (passthrough traffic).
	DomainRuleID string
	// HookOutcome holds the request-side hook pipeline result in the form
	// expected by traffic.FormatHookOutcome. The field is zero-value when
	// compliance is disabled or no hooks ran.
	HookOutcome traffic.HookOutcomeInput
}

// contextWithCPMarker returns a derived context that carries m.
func contextWithCPMarker(parent context.Context, m *CPMarker) context.Context {
	return context.WithValue(parent, cpMarkerCtxKey{}, m)
}

// CPMarkerFromContext retrieves the CPMarker stored by contextWithCPMarker.
// Returns nil when no marker was stashed — this is the case for any
// early-bailout path that does not go through forward_handler (e.g. CONNECT
// tunnel passthrough, TLS-handshake failure).
func CPMarkerFromContext(ctx context.Context) *CPMarker {
	v, _ := ctx.Value(cpMarkerCtxKey{}).(*CPMarker)
	return v
}

// cpHookOutcomeFromResult converts a request-side CompliancePipelineResult
// into a HookOutcomeInput suitable for traffic.FormatHookOutcome. The mapping
// follows spec §4.5:
//   - RejectHard / BlockSoft → Rejected = hookName, RejectReason = reasonCode (or reason)
//   - Modify → appended to Passed + Transformed = true
//   - Approve / Abstain → appended to Passed
//   - Any reject halts iteration (later hooks are not reported).
//
// Returns an empty HookOutcomeInput (→ "none") when r is nil or has no hook
// results.
//
// This function is intentionally per-package rather than shared with AI Gateway's
// aigwHookOutcomeFromResult. The two services may diverge in their hook pipeline
// semantics over time; DRYing them prematurely into shared/ would couple the
// services' evolution unnecessarily.
func cpHookOutcomeFromResult(r *core.CompliancePipelineResult) traffic.HookOutcomeInput {
	if r == nil || len(r.HookResults) == 0 {
		return traffic.HookOutcomeInput{}
	}
	in := traffic.HookOutcomeInput{}
	for _, hr := range r.HookResults {
		switch hr.Decision {
		case core.RejectHard, core.BlockSoft:
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
		case core.Modify:
			in.Passed = append(in.Passed, hr.HookName)
			in.Transformed = true
		default:
			in.Passed = append(in.Passed, hr.HookName)
		}
	}
	return in
}
