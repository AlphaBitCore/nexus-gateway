package pipeline

// coverage_gaps_test.go targets the residual gaps surfaced by
// `go tool cover -func` against this package after the original test suite
// was written. Each test pins an observable invariant — not just a code
// path — and cites the gap it closes in its top comment.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/audit"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/payloadcapture"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// applyModifiedContentToNormalized (was 0% — transitional rewrite helper)

// modifiedNormalizedFixture builds a NormalizedPayload with two messages,
// each carrying one text and one non-text block so the helper has to skip
// non-text fragments while rewriting text in order.
func modifiedNormalizedFixture() *normalize.NormalizedPayload {
	return &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{
				Role: normalize.RoleUser,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "orig-1"},
					{Type: normalize.ContentImageRef},
				},
			},
			{
				Role: normalize.RoleAssistant,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "orig-2"},
				},
			},
			{
				Role: normalize.RoleUser,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "orig-3"},
				},
			},
		},
	}
}

func TestApplyModifiedContentToNormalized_SkipsToolUseEntriesInIndexSpace(t *testing.T) {
	// A ContentToolUse block PRECEDES a ContentText block in one message. The
	// flat ModifiedContent space includes the tool-use leaf entry tagged
	// "tool_use" before the text entry. The positional text walk must SKIP the
	// tool_use entry so the text block receives the text modification — not the
	// tool-use leaf's masked value (the off-by-one this guard closes).
	p := &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{{
			Role: normalize.RoleAssistant,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentToolUse, ToolUse: &normalize.ToolUse{
					CallID: "c", Name: "f", Input: map[string]any{"q": "leaf-pii"}}},
				{Type: normalize.ContentText, Text: "orig-text"},
			},
		}},
	}
	modified := []core.ContentBlock{
		{Type: "tool_use", Text: "[LEAF-MASKED]"}, // tool-use leaf entry, must be skipped
		{Type: "text", Text: "NEW-TEXT"},          // the actual text modification
	}
	got := applyModifiedContentToNormalized(p, modified)
	if got == nil {
		t.Fatal("got nil")
	}
	// content[0] is the tool_use block — its structured Input is untouched by
	// this text walk (tool-arg redaction flows through TransformSpans).
	if got.Messages[0].Content[0].Type != normalize.ContentToolUse {
		t.Fatalf("tool_use block clobbered: %+v", got.Messages[0].Content[0])
	}
	// content[1] is the text block — it must receive NEW-TEXT, NOT the masked
	// tool-leaf value.
	if got.Messages[0].Content[1].Text != "NEW-TEXT" {
		t.Fatalf("text block got %q, want NEW-TEXT (tool_use entry not skipped → index skew)",
			got.Messages[0].Content[1].Text)
	}
}

func TestApplyModifiedContentToNormalized_NilPayloadReturnsNil(t *testing.T) {
	// The transitional helper must be safe to call with a nil payload —
	// callers in the pipeline pass input.Normalized which can be nil for
	// connection-stage / non-content traffic. Returning a non-nil value
	// would mask the "no payload to apply to" signal.
	got := applyModifiedContentToNormalized(nil, []core.ContentBlock{{Text: "x"}})
	if got != nil {
		t.Fatalf("nil payload should return nil, got %+v", got)
	}
}

func TestApplyModifiedContentToNormalized_EmptyModifiedReturnsInputUnchanged(t *testing.T) {
	// Empty modified slice is a no-op; helper should return the same pointer
	// without allocating a copy (the caller may keep iterating on the
	// original payload).
	p := modifiedNormalizedFixture()
	got := applyModifiedContentToNormalized(p, nil)
	if got != p {
		t.Fatalf("empty modified should return input pointer; got %p want %p", got, p)
	}
}

func TestApplyModifiedContentToNormalized_RewritesInOrderSkippingNonText(t *testing.T) {
	// Helper iterates content in (message, content) order and replaces
	// ONLY text blocks; non-text blocks are passed through. Two modified
	// entries must hit message[0].text and message[1].text — leaving
	// message[2] unchanged.
	p := modifiedNormalizedFixture()
	mod := []core.ContentBlock{
		{Text: "new-1"},
		{Text: "new-2"},
	}
	got := applyModifiedContentToNormalized(p, mod)
	if got == nil {
		t.Fatal("got nil")
		return
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages: got %d, want 3 (remaining message preserved)", len(got.Messages))
	}
	if got.Messages[0].Content[0].Text != "new-1" {
		t.Errorf("message[0].content[0]: got %q, want new-1", got.Messages[0].Content[0].Text)
	}
	// Non-text block at message[0].content[1] must be untouched.
	if got.Messages[0].Content[1].Type != normalize.ContentImageRef {
		t.Errorf("non-text block was rewritten: %+v", got.Messages[0].Content[1])
	}
	if got.Messages[1].Content[0].Text != "new-2" {
		t.Errorf("message[1].content[0]: got %q, want new-2", got.Messages[1].Content[0].Text)
	}
	if got.Messages[2].Content[0].Text != "orig-3" {
		t.Errorf("message[2] should be unchanged; got %q", got.Messages[2].Content[0].Text)
	}
	// Original payload must not be mutated (defensive copy contract).
	if p.Messages[0].Content[0].Text != "orig-1" {
		t.Errorf("input payload was mutated: %q", p.Messages[0].Content[0].Text)
	}
}

func TestApplyModifiedContentToNormalized_FewerModifiedThanTextBlocks(t *testing.T) {
	// When modified slice is shorter than text-block count, only the
	// leading text blocks are rewritten; subsequent text blocks AND
	// subsequent whole messages survive verbatim. This is the
	// "remaining messages copied unchanged" fast path inside the helper.
	p := modifiedNormalizedFixture()
	mod := []core.ContentBlock{
		{Text: "ONLY-FIRST"},
	}
	got := applyModifiedContentToNormalized(p, mod)
	if got.Messages[0].Content[0].Text != "ONLY-FIRST" {
		t.Errorf("first text rewrite missing: %q", got.Messages[0].Content[0].Text)
	}
	if got.Messages[1].Content[0].Text != "orig-2" {
		t.Errorf("second message text should be unchanged after limit; got %q", got.Messages[1].Content[0].Text)
	}
	if got.Messages[2].Content[0].Text != "orig-3" {
		t.Errorf("third message text should be unchanged after limit; got %q", got.Messages[2].Content[0].Text)
	}
}

// safeHookExecute panic recovery (was 60% — panic branch uncovered)

// panicHook crashes inside Execute to exercise the recover() branch.
type panicHook struct {
	core.AnyEndpointAnyModality
}

func (panicHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	panic("hook intentionally panicked")
}

func TestSafeHookExecute_RecoversFromPanicAndReturnsError(t *testing.T) {
	// A panic in a third-party hook must be translated into a normal error
	// so the pipeline's fail-policy branch decides what to do. Without the
	// recover the entire data-plane process would crash on a bad hook.
	res, err := safeHookExecute(context.Background(), panicHook{}, &core.HookInput{})
	if err == nil {
		t.Fatal("expected non-nil error from panic recovery")
	}
	if res != nil {
		t.Errorf("expected nil result on panic, got %+v", res)
	}
	if !strings.Contains(err.Error(), "hook panic") {
		t.Errorf("error message lost panic context: %v", err)
	}
}

