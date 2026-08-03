package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/matcher"
)

// benchPrescanHook mirrors prescanHook (unionprescan_test.go) but takes a
// testing.TB so benchmarks can build one. It is a faithful stand-in for a
// production content-scanning hook: it reports ScansContent and exports
// prescan patterns, so BuildPipeline's union-prescan and max-pattern-bound
// memos are exercised on the same code path production takes.
type benchPrescanHook struct {
	core.ChatOnly
	m        matcher.Matcher
	stripped []core.PrescanPattern
}

func newBenchPrescanHook(tb testing.TB, exprs ...string) *benchPrescanHook {
	tb.Helper()
	var pats []matcher.Pattern
	var exported []core.PrescanPattern
	for i, e := range exprs {
		s, err := matcher.StripAnchors(e)
		if err != nil {
			tb.Fatalf("strip %q: %v", e, err)
		}
		pats = append(pats, matcher.Pattern{ID: i, Expr: s})
		exported = append(exported, core.PrescanPattern{Expr: s})
	}
	m, bad := matcher.CompileDefault(pats)
	if len(bad) > 0 {
		tb.Fatalf("compile: %v", bad)
	}
	return &benchPrescanHook{m: m, stripped: exported}
}

func (h *benchPrescanHook) Execute(context.Context, *core.HookInput) (*core.HookResult, error) {
	return &core.HookResult{Decision: core.Approve}, nil
}
func (h *benchPrescanHook) ScansContent() bool { return true }
func (h *benchPrescanHook) MayMatchRaw(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	return len(h.m.Scan([]string{string(body)}, true)) > 0
}
func (h *benchPrescanHook) PrescanPatterns() []core.PrescanPattern { return h.stripped }

// benchResolver builds a PolicyResolver shaped like a production hooks-ON
// deployment: the five content hooks the perf rig enables (pii-scanner,
// keyword-blocker, request-content-safety, pii-outbound-scanner,
// response-content-safety) split across the request and response stages,
// plus decoy configs that every BuildPipeline call must filter out —
// disabled hooks, other-stage hooks, and other-ingress hooks. The decoys
// matter: resolveFrom walks the FULL config slice on every call, so the
// filter loop's cost is part of what the benchmark measures.
func benchResolver(tb testing.TB) *PolicyResolver {
	tb.Helper()

	// Patterns are representative of the PII rule shapes the rig runs
	// (email, card-like digit run, SSN-like grouping).
	specs := []struct {
		id, impl, stage string
		exprs           []string
	}{
		{"pii-scanner", "impl-pii", "request", []string{`[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`, `[0-9]{16}`}},
		{"keyword-blocker", "impl-kw", "request", []string{`confidential`, `internal-only`}},
		{"request-content-safety", "impl-rcs", "request", []string{`[0-9]{3}-[0-9]{2}-[0-9]{4}`}},
		{"pii-outbound-scanner", "impl-pii-out", "response", []string{`[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`, `[0-9]{16}`}},
		{"response-content-safety", "impl-rsafety", "response", []string{`[0-9]{3}-[0-9]{2}-[0-9]{4}`}},
	}

	reg := core.NewHookRegistry()
	cfgs := make([]core.HookConfig, 0, len(specs)+12)

	for i, s := range specs {
		hook := newBenchPrescanHook(tb, s.exprs...)
		reg.Register(s.impl, func(_ *core.HookConfig) (core.Hook, error) { return hook, nil })
		cfgs = append(cfgs, core.HookConfig{
			ID:                s.id,
			ImplementationID:  s.impl,
			Name:              s.id,
			Enabled:           true,
			Stage:             s.stage,
			ApplicableIngress: []string{"ALL"},
			Priority:          100 - i, // reversed so the sort has real work to do
			Config:            map[string]any{"onMatch": map[string]any{"action": "redact"}},
		})
	}

	// Decoys the filter loop must reject on every call.
	reg.Register("impl-decoy", func(_ *core.HookConfig) (core.Hook, error) { return metaHook{}, nil })
	for i := range 4 {
		cfgs = append(cfgs,
			core.HookConfig{ // disabled
				ID: fmt.Sprintf("decoy-disabled-%d", i), ImplementationID: "impl-decoy",
				Enabled: false, Stage: "request", ApplicableIngress: []string{"ALL"},
			},
			core.HookConfig{ // connection stage — never matches request/response
				ID: fmt.Sprintf("decoy-stage-%d", i), ImplementationID: "impl-decoy",
				Enabled: true, Stage: "connection", ApplicableIngress: []string{"ALL"},
			},
			core.HookConfig{ // other ingress
				ID: fmt.Sprintf("decoy-ingress-%d", i), ImplementationID: "impl-decoy",
				Enabled: true, Stage: "request", ApplicableIngress: []string{"AI_GATEWAY"},
			},
		)
	}

	return NewPolicyResolver(cfgs, reg, testLogger())
}

