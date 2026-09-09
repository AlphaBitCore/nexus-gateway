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

	// unbuildable holds the implementationIds this pipeline WANTED and could not
	// build (unknown implementation, factory error, connection-stage
	// incompatibility). Every result carries a tag naming them, because a
	// compliance hook that does not run is a gap the request it failed to guard
	// has to be able to report — a build failure has no request-level signal
	// otherwise, and silence reads exactly like "no hook was configured".
	unbuildable []string

	// health is the resolver's failure-window tracker, or nil. Every execution
	// reports its outcome to it, and the merge asks it which of the hooks that
	// ran are currently degraded.
	health *hookHealth

	// unionPrescan, when non-nil, is a single matcher folding every content
	// hook's anchor-stripped prefilter (built+cached per resolved hook set by the
	// PolicyResolver). MayMatchRawContent scans it ONCE instead of looping one
	// cgo scan per hook. nil => use the per-hook loop. See unionprescan.go.
	unionPrescan matcher.Matcher

	// boundComputed/maxBounded/anyUnbounded carry the pre-stamped MaxPatternBound
	// result. BuildPipeline sets these from the resolver's per-generation cache
	// (maxPatternBoundFor) so MaxPatternBound() is O(1) on the streaming hot path
	// instead of re-walking every hook regex per request. boundComputed is false on
	// a NewPipeline-built pipeline (tests), where MaxPatternBound computes lazily.
	boundComputed bool
	maxBounded    int
	anyUnbounded  bool

	// gen is the config generation this pipeline's hooks were resolved under, the
	// same one the per-generation caches above are tagged with. It exists so a
	// consumer can tell "the rule set that produced this" apart from "the rule set
	// that produced the last one" — the only honest lifetime for anything derived
	// from a rule set, including a once-per-rule-set operator warning. Zero on a
	// NewPipeline-built pipeline (tests), which is a generation like any other.
	gen uint64
}

// RuleSetGeneration returns the config generation this pipeline's hooks were
// resolved under. It advances on every config push, so a value derived from the
// rule set can be scoped to the rule set that produced it rather than to the
// process.
func (p *Pipeline) RuleSetGeneration() uint64 { return p.gen }

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