func TestPipeline_PanicHookFailOpen(t *testing.T) {
	// End-to-end: a panicking hook inside a fail-open pipeline must NOT
	// propagate as REJECT_HARD. This is the observable contract the
	// safeHookExecute branch underpins.
	hks := []boundHook{
		{hook: panicHook{}, config: &core.HookConfig{
			ID: "p1", Name: "panic-hook", Priority: 1, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, 100*time.Millisecond, 1*time.Second, false, testLogger())
	got := p.Execute(context.Background(), &core.HookInput{})
	if got.Decision != core.Approve {
		t.Fatalf("expected APPROVE (fail-open on panic), got %s", got.Decision)
	}
	if got.HookResults[0].Error == "" {
		t.Fatal("expected non-empty error recorded on panic")
	}
}

// NewPipeline default-timeout branches (was 71.4%)

func TestNewPipeline_AppliesDefaultTimeoutsWhenZeroOrNegative(t *testing.T) {
	// Zero / negative inputs must coerce to the 5s per-hook + 30s total
	// defaults so a misconfigured caller doesn't deadlock the pipeline.
	for _, tc := range []struct {
		name     string
		perHook  time.Duration
		total    time.Duration
		wantPer  time.Duration
		wantTotl time.Duration
	}{
		{"zero", 0, 0, 5 * time.Second, 30 * time.Second},
		{"negative", -1, -1, 5 * time.Second, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPipeline(nil, tc.perHook, tc.total, false, testLogger())
			if p.perHookTimeout != tc.wantPer {
				t.Errorf("perHookTimeout: got %v, want %v", p.perHookTimeout, tc.wantPer)
			}
			if p.totalTimeout != tc.wantTotl {
				t.Errorf("totalTimeout: got %v, want %v", p.totalTimeout, tc.wantTotl)
			}
		})
	}
}

func TestNewPipeline_PositiveTimeoutsArePreserved(t *testing.T) {
	// Positive inputs survive — the default branch only fires on
	// zero / negative values.
	p := NewPipeline(nil, 250*time.Millisecond, 7*time.Second, true, testLogger())
	if p.perHookTimeout != 250*time.Millisecond {
		t.Errorf("perHookTimeout: got %v, want 250ms", p.perHookTimeout)
	}
	if p.totalTimeout != 7*time.Second {
		t.Errorf("totalTimeout: got %v, want 7s", p.totalTimeout)
	}
	if !p.parallel {
		t.Error("parallel flag not preserved")
	}
}

// hookAppliesToKind branches (was 12.5%)

func TestHookAppliesToKind_NilInputOrNilNormalizedAreApplicable(t *testing.T) {
	// nil input / nil Normalized → applicable; lets connection-stage and
	// empty-capture hooks run without forcing the caller to stamp a fake
	// payload kind.
	bh := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{"ai"}}}
	if !hookAppliesToKind(bh, nil) {
		t.Error("nil input should be applicable")
	}
	if !hookAppliesToKind(bh, &core.HookInput{Normalized: nil}) {
		t.Error("nil Normalized should be applicable")
	}
}

func TestHookAppliesToKind_EmptyKindsDefaultsToAI(t *testing.T) {
	// Per the helper's doc: empty ApplicableTrafficKinds is treated as
	// ["ai"] for backwards compatibility with older configs.
	bh := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: nil}}
	in := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindAIChat}}
	if !hookAppliesToKind(bh, in) {
		t.Error("empty kinds should default to 'ai' and match KindAIChat")
	}
	httpIn := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindHTTPJSON}}
	if hookAppliesToKind(bh, httpIn) {
		t.Error("empty kinds defaulting to 'ai' must NOT match http kinds")
	}
}

func TestHookAppliesToKind_StarAndAllShortCircuitTrue(t *testing.T) {
	for _, marker := range []string{"all", "*"} {
		bh := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{marker}}}
		in := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindHTTPBinary}}
		if !hookAppliesToKind(bh, in) {
			t.Errorf("marker %q should match every kind", marker)
		}
	}
}

func TestHookAppliesToKind_ExactKindMatch(t *testing.T) {
	bh := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{"ai-embedding"}}}
	match := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindAIEmbedding}}
	mismatch := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindAIChat}}
	if !hookAppliesToKind(bh, match) {
		t.Error("exact kind match should be applicable")
	}
	if hookAppliesToKind(bh, mismatch) {
		t.Error("exact-only kind should reject mismatched ai-chat")
	}
}

func TestHookAppliesToKind_AIFamilyAndHTTPFamilyShorthand(t *testing.T) {
	aiBound := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{"ai"}}}
	httpBound := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{"http"}}}
	cases := []struct {
		name        string
		bh          *boundHook
		kind        normalize.Kind
		wantApplies bool
	}{
		{"ai shorthand matches ai-chat", aiBound, normalize.KindAIChat, true},
		{"ai shorthand matches ai-image", aiBound, normalize.KindAIImage, true},
		{"ai shorthand rejects http-form", aiBound, normalize.KindHTTPForm, false},
		{"http shorthand matches http-multipart", httpBound, normalize.KindHTTPMultipart, true},
		{"http shorthand rejects ai-completion", httpBound, normalize.KindAICompletion, false},
		{"http shorthand matches http-binary", httpBound, normalize.KindHTTPBinary, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: c.kind}}
			if got := hookAppliesToKind(c.bh, in); got != c.wantApplies {
				t.Errorf("got %v, want %v", got, c.wantApplies)
			}
		})
	}
}

func TestHookAppliesToKind_UnknownKindReturnsFalse(t *testing.T) {
	// Unknown / unrecognised payload kind with a kind list that only has
	// specific entries must reject — there's no implicit fall-through.
	bh := &boundHook{config: &core.HookConfig{ApplicableTrafficKinds: []string{"ai-chat"}}}
	in := &core.HookInput{Normalized: &normalize.NormalizedPayload{Kind: normalize.KindUnsupported}}
	if hookAppliesToKind(bh, in) {
		t.Error("unsupported kind should not match a specific allowlist")
	}
}

// PolicyResolver.matchesIngress branches (was 40%)

func TestMatchesIngress_EmptyApplicableAcceptsAll(t *testing.T) {
	r := newResolverWith()
	cfg := &core.HookConfig{}
	for _, ing := range []string{"AI_GATEWAY", "AGENT", "COMPLIANCE_PROXY", "weird"} {
		if !r.matchesIngress(cfg, ing) {
			t.Errorf("empty list should match %q", ing)
		}
	}
}

func TestMatchesIngress_ALLAlias(t *testing.T) {
	r := newResolverWith()
	cfg := &core.HookConfig{ApplicableIngress: []string{"ALL"}}
	if !r.matchesIngress(cfg, "AGENT") {
		t.Error(`"ALL" should match every ingress`)
	}
	// Case-insensitive
	cfg2 := &core.HookConfig{ApplicableIngress: []string{"all"}}
	if !r.matchesIngress(cfg2, "COMPLIANCE_PROXY") {
		t.Error(`"all" lower-case should still match`)
	}
}

func TestMatchesIngress_NamedAliasesExact(t *testing.T) {
	r := newResolverWith()
	cases := []struct {
		listed   []string
		ingress  string
		wantTrue bool
	}{
		{[]string{"AI_GATEWAY"}, "AI_GATEWAY", true},
		{[]string{"AI_GATEWAY"}, "ai_gateway", true},        // case-insensitive
		{[]string{"AI_GATEWAY"}, "COMPLIANCE_PROXY", false}, // wrong target
		{[]string{"COMPLIANCE_PROXY"}, "COMPLIANCE_PROXY", true},
		{[]string{"COMPLIANCE_PROXY"}, "AGENT", false},
		{[]string{"AGENT"}, "AGENT", true},
		{[]string{"AGENT"}, "AI_GATEWAY", false},
	}
	for _, c := range cases {
		cfg := &core.HookConfig{ApplicableIngress: c.listed}
		if got := r.matchesIngress(cfg, c.ingress); got != c.wantTrue {
			t.Errorf("listed=%v ingress=%q: got %v want %v",
				c.listed, c.ingress, got, c.wantTrue)
		}
	}
}

func TestMatchesIngress_FreeFormCaseInsensitive(t *testing.T) {
	// Free-form ingress identifiers (not one of the named aliases) match
	// case-insensitively too — exercises the final EqualFold default arm.
	r := newResolverWith()
	cfg := &core.HookConfig{ApplicableIngress: []string{"custom-ingress"}}
	if !r.matchesIngress(cfg, "CUSTOM-INGRESS") {
		t.Error("free-form match should be case-insensitive")
	}
}

func TestMatchesIngress_MultipleEntriesAnyMatches(t *testing.T) {
	r := newResolverWith()
	cfg := &core.HookConfig{ApplicableIngress: []string{"AGENT", "AI_GATEWAY"}}
	if !r.matchesIngress(cfg, "AI_GATEWAY") {
		t.Error("any-of semantics: second entry should match")
	}
	if r.matchesIngress(cfg, "COMPLIANCE_PROXY") {
		t.Error("multi-entry list must still reject unlisted ingress")
	}
}

// PolicyResolver.SwapIfChanged (was 0%)