// benchBuild runs BuildPipeline the way the hot path does and fails the
// benchmark if the pipeline it produced is not the expected shape — a
// benchmark that silently measures a nil pipeline would report a fantastic
// number for doing nothing.
func benchBuild(b *testing.B, r *PolicyResolver, stage, ingress string, ep core.EndpointType) {
	b.Helper()
	lg := testLogger()

	// Warm the per-generation memos (union prescan, max pattern bound) and the
	// hook cache exactly as the first production request would, so the loop
	// measures STEADY-STATE cost, which is what every request after the first
	// pays.
	warm, err := r.BuildPipeline(stage, ingress, ep, nil, 5*time.Second, 30*time.Second, false, true, lg)
	if err != nil {
		b.Fatalf("warmup BuildPipeline: %v", err)
	}
	if warm == nil {
		b.Fatal("warmup BuildPipeline returned nil — benchmark would measure nothing")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		p, err := r.BuildPipeline(stage, ingress, ep, nil, 5*time.Second, 30*time.Second, false, true, lg)
		if err != nil || p == nil {
			b.Fatalf("BuildPipeline: err=%v nil=%v", err, p == nil)
		}
	}
}

// BenchmarkBuildPipeline_Request measures the per-REQUEST cost the bumped
// compliance path pays at tlsbump/forward_request_phase.go:147.
func BenchmarkBuildPipeline_Request(b *testing.B) {
	benchBuild(b, benchResolver(b), "request", "COMPLIANCE_PROXY", "")
}

// BenchmarkBuildPipeline_Response measures the per-RESPONSE cost at
// tlsbump/forward_response_phase.go:130. The response stage additionally
// resolves the cached max-pattern-bound, so it is the heavier of the two.
func BenchmarkBuildPipeline_Response(b *testing.B) {
	benchBuild(b, benchResolver(b), "response", "COMPLIANCE_PROXY", "")
}

// BenchmarkBuildPipeline_ResponseEndpointGated adds the endpoint gate that
// the classified path passes, exercising the SupportsEndpoint filter branch.
func BenchmarkBuildPipeline_ResponseEndpointGated(b *testing.B) {
	benchBuild(b, benchResolver(b), "response", "COMPLIANCE_PROXY", core.EndpointTypeChat)
}

// BenchmarkBuildPipeline_RequestParallel measures the same request-stage build
// under concurrency. resolveFrom takes r.hookMu.RLock once per surviving hook,
// so this is where reader-lock cache-line contention would show up: a
// per-request build that is cheap single-threaded can still be the bottleneck
// at proxy concurrency.
func BenchmarkBuildPipeline_RequestParallel(b *testing.B) {
	r := benchResolver(b)
	lg := testLogger()
	if _, err := r.BuildPipeline("request", "COMPLIANCE_PROXY", "", nil, 5*time.Second, 30*time.Second, false, true, lg); err != nil {
		b.Fatalf("warmup: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p, err := r.BuildPipeline("request", "COMPLIANCE_PROXY", "", nil, 5*time.Second, 30*time.Second, false, true, lg)
			if err != nil || p == nil {
				b.Fatalf("BuildPipeline: err=%v nil=%v", err, p == nil)
			}
		}
	})
}
