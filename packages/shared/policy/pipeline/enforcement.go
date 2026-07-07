package pipeline

import (
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/decision"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// MaxPatternBound returns the conservative upper bound + unbounded flag for the
// pipeline's resolved hook set. When the pipeline was built by the PolicyResolver
// (the production path) BuildPipeline pre-stamps the per-generation CACHED value
// (see maxPatternBoundFor), so this is O(1) on the streaming hot path; a pipeline
// built directly via NewPipeline (tests) computes it lazily. The underlying
// computeMaxPatternBound is O(total patterns) — it parses every hook regex — so it
// must NOT run per request; that is why the resolver memoises it per resolved set.
func (p *Pipeline) MaxPatternBound() (maxBounded int, anyUnbounded bool) {
	if p.boundComputed {
		return p.maxBounded, p.anyUnbounded
	}
	return computeMaxPatternBound(p.hooks)
}

// computeMaxPatternBound returns a conservative UPPER BOUND, in bytes, on the longest
// contiguous enforceable match across all bound content hooks — sizing the Model-A
// streaming flush-before-deliver lookahead (modela.Config.MaxPatternBytes). The bound
// must never under-estimate (that reopens the boundary leak), so each pattern is walked
// with matcher.MaxMatchBytes (which rounds up and reports unbounded `*`/`+`/`{n,}` as
// not-bounded). anyUnbounded reports whether ANY bound content hook carries an unbounded
// pattern (or cannot export its patterns) — a value such a pattern matches can exceed
// the tail window, the disclosed best-effort surface the finite lookahead cannot cover;
// callers MAY surface it (e.g. a config-time operator signal). maxBounded is 0 when no
// bounded pattern exists (the engine then falls back to its package default). This is the
// expensive computation (one regexp/syntax walk per pattern); callers cache the result per
// resolved hook set rather than invoking it per request.
func computeMaxPatternBound(hooks []boundHook) (maxBounded int, anyUnbounded bool) {
	for i := range hooks {
		pre, ok := hooks[i].hook.(core.RawContentPrescanner)
		if !ok || !pre.ScansContent() {
			continue // metadata hook: no redactable patterns
		}
		src, ok := hooks[i].hook.(core.PrescanPatternSource)
		if !ok {
			anyUnbounded = true // scans content but can't export patterns → conservatively best-effort
			continue
		}
		pats := src.PrescanPatterns()
		if pats == nil {
			anyUnbounded = true
			continue
		}
		for _, pp := range pats {
			n, bounded := matcher.MaxMatchBytes(pp.Expr)
			if !bounded {
				anyUnbounded = true
				continue
			}
			if n > maxBounded {
				maxBounded = n
			}
		}
	}
	return maxBounded, anyUnbounded
}

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
// that would have redacted onto the unbuffered streaming path. A hook whose
// runtime decision can exceed its declared ceiling (core.RuntimeEscalatable,
// e.g. webhook-forward) or whose onMatch is unparseable is counted as redact-
// capable here regardless of its declared action — see anyOnMatchAction.
func (p *Pipeline) MayRedact() bool {
	return p.anyOnMatchAction(decision.ActionRedact)
}

// MayBlock reports whether any bound hook is configured with an onMatch action of
// "block" (hard reject). Same scope-derived, content-independent contract as
// MayRedact. ParseOnMatch resolves an absent onMatch to the "block" default, so a
// content hook with no explicit action is counted as block — matching its real
// execution behaviour and keeping the buffer-routing direction safe. A hook whose
// runtime decision can exceed its declared ceiling (core.RuntimeEscalatable, e.g.
// webhook-forward) or whose onMatch is unparseable is counted as block-capable
// regardless of its declared action — see anyOnMatchAction.
func (p *Pipeline) MayBlock() bool {
	return p.anyOnMatchAction(decision.ActionBlock)
}

// anyOnMatchAction reports whether any bound hook could enforce the wanted
// action on a match. It returns true in three cases, every one biased toward the
// safe over-route direction (route to buffer rather than leak onto the live
// path):
//
//   - the hook's runtime decision can exceed its declarative onMatch ceiling
//     (it implements core.RuntimeEscalatable and reports true — canonically
//     webhook-forward, whose remote reply can block/redact under any ceiling).
//     Such a hook is treated as may-block AND may-redact regardless of want,
//     because reading only its declared action would under-route it and silently
//     drop the runtime enforcement;
//   - its parsed onMatch action equals want;
//   - its onMatch is unparseable. A malformed config is treated conservatively
//     as enforcing — mirroring hookIsEnforcing — so a misconfigured hook routes
//     to buffer instead of leaking onto the unbuffered live path. (The two
//     predicates previously disagreed here: anyOnMatchAction skipped a parse
//     error while hookIsEnforcing counted it.)
func (p *Pipeline) anyOnMatchAction(want decision.Action) bool {
	for i := range p.hooks {
		if esc, ok := p.hooks[i].hook.(core.RuntimeEscalatable); ok && esc.MayExceedOnMatch() {
			return true
		}
		cfg := p.hooks[i].config
		if cfg == nil {
			continue
		}
		om, err := core.ParseOnMatch(cfg.Config)
		if err != nil {
			return true
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
func failClosedOnError(strict bool, hook core.Hook, cfg *core.HookConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.FailBehavior)) {
	case "fail-closed":
		return true
	case "fail-open":
		return false
	default:
		return strict && hookIsEnforcing(hook, cfg)
	}
}

// hookIsEnforcing reports whether the hook would redact or block on a match —
// the scope whose guaranteed-execution contract a transient error breaks. A hook
// whose runtime decision can EXCEED its declared onMatch ceiling (a
// RuntimeEscalatable, e.g. a webhook whose remote verdict reconciles to a stricter
// action) counts as enforcing regardless of its declared action — mirroring the
// routing predicate (anyOnMatchAction) so a strict caller fails such a hook's
// transient error closed rather than leaking. Otherwise the onMatch action is parsed
// from the hook's declarative config; a hook with no explicit onMatch defaults to
// block (the security "block on match" default in core.ParseOnMatch), so a match-only
// enforcer counts as enforcing. An unparseable onMatch is treated conservatively as
// enforcing so a strict caller fails closed rather than leaking on a misconfigured rule.
func hookIsEnforcing(hook core.Hook, cfg *core.HookConfig) bool {
	if esc, ok := hook.(core.RuntimeEscalatable); ok && esc.MayExceedOnMatch() {
		return true
	}
	oc, err := core.ParseOnMatch(cfg.Config)
	if err != nil {
		return true
	}
	return oc.Action == core.ActionRedact || oc.Action == core.ActionBlock
}