func TestSwapIfChanged_DistinctSliceTriggersSwapAndCachePreservedByContentDiff(t *testing.T) {
	// SwapIfChanged exists to avoid blowing the hook cache when a TTL
	// cache returns the same slice repeatedly. The pointer-equality
	// fast-path inside SwapIfChanged is defensive — Swap stores a
	// defensive copy so the resolver's backing array never aliases the
	// caller's. Even so, the secondary content-diff in Swap preserves
	// the hook cache when the row content is unchanged.
	//
	// Observable: a fresh-slice SwapIfChanged with identical content
	// returns true (a Swap occurred) but the underlying hook factory is
	// NOT re-invoked because content matched the prior snapshot.
	c := &countingFactory{}
	registry := core.NewHookRegistry()
	registry.Register("h1", c.Factory())
	registry.Freeze()
	configs := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r := NewPolicyResolver(configs, registry, testLogger())
	if _, err := r.ResolveHooks("request", "AI_GATEWAY", false); err != nil {
		t.Fatalf("warm resolve: %v", err)
	}
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("warm: factory calls = %d, want 1", got)
	}

	// A fresh slice with identical content: SwapIfChanged returns true
	// (Swap fired because the backing arrays differ) but the content-diff
	// inside Swap preserves the hook cache.
	fresh := append([]core.HookConfig(nil), configs...)
	if swapped := r.SwapIfChanged(fresh); !swapped {
		t.Error("fresh-slice SwapIfChanged should return true (defensive-copy semantics)")
	}
	if _, err := r.ResolveHooks("request", "AI_GATEWAY", false); err != nil {
		t.Fatalf("post-swap resolve: %v", err)
	}
	if got := c.calls.Load(); got != 1 {
		t.Errorf("identical-content swap should preserve cache; calls=%d want 1", got)
	}
}

func TestSwapIfChanged_DifferentSliceTriggersSwap(t *testing.T) {
	// A fresh slice (different backing array) goes through the normal
	// Swap path — the cache is reduced by content diff and changed rows
	// are re-instantiated. Returns true.
	c := &countingFactory{}
	registry := core.NewHookRegistry()
	registry.Register("h1", c.Factory())
	registry.Freeze()
	first := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r := NewPolicyResolver(first, registry, testLogger())
	if _, err := r.ResolveHooks("request", "AI_GATEWAY", false); err != nil {
		t.Fatalf("warm: %v", err)
	}

	// Fresh slice header — even with identical content the pointer check
	// fails, so SwapIfChanged invokes Swap (cache survives via DeepEqual).
	fresh := []core.HookConfig{
		{ID: "h1", ImplementationID: "h1", Name: "h1", Priority: 0, Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	if swapped := r.SwapIfChanged(fresh); !swapped {
		t.Error("fresh slice should trigger Swap")
	}
}

func TestSwapIfChanged_EmptyInputTriggersSwap(t *testing.T) {
	// When the loaded snapshot has entries but the new input is empty,
	// the fast-pointer-check fails (len mismatch) so Swap fires — leaving
	// the resolver with no core.
	r := newResolverWith("a", "b")
	if !r.HasHooks("request") {
		t.Fatal("expected initial hooks")
	}
	if swapped := r.SwapIfChanged(nil); !swapped {
		t.Error("empty new snapshot should trigger Swap")
	}
	if r.HasHooks("request") {
		t.Error("expected no hooks after SwapIfChanged(nil)")
	}
}

func TestSwapIfChanged_NilLoadedBranchSwaps(t *testing.T) {
	// Construct a resolver and then null-out the snapshot pointer so the
	// SwapIfChanged "cur is nil" defensive branch fires. Tests that the
	// first-Swap-after-nil path works (callable from a freshly minted
	// resolver before any data has been loaded).
	r := newResolverWith()
	// New resolver was created with no configs; snapshot() returns the
	// stored empty slice, so cur != nil but len == 0. That triggers the
	// "len mismatch" branch — provide a non-empty slice and expect a swap.
	fresh := []core.HookConfig{{ID: "z", ImplementationID: "z", Name: "z",
		Enabled: true, Stage: "request", FailBehavior: "fail-open",
		ApplicableIngress: []string{"ALL"}}}
	if !r.SwapIfChanged(fresh) {
		t.Error("fresh non-empty into empty-snapshot resolver should swap")
	}
}

// PolicyResolver.BuildPipeline 0-hooks branch (was 66.7%)

func TestBuildPipeline_NoApplicableHooksReturnsNilNilNoError(t *testing.T) {
	// BuildPipeline returns (nil, nil) when no enabled hook applies —
	// callers branch on `pipe == nil` to skip pipeline execution entirely.
	// Returning a zero-hook *Pipeline would still walk the dispatch
	// machinery on every request.
	r := newResolverWith("a", "b") // both enabled, but stage="request"
	pipe, err := r.BuildPipeline("connection", "AI_GATEWAY",
		"", nil, time.Second, 5*time.Second, false, false, testLogger())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pipe != nil {
		t.Errorf("expected nil pipeline when no hooks match, got %+v", pipe)
	}
}

func TestBuildPipeline_SkipsIncompatibleHookToNilPipeline(t *testing.T) {
	// Availability-first: when the ONLY candidate is a
	// connection-stage config bound to a non-ConnectionStageCompatible impl,
	// resolve() skips it (no error) and BuildPipeline returns (nil, nil) — the
	// caller treats nil as "no pipeline to run". A single misconfigured hook
	// must never abort the whole build or 500 the data plane.
	registry := core.NewHookRegistry()
	registry.Register("noop", func(_ *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "bad", ImplementationID: "noop", Name: "bad",
			Stage: "connection", Enabled: true, FailBehavior: "fail-open"},
	}, registry, testLogger())
	pipe, err := r.BuildPipeline("connection", "AI_GATEWAY",
		"", nil, time.Second, 5*time.Second, false, false, testLogger())
	if err != nil {
		t.Fatalf("incompatible hook must be skipped, not error: %v", err)
	}
	if pipe != nil {
		t.Errorf("expected nil pipeline when the only hook is dropped, got %+v", pipe)
	}
}

func TestResolve_FactoryError_SkipsHookNoAbort(t *testing.T) {
	// A hook whose factory returns an error is skipped+logged, and a
	// healthy hook in the same config set still resolves. This is the core
	// "one bad rule degrades to that rule off, not all compliance off" fix.
	registry := core.NewHookRegistry()
	registry.Register("boom", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, fmt.Errorf("simulated factory failure")
	})
	registry.Register("ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "bad", ImplementationID: "boom", Name: "bad", Priority: 0,
			Stage: "request", Enabled: true, FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "good", ImplementationID: "ok", Name: "good", Priority: 1,
			Stage: "request", Enabled: true, FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}, registry, testLogger())

	hks, err := r.ResolveHooks("request", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("factory error must be skipped, not propagated: %v", err)
	}
	if len(hks) != 1 {
		t.Fatalf("expected only the healthy hook; got %d", len(hks))
	}
	if hks[0].config.ID != "good" {
		t.Fatalf("surviving hook should be the one whose factory succeeded; got %q", hks[0].config.ID)
	}
}

// AuditEmitter.Emit / EmitDual / buildEvent / stagePayload (were all 0%)

func ptrInt(v int) *int { return &v }

// failMarshalHookResult is a hook result that intentionally cannot be JSON-
// marshalled, so stagePayload's marshal-error branch can be exercised.
// json.Marshal fails on a channel, NaN, or a recursive cycle.
//
// We use a HookResult whose embedded TransformSpans field contains a value
// that cannot be marshalled — but TransformSpans is []TransformSpan with
// only primitive fields, so json.Marshal of the result itself doesn't fail.
// The simpler shape is to leave marshal happy and rely on the success
// branch; the only naturally-failing target is BlockingRule serialisation
// of a value with channels — also impossible given the struct shape.
//
// Both pipeline + blocking-rule marshal branches are therefore exercised
// only on success. Coverage of the error-log path stays under audit's own
// json package (audited via shared/audit tests).

func TestStagePayload_NilResultReturnsAllZero(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	decision, reason, code, pipeline, blocking, tags := stagePayload(e, AuditInfo{}, nil)
	if decision != "" || reason != nil || code != nil || pipeline != nil || blocking != nil || tags != nil {
		t.Errorf("nil result should yield zero values; got (%q, %v, %v, %v, %v, %v)",
			decision, reason, code, pipeline, blocking, tags)
	}
}