// HasContentScanningHook reports whether the pipeline contains at least one
// hook that actually inspects request/response content (as opposed to
// metadata-only hooks: rate limit, IP access, request size, webhook). It is
// body-independent — the answer depends only on the resolved hook set — so a
// caller can distinguish "a content hook scanned this request" from "only
// metadata hooks ran". Used to stamp an honest compliance-coverage value: a
// pipeline of only metadata hooks scanned no content and must not be reported
// as prompt-scanned. A hook that does not implement RawContentPrescanner is
// treated as content-scanning (conservative: we cannot prove it is
// metadata-only), matching MayMatchRawContent's unaccounted-hook posture.
func (p *Pipeline) HasContentScanningHook() bool {
	for i := range p.hooks {
		pre, ok := p.hooks[i].hook.(core.RawContentPrescanner)
		if !ok || pre.ScansContent() {
			return true
		}
	}
	return false
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
		// Completeness is load-bearing here, not an optimisation detail. The
		// union matcher is cgo memory owned by the resolver's per-generation
		// cache, and Swap closes the superseded generation (closeUnionsIfGen)
		// while a pipeline built under it still holds this pointer — for an SSE
		// response that holder lives for the whole stream. Scanning a closed
		// matcher yields zero hits, and reading that as "no bound hook can match
		// this body" is the one thing this method may never say: the caller then
		// skips extraction and every content hook abstains. Ask the question
		// that can report "did not look", and treat that as may-match.
		if cs, ok := p.unionPrescan.(matcher.CompleteScanner); ok {
			hits, complete := cs.ScanComplete([]string{bytesView(body)}, true)
			return !complete || len(hits) > 0
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
		// The parallel executor already runs each hook on its own goroutine, so
		// one hook that ignores its context cannot delay its siblings — only the
		// wg.Wait at the end. Left as it is; the sequential path is the one the
		// gateway uses and the one a hung hook holds.
		results = p.executeParallel(totalCtx, input)
	} else {
		results = p.executeSequentialAbandonable(totalCtx, input)
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

	kind, hasKind := payloadKindOf(input)
	for i := range p.hooks {
		bh := &p.hooks[i]
		if !hookAppliesToKind(bh, kind, hasKind) {
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

// runSequential is the chain itself. emit reports each hook's result as it is
// produced and returns false to stop the chain — the abandonable wrapper uses
// that to stop feeding a receiver that has walked away.
func (p *Pipeline) runSequential(
	ctx context.Context,
	input *core.HookInput,
	before func(bh *boundHook, i int) bool,
	emit func(core.HookResult, int) bool,
) {
	kind, hasKind := payloadKindOf(input)
	for i := range p.hooks {
		bh := &p.hooks[i]
		if !hookAppliesToKind(bh, kind, hasKind) {
			// Skipped per applicableTrafficKinds — do not append to results
			// so the audit row does not show a phantom hook execution.
			continue
		}
		if before != nil && !before(bh, i) {
			return
		}
		hr := p.executeOneHook(ctx, bh, input)
		hr.Order = i
		if hr.Decision == core.RejectHard {
			emit(hr, i)
			return
		}
		// Apply this hook's effects BEFORE handing the result over. emit is a
		// goroutine boundary now, so it is also the handoff: a receiver holding
		// the result must be able to read input without racing the writer that
		// produced it. Ordering these the other way round is what `go test
		// -race` caught first.
		//
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
		if !emit(hr, i) {
			return
		}
	}
}

// executeSequentialAbandonable runs the sequential chain on its own goroutine
// and waits for each result under that hook's own deadline, so a hook that
// ignores its context cannot hold the request.
//
// WHY THIS IS NOT executeOneHook's job. safeHookExecute calls Execute
// synchronously: the per-hook context carries a deadline the hook is free to
// ignore, and every built-in scanning hook does (pii-detector, keyword-filter,
// content-safety and rulepack-engine never read ctx.Done(); only
// webhook-forward honours it, through http.NewRequestWithContext). Waiting has
// to happen on the other side of a goroutine boundary or it does not happen.
//
// ONE GOROUTINE, NOT ONE PER HOOK. The chain is sequential and stateful — each
// hook sees the previous hook's redactions and tags — so it cannot be split
// across goroutines anyway. Abandoning it abandons the rest of the chain, which
// is the honest outcome: after a hook times out we do not know what the
// remaining hooks would have said.
//
// OWNERSHIP. Once started, the goroutine owns input: it rewrites
// input.Normalized and appends to input.UpstreamTags between hooks, and after
// an abandonment it goes on doing so. Callers must therefore not read input
// after Execute returns — everything they need comes back through the results.
// An abandoned chain contributes no modifications, which is also the right
// answer: half-applied redaction is worse than none.
func (p *Pipeline) executeSequentialAbandonable(ctx context.Context, input *core.HookInput) []core.HookResult {
	type step struct {
		started bool
		timeout time.Duration
		idx     int
		hr      core.HookResult
	}
	// Buffered for two messages per hook so an abandoned goroutine finishes its
	// chain and exits rather than blocking on a send nobody will receive.
	ch := make(chan step, 2*len(p.hooks))

	// Snapshot the traffic kind BEFORE the chain goroutine starts. Once it is
	// running it owns `input` and may replace input.Normalized; the abandon path
	// below then runs on THIS goroutine while that write is in flight, and
	// reading the kind through the pointer there is a data race the detector
	// reports. The value is invariant across a chain, so the snapshot loses
	// nothing.
	abandonKind, abandonHasKind := payloadKindOf(input)
	go func() {
		defer close(ch)
		p.runSequential(ctx, input,
			func(bh *boundHook, i int) bool {
				t := p.perHookTimeout
				if bh.config.TimeoutMs > 0 {
					t = time.Duration(bh.config.TimeoutMs) * time.Millisecond
				}
				select {
				case ch <- step{started: true, timeout: t, idx: i}:
					return true
				case <-ctx.Done():
					return false
				}
			},
			func(hr core.HookResult, i int) bool {
				select {
				case ch <- step{idx: i, hr: hr}:
					return true
				case <-ctx.Done():
					return false
				}
			})
	}()

	results := make([]core.HookResult, 0, len(p.hooks))
	// One timer for the whole chain. Reset needs a stopped, drained timer or
	// the previous hook's expiry fires the next hook's wait immediately.
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		// The started message is sent OUTSIDE the hook call, so it cannot be
		// late because a hook is stuck; no timeout is needed on this receive.
		st, ok := <-ch
		if !ok {
			return results
		}
		if !st.started {
			// Only the abandon paths below leave the protocol, so a result
			// arriving here means the chain is ahead of us — take it.
			results = append(results, st.hr)
			if st.hr.Decision == core.RejectHard {
				return results
			}
			continue
		}
		timer.Reset(st.timeout)
		select {
		case done, more := <-ch:
			if !timer.Stop() {
				<-timer.C
			}
			if !more {
				return results
			}
			results = append(results, done.hr)
			if done.hr.Decision == core.RejectHard {
				return results
			}
		case <-timer.C:
			// The hook outlived its budget and the goroutine is still inside
			// it. Abandon the chain and answer for every hook that never
			// reported, with the posture an error would have taken.
			return append(results, p.abandonedResults(st.idx, st.timeout, abandonKind, abandonHasKind)...)
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return append(results, p.abandonedResults(st.idx, st.timeout, abandonKind, abandonHasKind)...)
		}
	}
}

// abandonedResults synthesises a verdict for the hook at from and every hook
// after it, none of which produced one.
//
// It routes through failClosedOnError rather than choosing a decision here, so
// an abandoned hook and an erroring hook get the same posture from the same
// rule. A fail-closed hook that never ran must not leave the pipeline saying
// the content is fine — that silent approval is the gap fail-closed exists to
// prevent, and it is what the pipeline did before this function existed.
func (p *Pipeline) abandonedResults(from int, timeout time.Duration, payloadKind string, hasPayload bool) []core.HookResult {
	out := make([]core.HookResult, 0, len(p.hooks)-from)
	for i := from; i < len(p.hooks); i++ {
		bh := &p.hooks[i]
		if !hookAppliesToKind(bh, payloadKind, hasPayload) {
			// Answer for the hooks that would have RUN. runSequential skips
			// this one so the audit shows no phantom execution, and answering
			// for it here reintroduces exactly that phantom — with a verdict,
			// which under fail-closed is a refusal. A hook scoped to http
			// traffic would then block an AI request it was never going to
			// look at, and the audit row would name it as the blocker.
			continue
		}
		hookName := bh.config.Name
		if hookName == "" {
			hookName = bh.config.ImplementationID
		}
		hr := core.HookResult{
			HookID:           bh.config.ID,
			ImplementationID: bh.config.ImplementationID,
			HookName:         hookName,
			Order:            i,
			Error:            fmt.Sprintf("hook abandoned after %v without returning", timeout),
		}
		if failClosedOnError(p.strictFailClosed, bh.hook, bh.config) {
			hr.Decision = core.RejectHard
			hr.Reason = "hook abandoned (fail-closed): exceeded its timeout without returning"
			hr.ReasonCode = "HOOK_ABANDONED_FAIL_CLOSED"
		} else {
			hr.Decision = core.Approve
			hr.Reason = "hook abandoned (fail-open): exceeded its timeout without returning"
			hr.ReasonCode = "HOOK_ABANDONED_FAIL_OPEN"
			HookFailOpenTotal.WithLabelValues(hookName).Inc()
		}
		if i == from {
			HookTimeoutTotal.WithLabelValues(hookName).Inc()
		}
		HookDecisionTotal.WithLabelValues(hookName, string(hr.Decision)).Inc()
		out = append(out, hr)
	}
	return out
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

// payloadKindOf snapshots the traffic kind of an Execute's input, once, before
// any hook can replace the payload. See hookAppliesToKind for why the value
// rather than the pointer is what travels.
func payloadKindOf(input *core.HookInput) (string, bool) {
	if input == nil || input.Normalized == nil {
		return "", false
	}
	return string(input.Normalized.Kind), true
}

// hookAppliesToKind reports whether bh should run against the kind in
// input.Normalized. HookConfig.ApplicableTrafficKinds defaults to ["ai"]
// when nil/empty, so content-touching hooks run only on AI traffic unless
// explicitly broadened (e.g. ["ai", "http-json"]).
//
// A nil Normalized payload (connection-stage hooks, empty captures) is
// treated as "any kind". Content-scanning hooks handle the nil case
// themselves and ABSTAIN naturally.
//
// It takes the kind as a VALUE rather than reading it through the input, and
// that is a concurrency requirement rather than a style choice. The abandon path
// calls this for hooks that never ran, on the caller's goroutine, WHILE the
// abandoned chain goroutine is still running and may replace input.Normalized
// with a freshly-built payload (the Modify branch). Reading Kind — a two-word
// string header — out of a struct another goroutine is initialising is a torn
// read, and the race detector says so. Snapshotting the kind once, before the
// chain starts, removes the shared read entirely.
//
// Nothing is lost by snapshotting: every in-chain rewrite of the payload copies
// it and preserves Kind, so the value is invariant for the life of one Execute.
func hookAppliesToKind(bh *boundHook, payloadKind string, hasPayload bool) bool {
	if !hasPayload {
		return true
	}
	kinds := bh.config.ApplicableTrafficKinds
	if len(kinds) == 0 {
		kinds = []string{"ai"}
	}
	for _, k := range kinds {
		if k == "all" || k == "*" {
			return true
		}
		if k == payloadKind {
			return true
		}
		// "ai" matches any ai-* kind; "http" matches any http-* kind. The
		// predicate is asked of the snapshotted value, for the same reason the
		// exact match above is.
		if k == "ai" && normalize.Kind(payloadKind).IsAI() {
			return true
		}
		if k == "http" && normalize.Kind(payloadKind).IsHTTP() {
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

	// newLazyTimeout, not context.WithTimeout: safeHookExecute calls Execute
	// synchronously, so this deadline interrupts nothing — it is a deadline the
	// hook may honour or ignore, and every built-in scanning hook ignores it.
	// The timer is armed on first Done(), which only webhook-forward reaches.
	hookCtx, cancel := newLazyTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	result, err := safeHookExecute(hookCtx, bh.hook, input)
	elapsed := time.Since(start)

	hookName := bh.config.Name
	if hookName == "" {
		hookName = bh.config.ImplementationID
	}

	HookDuration.WithLabelValues(hookName).Observe(elapsed.Seconds())

	// Report the outcome to the failure window BEFORE branching on it, so the
	// two arms cannot disagree about what counts. Only an error counts as a
	// failure: a REJECT or a BLOCK is the hook working, and counting a firing
	// policy as a broken one would light the gauge exactly when compliance is
	// doing its job.
	p.health.Record(bh.config.ImplementationID, err != nil, p.logger)

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
			LatencyUs:        int(elapsed.Microseconds()),
			Error:            err.Error(),
		}

		// Fail-posture on hook ERROR / TIMEOUT / PANIC. See failClosedOnError
		// (enforcement.go) for the precedence: explicit fail-closed/fail-open
		// win; otherwise a strict (non-packet-path) caller fails an ENFORCING
		// hook closed so a transient error cannot leak PII/secrets on the very
		// rule meant to stop them, while packet-path callers stay fail-open.
		if failClosedOnError(p.strictFailClosed, bh.hook, bh.config) {
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
	// Both derive from the single captured elapsed: LatencyUs is precise, LatencyMs
	// its integer-ms floor (sub-millisecond hooks → 0). Not clamped (see HookResult).
	result.LatencyMs = int(elapsed.Milliseconds())
	result.LatencyUs = int(elapsed.Microseconds())

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
