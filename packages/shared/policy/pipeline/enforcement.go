package pipeline

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// enforcement.go — scope-derived, content-INDEPENDENT predicates the streaming
// relay uses BEFORE any response bytes are read to decide whether a request must
// run in buffered execution, plus the runtime fail-posture derivation for a hook
// ERROR/TIMEOUT/PANIC. Kept separate from the executor (pipeline.go): this is
// "could this resolved pipeline ever enforce" introspection, not the run loop.

// MayRedact reports whether any bound hook in this pipeline is configured with an
// onMatch action of "redact". The streaming relay calls this on the resolved
// response pipeline — before any upstream bytes are read — to decide whether a
// request must run in buffered execution: a redact decision rewrites response
// content, which cannot be applied losslessly on a live wire, so a redact-capable
// pipeline forces the whole response to buffer-then-redact-then-deliver.
//
// The check is scope-derived and content-INDEPENDENT: it answers "could this
// resolved pipeline ever redact", never "does this body match a pattern".
// Over-approximation (a redact-action hook whose rules happen not to fire still
// routes to buffer) is the safe direction — it can never under-route a request
// that would have redacted onto the unbuffered streaming path.
func (p *Pipeline) MayRedact() bool {
	return p.anyOnMatchAction(decision.ActionRedact)
}

// MayBlock reports whether any bound hook is configured with an onMatch action of
// "block" (hard reject). Same scope-derived, content-independent contract as
// MayRedact. ParseOnMatch resolves an absent onMatch to the "block" default, so a
// content hook with no explicit action is counted as block — matching its real
// execution behaviour and keeping the buffer-routing direction safe.
func (p *Pipeline) MayBlock() bool {
	return p.anyOnMatchAction(decision.ActionBlock)
}

// anyOnMatchAction reports whether any bound hook's parsed onMatch action equals
// want. A malformed onMatch is skipped: the hook fail-closes at execution, and a
// parse error must not be read as an enforcing action.
func (p *Pipeline) anyOnMatchAction(want decision.Action) bool {
	for i := range p.hooks {
		cfg := p.hooks[i].config
		if cfg == nil {
			continue
		}
		om, err := core.ParseOnMatch(cfg.Config)
		if err != nil {
			continue
		}
		if om.Action == want {
			return true
		}
	}
	return false
}

// SetStrictFailClosed sets the per-service strict fail posture (see the Pipeline
// field doc). BuildPipeline calls this with the same flag it forwards to
// ResolveHooks so the runtime hook-error fail-posture matches the build-time
// posture: the ai-gateway reverse proxy (strict=true) fails an enforcing hook's
// error closed; the agent/tlsbump packet-path callers (strict=false) keep
// fail-open for host-network safety.
func (p *Pipeline) SetStrictFailClosed(strict bool) {
	p.strictFailClosed = strict
}

// failClosedOnError decides whether a hook ERROR/TIMEOUT/PANIC fails closed
// (REJECT_HARD) given the per-service strict posture and the hook's config.
//
// Precedence:
//   - explicit FailBehavior=="fail-closed" → true (always reject)
//   - explicit FailBehavior=="fail-open"   → false (admin override always wins,
//     even under strict — for a known-flaky non-critical enforcing hook)
//   - unset/other                          → strict AND the hook is ENFORCING
//
// "Enforcing" means the hook's onMatch action is redact or block (it would
// rewrite or reject on a match), derived via core.ParseOnMatch on the hook's
// declarative config. A non-enforcing (approve-scope) hook never fails closed.
func failClosedOnError(strict bool, cfg *core.HookConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.FailBehavior)) {
	case "fail-closed":
		return true
	case "fail-open":
		return false
	default:
		return strict && hookIsEnforcing(cfg)
	}
}

// hookIsEnforcing reports whether the hook would redact or block on a match —
// the scope whose guaranteed-execution contract a transient error breaks. The
// onMatch action is parsed from the hook's declarative config; a hook with no
// explicit onMatch defaults to block (the security "block on match" default in
// core.ParseOnMatch), so a match-only enforcer counts as enforcing. An
// unparseable onMatch is treated conservatively as enforcing so a strict caller
// fails closed rather than leaking on a misconfigured rule.
func hookIsEnforcing(cfg *core.HookConfig) bool {
	oc, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return true
	}
	return oc.Action == core.ActionRedact || oc.Action == core.ActionBlock
}