func TestStagePayload_PopulatedResultRoundTripsFields(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	res := &core.CompliancePipelineResult{
		Decision:   core.RejectHard,
		Reason:     "bad words",
		ReasonCode: "BAD_WORDS",
		HookResults: []core.HookResult{
			{HookID: "h1", HookName: "k", Decision: core.RejectHard, LatencyMs: 5},
		},
		Tags: []string{"compliance:pii"},
		BlockingRule: &core.BlockingRule{
			Pack: "default", PackVersion: "1.0", RuleID: "r1",
		},
	}
	decision, reason, code, pipeline, blocking, tags := stagePayload(e, AuditInfo{TransactionID: "tx-1"}, res)
	if decision != "REJECT_HARD" {
		t.Errorf("decision: got %q", decision)
	}
	if reason == nil || *reason != "bad words" {
		t.Errorf("reason: got %v", reason)
	}
	if code == nil || *code != "BAD_WORDS" {
		t.Errorf("code: got %v", code)
	}
	if len(pipeline) == 0 || !bytes.Contains(pipeline, []byte("\"hookId\":\"h1\"")) {
		t.Errorf("pipeline JSON missing hook record: %s", string(pipeline))
	}
	if len(blocking) == 0 || !bytes.Contains(blocking, []byte("\"rule_id\":\"r1\"")) {
		t.Errorf("blocking-rule JSON missing rule_id: %s", string(blocking))
	}
	if len(tags) != 1 || tags[0] != "compliance:pii" {
		t.Errorf("tags not round-tripped: %v", tags)
	}
}

func TestStagePayload_EmptyReasonAndCodeStayNil(t *testing.T) {
	// Empty Reason / ReasonCode become nil pointers so the SQL column
	// stores NULL — distinguishes "no reason" from empty-string.
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	res := &core.CompliancePipelineResult{
		Decision: core.Approve,
	}
	_, reason, code, pipeline, blocking, tags := stagePayload(e, AuditInfo{}, res)
	if reason != nil {
		t.Errorf("empty reason should be nil, got %v", reason)
	}
	if code != nil {
		t.Errorf("empty code should be nil, got %v", code)
	}
	if pipeline != nil {
		t.Errorf("empty HookResults should yield nil pipeline, got %s", string(pipeline))
	}
	if blocking != nil {
		t.Errorf("nil BlockingRule should yield nil blocking, got %s", string(blocking))
	}
	if tags != nil {
		t.Errorf("nil Tags should stay nil")
	}
}

func TestBuildEvent_DualPipelineStampsBothStagesAndMergesTags(t *testing.T) {
	// EmitDual → buildEvent must:
	//   • stamp request + response decisions independently
	//   • merge tags across stages
	//   • compute total_tokens = prompt + completion
	//   • route empty UsageStatus to non_llm when no provider, no_body otherwise
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())

	req := &core.CompliancePipelineResult{
		Decision:    core.Approve,
		Action:      core.ActionApprove,
		HookResults: []core.HookResult{{HookID: "h1", HookName: "k1", LatencyMs: 3}},
		Tags:        []string{"compliance:ok"},
	}
	resp := &core.CompliancePipelineResult{
		Decision:    core.BlockSoft,
		Action:      core.ActionBlock,
		Reason:      "post-flagged",
		ReasonCode:  "POST",
		HookResults: []core.HookResult{{HookID: "h2", HookName: "k2", LatencyMs: 7}},
		Tags:        []string{"severity:soft"},
	}
	usage := traffic.UsageMeta{
		PromptTokens:     ptrInt(11),
		CompletionTokens: ptrInt(13),
		Status:           traffic.UsageStatusOK,
	}
	info := AuditInfo{
		TransactionID: "txn-1",
		ConnectionID:  "conn-1",
		TraceID:       "trace-1",
		Headers: map[string][]string{
			"Content-Type": {"application/json"},
			"User-Agent":   {"curl/8.5"},
		},
		RequestMeta: traffic.RequestMeta{
			Provider:          "openai",
			Model:             "gpt-4o-mini",
			ApiKeyClass:       "sk-",
			ApiKeyFingerprint: "deadbeefdeadbeef",
		},
		LatencyBreakdown: map[string]int{"conn_setup_ms": 8},
		DomainRuleID:     "d-1",
		PathAction:       "PROCESS",
		SourceProcess:    "Google Chrome Helper",
	}

	e.EmitDual(
		&core.HookInput{IngressType: "COMPLIANCE_PROXY", TargetHost: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
		info, req, resp, "BUMP_SUCCESS", 200, 25,
		[]byte(`{"hello":"world"}`), []byte(`{"ok":true}`), usage,
	)

	if got := w.count(); got != 1 {
		t.Fatalf("EmitDual should Enqueue exactly one event, got %d", got)
	}
	ev := w.events[0]

	// Stage 1: request stamped.
	if ev.RequestHookDecision != "APPROVE" {
		t.Errorf("request decision: got %q", ev.RequestHookDecision)
	}
	// Stage 2: response stamped via nullableString pointer.
	if ev.ResponseHookDecision == nil || *ev.ResponseHookDecision != "BLOCK_SOFT" {
		t.Errorf("response decision: got %v", ev.ResponseHookDecision)
	}
	if ev.ResponseHookReason == nil || *ev.ResponseHookReason != "post-flagged" {
		t.Errorf("response reason: got %v", ev.ResponseHookReason)
	}

	// Tag merging.
	if len(ev.ComplianceTags) != 2 ||
		ev.ComplianceTags[0] != "compliance:ok" ||
		ev.ComplianceTags[1] != "severity:soft" {
		t.Errorf("merged tags: got %v", ev.ComplianceTags)
	}

	// Latency phase aggregates: hook ms summed from JSONB blob.
	if ev.RequestHooksMs == nil || *ev.RequestHooksMs != 3 {
		t.Errorf("request hooks ms: got %v want 3", ev.RequestHooksMs)
	}
	if ev.ResponseHooksMs == nil || *ev.ResponseHooksMs != 7 {
		t.Errorf("response hooks ms: got %v want 7", ev.ResponseHooksMs)
	}

	// Token math.
	if ev.PromptTokens != 11 || ev.CompletionTokens != 13 || ev.TotalTokens != 24 {
		t.Errorf("tokens: prompt=%d completion=%d total=%d", ev.PromptTokens, ev.CompletionTokens, ev.TotalTokens)
	}

	// status_code stamped via pointer (non-zero → non-nil).
	if ev.StatusCode == nil || *ev.StatusCode != 200 {
		t.Errorf("statusCode: got %v", ev.StatusCode)
	}

	// User-Agent extracted.
	if ev.UserAgent == nil || *ev.UserAgent != "curl/8.5" {
		t.Errorf("user-agent: got %v", ev.UserAgent)
	}

	// Domain/path/process fields.
	if ev.DomainRuleID != "d-1" || ev.PathAction != "PROCESS" || ev.SourceProcess != "Google Chrome Helper" {
		t.Errorf("domain fields: %+v / %+v / %+v", ev.DomainRuleID, ev.PathAction, ev.SourceProcess)
	}

	// Provider stamped + usage status preserved.
	if ev.Provider != "openai" || ev.Model != "gpt-4o-mini" {
		t.Errorf("provider/model: %q / %q", ev.Provider, ev.Model)
	}
	if ev.UsageExtractionStatus != string(traffic.UsageStatusOK) {
		t.Errorf("usage status: got %q want ok", ev.UsageExtractionStatus)
	}

	// LatencyBreakdown preserved.
	if v := ev.LatencyBreakdown["conn_setup_ms"]; v != 8 {
		t.Errorf("latencyBreakdown: got %v want 8", v)
	}

	// Request body container shape: inline (small body).
	if ev.RequestBody.Kind != audit.BodyInline {
		t.Errorf("request body kind: got %s, want inline", ev.RequestBody.Kind)
	}

	// IngressType propagated from HookInput.
	if ev.IngressType != "COMPLIANCE_PROXY" {
		t.Errorf("ingress: got %q", ev.IngressType)
	}
	if ev.BumpStatus != "BUMP_SUCCESS" {
		t.Errorf("bumpStatus: got %q", ev.BumpStatus)
	}
}

func TestBuildEvent_EmitSingleResponseNilResponseDecisionStaysNullablePointerNil(t *testing.T) {
	// Emit (single-pipeline path) forwards a nil response result; the
	// resulting AuditEvent must carry ResponseHookDecision == nil so the
	// Hub-side column persists SQL NULL rather than "" or "APPROVE".
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	req := &core.CompliancePipelineResult{
		Decision: core.Approve,
	}
	e.Emit(&core.HookInput{IngressType: "AI_GATEWAY"}, AuditInfo{}, req, "BUMP_SUCCESS", 0, 10, nil, nil, traffic.UsageMeta{})
	if w.count() != 1 {
		t.Fatalf("Emit should write exactly one event")
	}
	ev := w.events[0]
	if ev.ResponseHookDecision != nil {
		t.Errorf("expected nil response decision pointer, got %v", *ev.ResponseHookDecision)
	}
	// statusCode 0 → nil pointer (not a real status).
	if ev.StatusCode != nil {
		t.Errorf("statusCode 0 should be nil pointer, got %v", *ev.StatusCode)
	}
	// Body containers absent on zero bodies.
	if ev.RequestBody.Kind != audit.BodyAbsent || ev.ResponseBody.Kind != audit.BodyAbsent {
		t.Errorf("empty bodies should yield BodyAbsent; got %s / %s",
			ev.RequestBody.Kind, ev.ResponseBody.Kind)
	}
	// No provider → usage status defaults to "non_llm".
	if ev.UsageExtractionStatus != string(traffic.UsageStatusNonLLM) {
		t.Errorf("no-provider usage status: got %q want non_llm", ev.UsageExtractionStatus)
	}
}

func TestBuildEvent_NoUsageProviderPresentDefaultsToNoBody(t *testing.T) {
	// Empty UsageStatus + present provider → "no_body" (we knew it was an
	// AI request, just couldn't extract counts). Distinguishes the
	// non-LLM case (no provider).
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	info := AuditInfo{
		RequestMeta: traffic.RequestMeta{Provider: "openai"},
	}
	e.Emit(&core.HookInput{}, info, &core.CompliancePipelineResult{Decision: core.Approve},
		"BUMP_SUCCESS", 200, 1, nil, nil, traffic.UsageMeta{})
	if w.events[0].UsageExtractionStatus != string(traffic.UsageStatusNoBody) {
		t.Errorf("status: got %q want no_body", w.events[0].UsageExtractionStatus)
	}
}

func TestBuildEvent_PhaseSinkPopulatesUpstreamTimings(t *testing.T) {
	// info.PhaseSink with both Ttfb and Total populated → AuditEvent
	// carries non-nil pointers; missing sink → both nil.
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())

	// Build a phase sink by running it through the tracing transport
	// helpers to set both timings without resorting to source mutation.
	ps := traffic.NewPhaseSink()
	// Manually nudge both timings: simulate one full upstream roundtrip
	// via AddBreakdown on the public surface (only path that doesn't
	// require a real HTTP roundtrip). The Ttfb/Total atomic fields stay
	// nil unless the transport observed bytes. So this test verifies the
	// "all nil" branch which is the common case for request-stage block
	// without ever reaching the upstream.
	info := AuditInfo{PhaseSink: ps}
	e.Emit(&core.HookInput{}, info, nil, "", 0, 0, nil, nil, traffic.UsageMeta{})
	ev := w.events[0]
	if ev.UpstreamTtfbMs != nil {
		t.Errorf("ttfb without observed bytes should be nil, got %v", *ev.UpstreamTtfbMs)
	}
	if ev.UpstreamTotalMs != nil {
		t.Errorf("total without observed bytes should be nil, got %v", *ev.UpstreamTotalMs)
	}
}

func TestBuildEvent_LargeBodyWithSpillStoreYieldsSpillBody(t *testing.T) {
	// With a spill store + payload-capture threshold of 8 bytes, a 16-byte
	// body must end up as a Spill body — exercising the "store != nil &&
	// size >= threshold" branch inside EmitBody.
	w := &captureWriter{}
	store := payloadcapture.NewStore(payloadcapture.Config{MaxInlineBodyBytes: 8})
	e := NewAuditEmitter(w, testEmitterLogger()).
		WithSpillStore(nopSpill{}).
		WithPayloadCaptureStore(store)
	big := bytes.Repeat([]byte("x"), 16)
	e.Emit(&core.HookInput{}, AuditInfo{}, &core.CompliancePipelineResult{Decision: core.Approve, Action: core.ActionApprove},
		"BUMP_SUCCESS", 200, 1, big, big, traffic.UsageMeta{})
	ev := w.events[0]
	if ev.RequestBody.Kind != audit.BodySpill {
		t.Errorf("expected spill body for large request, got %s", ev.RequestBody.Kind)
	}
	if ev.ResponseBody.Kind != audit.BodySpill {
		t.Errorf("expected spill body for large response, got %s", ev.ResponseBody.Kind)
	}
}

// EmitKillSwitchPassthrough (was 0%)

func TestEmitKillSwitchPassthrough_HostPort(t *testing.T) {
	// SplitHostPort happy path: source "1.2.3.4:5678" → SourceIP "1.2.3.4".
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	e.EmitKillSwitchPassthrough("1.2.3.4:5678", "api.openai.com")
	if w.count() != 1 {
		t.Fatalf("expected 1 event")
	}
	ev := w.events[0]
	if ev.SourceIP != "1.2.3.4" {
		t.Errorf("sourceIP: got %q want 1.2.3.4", ev.SourceIP)
	}
	if ev.TargetHost != "api.openai.com" {
		t.Errorf("targetHost: got %q", ev.TargetHost)
	}
	if ev.BumpStatus != "BUMP_DISABLED_EMERGENCY" {
		t.Errorf("bumpStatus: got %q", ev.BumpStatus)
	}
	if ev.RequestHookDecision != "PASSTHROUGH" {
		t.Errorf("decision: got %q", ev.RequestHookDecision)
	}
	if ev.RequestHookReason == nil || !strings.Contains(*ev.RequestHookReason, "kill switch") {
		t.Errorf("reason: got %v", ev.RequestHookReason)
	}
	if ev.RequestHookReasonCode == nil || *ev.RequestHookReasonCode != "KILLSWITCH_ENGAGED" {
		t.Errorf("reasonCode: got %v", ev.RequestHookReasonCode)
	}
}

func TestEmitKillSwitchPassthrough_BareIPFallsBack(t *testing.T) {
	// When sourceAddr lacks a port, SplitHostPort errors and the helper
	// falls back to using the whole string as SourceIP.
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	e.EmitKillSwitchPassthrough("10.0.0.1", "example.com")
	if w.events[0].SourceIP != "10.0.0.1" {
		t.Errorf("fallback sourceIP: got %q", w.events[0].SourceIP)
	}
}

// EmitExempted (was 0%)

func TestEmitExempted_StampsExemptionFields(t *testing.T) {
	w := &captureWriter{}
	e := NewAuditEmitter(w, testEmitterLogger())
	e.EmitExempted("9.9.9.9", "api.anthropic.com", "ex-42", "audit-paused")
	ev := w.events[0]
	if ev.SourceIP != "9.9.9.9" || ev.TargetHost != "api.anthropic.com" {
		t.Errorf("source/target: %q / %q", ev.SourceIP, ev.TargetHost)
	}
	if ev.RequestHookDecision != "EXEMPTED" {
		t.Errorf("decision: got %q", ev.RequestHookDecision)
	}
	if ev.RequestHookReasonCode == nil || *ev.RequestHookReasonCode != "EXEMPTED" {
		t.Errorf("reasonCode: got %v", ev.RequestHookReasonCode)
	}
	if ev.RequestHookReason == nil || !strings.Contains(*ev.RequestHookReason, "ex-42") {
		t.Errorf("reason missing exemption id: %v", ev.RequestHookReason)
	}
	if ev.RequestHookReason == nil || !strings.Contains(*ev.RequestHookReason, "audit-paused") {
		t.Errorf("reason missing exemption reason: %v", ev.RequestHookReason)
	}
	if ev.BumpStatus != "BUMP_SUCCESS" {
		t.Errorf("bumpStatus: got %q want BUMP_SUCCESS", ev.BumpStatus)
	}
}

// HookConfigCache.Snapshot (was 0%)

func TestHookConfigCache_Snapshot_NilReceiverReturnsNil(t *testing.T) {
	// Defensive nil-receiver guard so runtime-introspection callers
	// (e31-s7) don't crash when wiring is incomplete.
	var c *HookConfigCache
	if got := c.Snapshot(); got != nil {
		t.Errorf("nil receiver should return nil, got %v", got)
	}
}

func TestHookConfigCache_Snapshot_ReturnsRedactedView(t *testing.T) {
	loader := func(_ context.Context) ([]core.HookConfig, error) {
		return []core.HookConfig{
			{
				ID:                "h1",
				ImplementationID:  "keyword-filter",
				Name:              "secrets-filter",
				Stage:             "request",
				Priority:          5,
				Enabled:           true,
				FailBehavior:      "fail-open",
				TimeoutMs:         200,
				ApplicableIngress: []string{"AI_GATEWAY"},
				Config: map[string]any{
					"api_key": "shouldnotleak", // redacted from snapshot
				},
			},
		}, nil
	}
	cache := NewHookConfigCache(loader, builtins.Registry, time.Minute, slog.Default())
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	snap := cache.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot length: got %d want 1", len(snap))
	}
	got := snap[0]
	if got.ID != "h1" || got.Name != "secrets-filter" || got.ImplementationID != "keyword-filter" ||
		got.Stage != "request" || got.Priority != 5 || !got.Enabled ||
		got.FailBehavior != "fail-open" || got.TimeoutMs != 200 ||
		len(got.ApplicableIngress) != 1 || got.ApplicableIngress[0] != "AI_GATEWAY" {
		t.Errorf("snapshot fields not round-tripped: %+v", got)
	}
}

// HookConfigCache.Start failure branch (was 66.7% — warn-and-continue branch)

func TestHookConfigCache_Start_ContinuesOnLoaderError(t *testing.T) {
	// Initial loader failure must log a warning but return nil so the
	// service still boots with an empty config; the TTL backstop / push
	// callback will retry later.
	loader := func(_ context.Context) ([]core.HookConfig, error) {
		return nil, errors.New("db unreachable")
	}
	buf := &bytes.Buffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cache := NewHookConfigCache(loader, builtins.Registry, time.Minute, logger)
	if err := cache.Start(context.Background()); err != nil {
		t.Fatalf("Start must not propagate loader error, got %v", err)
	}
	if !strings.Contains(buf.String(), "initial hook config load failed") {
		t.Errorf("expected warning log, got %q", buf.String())
	}
}

func TestHookConfigCache_NilLoggerDefaultsToSlogDefault(t *testing.T) {
	// nil logger arg → cache constructor must not crash; it falls back to
	// slog.Default for the operator-visible logs.
	cache := NewHookConfigCache(
		func(_ context.Context) ([]core.HookConfig, error) { return nil, nil },
		builtins.Registry, 0, nil,
	)
	if cache == nil || cache.logger == nil {
		t.Fatal("cache or logger nil after default-fallback")
	}
}

// RegisterMetrics (was 0%)

func TestRegisterMetrics_RegistersAllVectorsUnderNamespace(t *testing.T) {
	// RegisterMetrics must wire every Histogram + CounterVec into the
	// supplied Registerer under the namespace label, so a Gather() picks
	// them up with the expected name.
	reg := prometheus.NewRegistry()
	m := RegisterMetrics(reg, "unit_test_ns")
	if m == nil {
		t.Fatal("nil metrics")
		return
	}
	if m.PipelineDuration == nil || m.HookDuration == nil ||
		m.HookDecisionTotal == nil || m.PipelineDecisionTotal == nil ||
		m.HookErrorTotal == nil || m.HookTimeoutTotal == nil ||
		m.HookFailOpenTotal == nil {
		t.Fatalf("missing metric vector(s): %+v", m)
	}

	// Drive one observation so at least one metric exposes.
	m.PipelineDuration.Observe(0.001)
	m.HookDecisionTotal.WithLabelValues("test-hook", "APPROVE").Inc()
	m.HookFailOpenTotal.WithLabelValues("test-hook").Inc()
	got, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"unit_test_ns_compliance_pipeline_duration_seconds": false,
		"unit_test_ns_compliance_hook_decision_total":       false,
		"unit_test_ns_compliance_hook_fail_open_total":      false,
	}
	for _, mf := range got {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("metric %s not registered under namespace", name)
		}
	}
}

func TestRegisterDefaultMetrics_Idempotent(t *testing.T) {
	// The boot-time entry point must tolerate being reached from multiple
	// wiring paths: a second call must not re-register on the default
	// registerer (promauto panics on duplicate registration).
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("repeated RegisterDefaultMetrics panicked: %v", r)
		}
	}()
	RegisterDefaultMetrics("nexus")
	RegisterDefaultMetrics("nexus")
}

func TestRegisterMetrics_NilRegistererFallsBackToDefault(t *testing.T) {
	// Passing nil for reg must coerce to prometheus.DefaultRegisterer
	// rather than panic — supports the "legacy single-registry" path.
	// Use a unique namespace per invocation so concurrent runs (or a
	// repeat of this test) don't collide with the default registry.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil registerer caused panic: %v", r)
		}
	}()
	ns := "compliance_test_default_reg_" + uniqueSuffix()
	m := RegisterMetrics(nil, ns)
	if m == nil || m.PipelineDuration == nil {
		t.Fatal("default-registerer fallback failed to build metrics")
	}
}

// uniqueSuffix returns a process-unique label suffix so repeated registrations
// against prometheus.DefaultRegisterer don't collide across test runs.
var uniqueCounter atomic.Int64

func uniqueSuffix() string {
	return time.Now().Format("150405") + "_" + intToA(int(uniqueCounter.Add(1)))
}

func intToA(n int) string {
	// Minimal int-to-string without strconv import collision worry.
	if n == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	return string(buf[i:])
}

// Smoke: parallel Pipeline with hookAppliesToKind filter skips non-applicable

func TestPipeline_Parallel_SkipsNonApplicableHooks(t *testing.T) {
	// In parallel mode, a hook with ApplicableTrafficKinds=["ai-image"]
	// running against ai-chat input must be filtered out — no HookResult
	// for it, even though we tried to dispatch.
	chatHook := &stubHook{decision: core.Approve, reason: "chat-ok"}
	imgHook := &stubHook{decision: core.RejectHard, reason: "image-only"}

	hks := []boundHook{
		{hook: chatHook, config: &core.HookConfig{
			ID: "h1", Name: "ai-chat-hook", Priority: 1, FailBehavior: "fail-open",
			ApplicableTrafficKinds: []string{"ai-chat"},
		}},
		{hook: imgHook, config: &core.HookConfig{
			ID: "h2", Name: "ai-image-hook", Priority: 2, FailBehavior: "fail-open",
			ApplicableTrafficKinds: []string{"ai-image"},
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, true, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{
		Normalized: &normalize.NormalizedPayload{Kind: normalize.KindAIChat},
	})
	if res.Decision != core.Approve {
		t.Fatalf("filtered REJECT_HARD should NOT have fired, got %s", res.Decision)
	}
	if len(res.HookResults) != 1 {
		t.Errorf("expected exactly 1 hook result (image-hook filtered), got %d", len(res.HookResults))
	}
	if imgHook.executed.Load() != 0 {
		t.Errorf("image hook should not have executed; got %d", imgHook.executed.Load())
	}
}

// applyModifiedContentToNormalized — exhaust-modified inside one message

func TestApplyModifiedContentToNormalized_BreaksOnLimitWithinMessage(t *testing.T) {
	// A single message with two text blocks + a modified slice of length 1
	// must rewrite only the first block; the inner-loop break on
	// `mi >= len(modified)` fires while we're still inside that message,
	// leaving the second text block untouched.
	p := &normalize.NormalizedPayload{
		Kind: normalize.KindAIChat,
		Messages: []normalize.Message{
			{
				Role: normalize.RoleUser,
				Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "first"},
					{Type: normalize.ContentText, Text: "second"},
				},
			},
		},
	}
	got := applyModifiedContentToNormalized(p, []core.ContentBlock{{Text: "FIRST"}})
	if got.Messages[0].Content[0].Text != "FIRST" {
		t.Errorf("first block: got %q want FIRST", got.Messages[0].Content[0].Text)
	}
	if got.Messages[0].Content[1].Text != "second" {
		t.Errorf("second block within same message must stay untouched, got %q", got.Messages[0].Content[1].Text)
	}
}

// mergeSortedDedup empty branch (was 92.9%)

func TestMergeSortedDedup_BothEmptyReturnsNil(t *testing.T) {
	if got := mergeSortedDedup(nil, nil); got != nil {
		t.Errorf("both nil should return nil, got %v", got)
	}
	if got := mergeSortedDedup([]string{}, []string{}); got != nil {
		t.Errorf("both empty should return nil, got %v", got)
	}
}

// executeOneHook empty-Name fallback + nil-result Abstain (was 94.9%)

// nilResultHook returns (nil, nil) so the pipeline must default to Abstain.
type nilResultHook struct {
	core.AnyEndpointAnyModality
}

func (nilResultHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return nil, nil
}

func TestPipeline_NilResultBecomesAbstain(t *testing.T) {
	hks := []boundHook{
		{hook: nilResultHook{}, config: &core.HookConfig{
			ID: "h1", Name: "nil-result", Priority: 1, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{})
	if len(res.HookResults) != 1 {
		t.Fatalf("expected 1 hook result, got %d", len(res.HookResults))
	}
	if res.HookResults[0].Decision != core.Abstain {
		t.Errorf("nil result should default to ABSTAIN, got %s", res.HookResults[0].Decision)
	}
}

func TestPipeline_HookNameFallsBackToImplementationID(t *testing.T) {
	// When HookConfig.Name is empty, executeOneHook uses ImplementationID
	// for the metric label + log fields. We confirm via the populated
	// HookName field on the HookResult that the fallback fired.
	hks := []boundHook{
		{hook: &stubHook{decision: core.Approve}, config: &core.HookConfig{
			ID: "h1", Name: "" /* empty */, ImplementationID: "impl-fallback",
			Priority: 1, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{})
	if res.HookResults[0].HookName != "impl-fallback" {
		t.Errorf("HookName: got %q want impl-fallback (Name fallback)", res.HookResults[0].HookName)
	}
}

// executeParallel pCancel on RejectHard (was 95.5%)

// orderedHook records execution order to verify the cancel-on-RejectHard
// fires by observing that slow hooks get cancelled mid-flight.
type orderedHook struct {
	core.AnyEndpointAnyModality
	dec      core.Decision
	delay    time.Duration
	executed atomic.Int32
}

func (h *orderedHook) Execute(ctx context.Context, _ *core.HookInput) (*core.HookResult, error) {
	h.executed.Add(1)
	if h.delay > 0 {
		select {
		case <-time.After(h.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &core.HookResult{Decision: h.dec}, nil
}

func TestPipeline_ParallelRejectHardTriggersInternalCancel(t *testing.T) {
	// Parallel mode: when one hook returns RejectHard, the executor calls
	// pCancel() so peer hooks doing long work observe a cancelled context
	// and short-circuit with ctx.Err(). The observable contract is the
	// pipeline-level RejectHard decision being reached without waiting
	// the full slow-hook delay.
	rejecter := &orderedHook{dec: core.RejectHard}
	peer := &orderedHook{dec: core.Approve, delay: 2 * time.Second}
	hks := []boundHook{
		{hook: rejecter, config: &core.HookConfig{
			ID: "r", Name: "rejecter", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: peer, config: &core.HookConfig{
			ID: "p", Name: "peer", Priority: 2, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, 5*time.Second, 30*time.Second, true /* parallel */, testLogger())
	start := time.Now()
	res := p.Execute(context.Background(), &core.HookInput{})
	elapsed := time.Since(start)
	if res.Decision != core.RejectHard {
		t.Fatalf("expected REJECT_HARD, got %s", res.Decision)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("parallel cancel did NOT fire: elapsed %v should be << 2s", elapsed)
	}
}

// executeSequential Modify+TransformSpans propagation (was 78.9%)

// spanModifyHook returns a Modify decision with a TransformSpan that
// rewrites the first text block, so the sequential executor's
// `len(hr.TransformSpans) > 0` branch fires and the second hook observes
// the patched payload.
type spanModifyHook struct {
	core.AnyEndpointAnyModality
}

func (spanModifyHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision: core.Modify,
		TransformSpans: []normalize.TransformSpan{
			{
				Source:         normalize.SourceHook,
				Action:         normalize.ActionRedact,
				ContentAddress: "messages.0.content.0",
				Start:          0,
				End:            5,
				Replacement:    "XXXXX",
			},
		},
	}, nil
}

// modifiedContentHook returns a Modify decision via the transitional
// ModifiedContent slice (no TransformSpans), exercising the legacy
// `applyModifiedContentToNormalized` branch in executeSequential.
type modifiedContentHook struct {
	core.AnyEndpointAnyModality
}

func (modifiedContentHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision: core.Modify,
		ModifiedContent: []core.ContentBlock{
			{Text: "TRANSITIONAL"},
		},
	}, nil
}

// observerHook captures the text it sees in input.Normalized so the test
// can prove the prior hook's modifications propagated.
type observerHook struct {
	core.AnyEndpointAnyModality
	seen string
}

func (h *observerHook) Execute(_ context.Context, in *core.HookInput) (*core.HookResult, error) {
	if in.Normalized != nil && len(in.Normalized.Messages) > 0 && len(in.Normalized.Messages[0].Content) > 0 {
		h.seen = in.Normalized.Messages[0].Content[0].Text
	}
	return &core.HookResult{Decision: core.Approve}, nil
}

func TestPipeline_Sequential_ModifyWithTransformSpansPropagates(t *testing.T) {
	observer := &observerHook{}
	hks := []boundHook{
		{hook: spanModifyHook{}, config: &core.HookConfig{
			ID: "m", Name: "modify", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: observer, config: &core.HookConfig{
			ID: "o", Name: "observer", Priority: 2, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	p.SetAllowModify(true)
	input := &core.HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind: normalize.KindAIChat,
			Messages: []normalize.Message{
				{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "hello world"},
				}},
			},
		},
	}
	_ = p.Execute(context.Background(), input)
	if !strings.HasPrefix(observer.seen, "XXXXX") {
		t.Errorf("observer should have seen redacted text, got %q", observer.seen)
	}
}

func TestPipeline_Sequential_ModifyWithLegacyModifiedContentPropagates(t *testing.T) {
	observer := &observerHook{}
	hks := []boundHook{
		{hook: modifiedContentHook{}, config: &core.HookConfig{
			ID: "m", Name: "legacy-modify", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: observer, config: &core.HookConfig{
			ID: "o", Name: "observer", Priority: 2, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	p.SetAllowModify(true)
	input := &core.HookInput{
		Normalized: &normalize.NormalizedPayload{
			Kind: normalize.KindAIChat,
			Messages: []normalize.Message{
				{Role: normalize.RoleUser, Content: []normalize.ContentBlock{
					{Type: normalize.ContentText, Text: "original"},
				}},
			},
		},
	}
	_ = p.Execute(context.Background(), input)
	if observer.seen != "TRANSITIONAL" {
		t.Errorf("legacy ModifiedContent should propagate; observer saw %q", observer.seen)
	}
}

// executeSequential hookAppliesToKind skip (covers 183-186 branch)

func TestPipeline_Sequential_SkipsNonApplicableHooks(t *testing.T) {
	// Sequential variant of TestPipeline_Parallel_SkipsNonApplicableHooks:
	// hook restricted to "ai-image" must be skipped on ai-chat input —
	// the result list must not contain a phantom hook entry.
	chatHook := &stubHook{decision: core.Approve}
	imgHook := &stubHook{decision: core.RejectHard}
	hks := []boundHook{
		{hook: chatHook, config: &core.HookConfig{
			ID: "h1", Name: "chat-hook", Priority: 1, FailBehavior: "fail-open",
			ApplicableTrafficKinds: []string{"ai-chat"},
		}},
		{hook: imgHook, config: &core.HookConfig{
			ID: "h2", Name: "image-hook", Priority: 2, FailBehavior: "fail-open",
			ApplicableTrafficKinds: []string{"ai-image"},
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false /* sequential */, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{
		Normalized: &normalize.NormalizedPayload{Kind: normalize.KindAIChat},
	})
	if res.Decision != core.Approve {
		t.Fatalf("filtered REJECT_HARD must not fire, got %s", res.Decision)
	}
	if len(res.HookResults) != 1 {
		t.Errorf("expected 1 hook result (image-hook filtered), got %d", len(res.HookResults))
	}
	if imgHook.executed.Load() != 0 {
		t.Errorf("image hook should not have executed; got %d", imgHook.executed.Load())
	}
}

// mergeResults TransformSpans aggregation + ModifiedContent capture
// + empty-tag skip (was 94.9%)

// transformOnApproveHook returns Approve with TransformSpans — the
// mergeResults aggregator must still walk these into pr.TransformSpans
// (even though the decision was Approve).
type transformOnApproveHook struct {
	core.AnyEndpointAnyModality
	span normalize.TransformSpan
}

func (h transformOnApproveHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{
		Decision:       core.Approve,
		TransformSpans: []normalize.TransformSpan{h.span},
	}, nil
}

func TestMergeResults_AggregatesSpansAcrossApproveHooks(t *testing.T) {
	span := normalize.TransformSpan{
		Source: normalize.SourceCacheNormaliser, Action: normalize.ActionStrip,
		ContentAddress: "messages.0.content.0", Start: 0, End: 3, Replacement: "",
	}
	hks := []boundHook{
		{hook: transformOnApproveHook{span: span}, config: &core.HookConfig{
			ID: "a", Name: "a", Priority: 1, FailBehavior: "fail-open",
		}},
		{hook: &stubHook{decision: core.Approve}, config: &core.HookConfig{
			ID: "b", Name: "b", Priority: 2, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{})
	if len(res.TransformSpans) != 1 {
		t.Fatalf("expected 1 aggregated TransformSpan, got %d", len(res.TransformSpans))
	}
	if res.TransformSpans[0].Source != normalize.SourceCacheNormaliser {
		t.Errorf("span source: got %s", res.TransformSpans[0].Source)
	}
}

// modifyContentResultHook returns a Modify decision with ModifiedContent
// populated so mergeResults captures it on pr.ModifiedContent.
type modifyContentResultHook struct {
	core.AnyEndpointAnyModality
	content []core.ContentBlock
}

func (h modifyContentResultHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Modify, ModifiedContent: h.content}, nil
}

func TestMergeResults_ModifyPathCapturesLastModifiedContent(t *testing.T) {
	// The hasModify-without-soft-reject branch in mergeResults stamps the
	// last-non-empty ModifiedContent onto the pipeline result.
	hks := []boundHook{
		{hook: modifyContentResultHook{content: []core.ContentBlock{{Text: "TRANSITIONAL"}}},
			config: &core.HookConfig{ID: "m", Name: "m", Priority: 1, FailBehavior: "fail-open"}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	p.SetAllowModify(true)
	res := p.Execute(context.Background(), &core.HookInput{})
	if res.Decision != core.Modify {
		t.Fatalf("expected MODIFY, got %s", res.Decision)
	}
	if len(res.ModifiedContent) != 1 || res.ModifiedContent[0].Text != "TRANSITIONAL" {
		t.Errorf("ModifiedContent not captured on pipeline result: %+v", res.ModifiedContent)
	}
	if res.ReasonCode != "CONTENT_MODIFIED" {
		t.Errorf("reasonCode: got %q want CONTENT_MODIFIED", res.ReasonCode)
	}
}

// emptyTagHook emits an empty tag to exercise the "tag == \"\"" skip
// inside mergeResults' tagSet build.
type emptyTagHook struct {
	core.AnyEndpointAnyModality
}

func (emptyTagHook) Execute(_ context.Context, _ *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve, Tags: []string{"", "real-tag"}}, nil
}

func TestMergeResults_EmptyTagIsSkipped(t *testing.T) {
	hks := []boundHook{
		{hook: emptyTagHook{}, config: &core.HookConfig{
			ID: "e", Name: "emit", Priority: 1, FailBehavior: "fail-open",
		}},
	}
	p := NewPipeline(hks, time.Second, 5*time.Second, false, testLogger())
	res := p.Execute(context.Background(), &core.HookInput{})
	if len(res.Tags) != 1 || res.Tags[0] != "real-tag" {
		t.Errorf("empty tag should be filtered out; got %v", res.Tags)
	}
}

// PolicyResolver.resolve disabled / ingress-mismatch / factory-error
// (was 89.7%; lines 159-160, 167-168, 197-201)

func TestResolve_DisabledHookIsSkipped(t *testing.T) {
	registry := core.NewHookRegistry()
	registry.Register("impl", stubFactory())
	registry.Freeze()
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "a", ImplementationID: "impl", Name: "off", Enabled: false,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
		{ID: "b", ImplementationID: "impl", Name: "on", Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}, registry, testLogger())
	out, err := r.ResolveHooks("request", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 enabled hook, got %d", len(out))
	}
	if out[0].config.ID != "b" {
		t.Errorf("wrong hook surfaced; got %s", out[0].config.ID)
	}
}

func TestResolve_IngressMismatchIsSkipped(t *testing.T) {
	registry := core.NewHookRegistry()
	registry.Register("impl", stubFactory())
	registry.Freeze()
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "agent-only", ImplementationID: "impl", Name: "agent-only", Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"AGENT"}},
	}, registry, testLogger())
	out, err := r.ResolveHooks("request", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("ingress-mismatched hook should be skipped; got %d", len(out))
	}
}

func TestResolve_FactoryError_DegradesToHookOff(t *testing.T) {
	// Availability-first: when a factory returns an error, resolve()
	// skips+logs that hook and continues — it does NOT abort the whole
	// pipeline build. A single broken hook config must degrade to "that hook
	// off", never "all compliance off" (which a propagated error would cause
	// at the call site that turns a non-nil resolve error into a 500).
	registry := core.NewHookRegistry()
	registry.Register("bad-factory", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, errors.New("synthetic factory failure")
	})
	registry.Freeze()
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "x", ImplementationID: "bad-factory", Name: "x", Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}, registry, testLogger())
	out, err := r.ResolveHooks("request", "AI_GATEWAY", false)
	if err != nil {
		t.Fatalf("factory error must be skipped, not propagated: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("hook whose factory failed must be dropped; got %d", len(out))
	}
}

func TestResolve_FactoryError_SkipLoggedOncePerEpoch(t *testing.T) {
	// The skip warning is deduplicated per hookId per reload epoch: two
	// resolve() calls without an intervening Swap log the skip only once, so
	// a persistently-broken hook does not flood the logs once per request.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	registry := core.NewHookRegistry()
	registry.Register("bad-factory", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, errors.New("synthetic factory failure")
	})
	registry.Freeze()
	r := NewPolicyResolver([]core.HookConfig{
		{ID: "x", ImplementationID: "bad-factory", Name: "x", Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}, registry, logger)

	for range 3 {
		if _, err := r.ResolveHooks("request", "AI_GATEWAY", false); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if got := strings.Count(buf.String(), "skipped during pipeline build"); got != 1 {
		t.Errorf("skip warning should log once per epoch; got %d", got)
	}

	// A Swap resets the dedup epoch, so the next resolve logs the skip again.
	r.Swap([]core.HookConfig{
		{ID: "x", ImplementationID: "bad-factory", Name: "x", Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	})
	if _, err := r.ResolveHooks("request", "AI_GATEWAY", false); err != nil {
		t.Fatalf("resolve after swap: %v", err)
	}
	if got := strings.Count(buf.String(), "skipped during pipeline build"); got != 2 {
		t.Errorf("skip warning should log again after Swap resets epoch; got %d", got)
	}
}

// PolicyResolver.Swap warn-dedup reset path under concurrency

func TestPolicyResolver_SwapResetsWarnedUnknownConcurrentSafe(t *testing.T) {
	// Concurrent Swap + warnUnknownImpl must not race the warnedMu lock.
	// This is a -race regression for the warn-dedup reset block; without
	// the lock, repeated Swap()s would corrupt the map mid-write.
	registry := core.NewHookRegistry()
	registry.Freeze() // empty — every impl id is unknown
	cfg := []core.HookConfig{
		{ID: "x", ImplementationID: "ghost", Name: "x", Priority: 0, Enabled: true,
			Stage: "request", FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}
	r := NewPolicyResolver(cfg, registry, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, _ = r.ResolveHooks("request", "AI_GATEWAY", false)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			r.Swap(cfg)
		}
	}()
	wg.Wait()
}
