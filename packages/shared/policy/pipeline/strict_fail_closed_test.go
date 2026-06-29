package pipeline

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// These tests pin the strictFailClosed contract added to ResolveHooks /
// BuildPipeline. The contract has three load-bearing cases:
//
//	(a) strict=true  + fail-closed unbuildable hook → error (reverse-proxy refuses)
//	(b) strict=true  + fail-OPEN  unbuildable hook → skipped, pipeline still builds
//	    (availability-first resilience preserved)
//	(c) strict=false + fail-closed unbuildable hook → skipped (NE host-network
//	    fail-open SAFETY case — a build error must never refuse, which would take
//	    down the host's outbound networking)
//
// "Unbuildable" is exercised across all three skip branches: unknown
// implementationId (no factory), factory build error, and connection-stage
// incompatibility.

// TestStrictFailClosed_UnknownImpl covers the no-factory branch.
func TestStrictFailClosed_UnknownImpl(t *testing.T) {
	registry := core.NewHookRegistry() // empty: every ImplementationID is unknown

	failClosedCfg := []core.HookConfig{
		{ID: "enforcer", ImplementationID: "no-such-impl", Name: "mandatory-enforcer",
			Stage: "request", Enabled: true, FailBehavior: "fail-closed",
			ApplicableIngress: []string{"ALL"}},
	}
	failOpenCfg := []core.HookConfig{
		{ID: "advisory", ImplementationID: "no-such-impl", Name: "advisory",
			Stage: "request", Enabled: true, FailBehavior: "fail-open",
			ApplicableIngress: []string{"ALL"}},
	}

	// (a) strict=true + fail-closed unbuildable → error.
	t.Run("strict_failclosed_returns_error", func(t *testing.T) {
		r := NewPolicyResolver(failClosedCfg, registry, testLogger())
		pipe, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			time.Second, 5*time.Second, false, true, testLogger())
		if err == nil {
			t.Fatalf("expected error for fail-closed unbuildable hook under strict, got pipe=%v", pipe)
		}
		if pipe != nil {
			t.Fatalf("expected nil pipeline on refusal, got %+v", pipe)
		}
		if !errors.Is(err, errFailClosedUnbuildable) {
			t.Fatalf("error must wrap errFailClosedUnbuildable; got %v", err)
		}
		if !strings.Contains(err.Error(), "enforcer") || !strings.Contains(err.Error(), "no-such-impl") {
			t.Fatalf("error must name the offending hook id + impl; got %q", err.Error())
		}
	})

	// (b) strict=true + fail-OPEN unbuildable → skipped, build succeeds (no hooks
	// left so BuildPipeline returns nil,nil — the "nothing to run" signal).
	t.Run("strict_failopen_still_skipped", func(t *testing.T) {
		r := NewPolicyResolver(failOpenCfg, registry, testLogger())
		pipe, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			time.Second, 5*time.Second, false, true, testLogger())
		if err != nil {
			t.Fatalf("fail-OPEN hook must be skipped even under strict, not error: %v", err)
		}
		if pipe != nil {
			t.Fatalf("the only hook was skipped; expected nil pipeline, got %+v", pipe)
		}
	})

	// (c) strict=false + fail-closed unbuildable → skipped (host-network fail-open
	// SAFETY case). This is the binding rule: a build error on an in-path
	// interceptor must NEVER refuse.
	t.Run("nonstrict_failclosed_still_skipped", func(t *testing.T) {
		r := NewPolicyResolver(failClosedCfg, registry, testLogger())
		pipe, err := r.BuildPipeline("request", "AI_GATEWAY", "", nil,
			time.Second, 5*time.Second, false, false, testLogger())
		if err != nil {
			t.Fatalf("strict=false MUST preserve fail-open skip even for a fail-closed hook (host-network safety); got error: %v", err)
		}
		if pipe != nil {
			t.Fatalf("the only hook was skipped; expected nil pipeline, got %+v", pipe)
		}
	})
}

// TestStrictFailClosed_FactoryError covers the factory-build-error branch and
// proves that under strict the surviving healthy hook is NOT enough to mask a
// fail-closed enforcer that failed to build.
func TestStrictFailClosed_FactoryError(t *testing.T) {
	registry := core.NewHookRegistry()
	registry.Register("boom", func(_ *core.HookConfig) (core.Hook, error) {
		return nil, fmt.Errorf("synthetic factory failure")
	})
	registry.Register("ok", func(_ *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})

	cfg := []core.HookConfig{
		{ID: "enforcer", ImplementationID: "boom", Name: "mandatory", Priority: 0,
			Stage: "request", Enabled: true, FailBehavior: "fail-closed", ApplicableIngress: []string{"ALL"}},
		{ID: "advisory", ImplementationID: "ok", Name: "advisory", Priority: 1,
			Stage: "request", Enabled: true, FailBehavior: "fail-open", ApplicableIngress: []string{"ALL"}},
	}

	t.Run("strict_aborts_despite_healthy_sibling", func(t *testing.T) {
		r := NewPolicyResolver(cfg, registry, testLogger())
		_, err := r.ResolveHooks("request", "AI_GATEWAY", true)
		if err == nil {
			t.Fatal("strict resolve must abort when a fail-closed hook's factory errors, even with a healthy sibling")
		}
		if !strings.Contains(err.Error(), "synthetic factory failure") {
			t.Fatalf("error must wrap the underlying factory error; got %q", err.Error())
		}
	})

	t.Run("nonstrict_skips_keeps_sibling", func(t *testing.T) {
		r := NewPolicyResolver(cfg, registry, testLogger())
		hks, err := r.ResolveHooks("request", "AI_GATEWAY", false)
		if err != nil {
			t.Fatalf("strict=false must skip the broken fail-closed hook, not error: %v", err)
		}
		if len(hks) != 1 || hks[0].config.ID != "advisory" {
			t.Fatalf("expected only the healthy advisory hook to survive; got %+v", hks)
		}
	})
}

// TestStrictFailClosed_ConnectionIncompatible covers the connection-stage
// incompatibility branch (a MODIFY-capable hook bound to the connection stage).
// noopHook is not ConnectionStageCompatible, so it triggers this branch.
func TestStrictFailClosed_ConnectionIncompatible(t *testing.T) {
	registry := core.NewHookRegistry()
	registry.Register("noop", func(_ *core.HookConfig) (core.Hook, error) {
		return &noopHook{}, nil
	})

	mk := func(fail string) []core.HookConfig {
		return []core.HookConfig{
			{ID: "conn", ImplementationID: "noop", Name: "conn-hook",
				Stage: "connection", Enabled: true, FailBehavior: fail},
		}
	}

	t.Run("strict_failclosed_returns_error", func(t *testing.T) {
		r := NewPolicyResolver(mk("fail-closed"), registry, testLogger())
		_, err := r.ResolveHooks("connection", "AI_GATEWAY", true)
		if err == nil {
			t.Fatal("strict resolve must error when a fail-closed connection hook is incompatible")
		}
		if !errors.Is(err, errFailClosedUnbuildable) {
			t.Fatalf("error must wrap errFailClosedUnbuildable; got %v", err)
		}
		if !strings.Contains(err.Error(), "connection-stage compatible") {
			t.Fatalf("error must explain the connection-stage incompatibility; got %q", err.Error())
		}
	})

	t.Run("nonstrict_failclosed_still_skipped", func(t *testing.T) {
		r := NewPolicyResolver(mk("fail-closed"), registry, testLogger())
		hks, err := r.ResolveHooks("connection", "AI_GATEWAY", false)
		if err != nil {
			t.Fatalf("strict=false must skip incompatible connection hook even when fail-closed (host-network safety); got %v", err)
		}
		if len(hks) != 0 {
			t.Fatalf("incompatible hook must be dropped; got %+v", hks)
		}
	})
}

// TestStrictFailClosed_CaseInsensitive proves the FailBehavior match is
// case-insensitive (EqualFold), matching the pipeline.go:Execute precedent.
func TestStrictFailClosed_CaseInsensitive(t *testing.T) {
	registry := core.NewHookRegistry() // unknown impl
	for _, fb := range []string{"FAIL-CLOSED", "Fail-Closed", "fail-closed"} {
		cfg := []core.HookConfig{
			{ID: "e", ImplementationID: "missing", Name: "e",
				Stage: "request", Enabled: true, FailBehavior: fb, ApplicableIngress: []string{"ALL"}},
		}
		r := NewPolicyResolver(cfg, registry, testLogger())
		if _, err := r.ResolveHooks("request", "AI_GATEWAY", true); err == nil {
			t.Fatalf("FailBehavior=%q should be treated as fail-closed under strict", fb)
		}
	}
}

// TestStrictFailClosed_ComplianceProxyAppliance is the regression: the
// compliance-proxy is a dedicated forward-proxy appliance that opts into strict
// fail-closed (tlsbump.WithStrictFailClosed). An admin-configured fail-closed
// hook that is UNBUILDABLE must therefore make BuildPipeline ERROR — so the
// appliance refuses the request rather than forwarding it uninspected — while the
// agent NE host-packet path (strict=false) still fails open.
func TestStrictFailClosed_ComplianceProxyAppliance(t *testing.T) {
	registry := core.NewHookRegistry() // empty → unbuildable
	cfg := []core.HookConfig{
		{ID: "pii-block", ImplementationID: "no-such-impl", Name: "mandatory-pii-block",
			Stage: "request", Enabled: true, FailBehavior: "fail-closed",
			ApplicableIngress: []string{"ALL"}},
	}

	// Compliance-proxy appliance: strict=true → REFUSE (error, nil pipeline).
	t.Run("compliance_proxy_refuses", func(t *testing.T) {
		r := NewPolicyResolver(cfg, registry, testLogger())
		pipe, err := r.BuildPipeline("request", "COMPLIANCE_PROXY", "", nil,
			time.Second, 5*time.Second, false, true, testLogger())
		if err == nil || pipe != nil {
			t.Fatalf("compliance-proxy must refuse an unbuildable fail-closed hook; got pipe=%v err=%v", pipe, err)
		}
		if !errors.Is(err, errFailClosedUnbuildable) {
			t.Fatalf("error must wrap errFailClosedUnbuildable; got %v", err)
		}
	})

	// Agent NE host-packet path: strict=false → STILL fail-open (skip, build ok).
	t.Run("agent_ne_stays_fail_open", func(t *testing.T) {
		r := NewPolicyResolver(cfg, registry, testLogger())
		pipe, err := r.BuildPipeline("request", "AGENT", "", nil,
			time.Second, 5*time.Second, false, false, testLogger())
		if err != nil {
			t.Fatalf("agent NE host path must stay fail-open (skip), not refuse; got error: %v", err)
		}
		if pipe != nil {
			t.Fatalf("the only hook was skipped; expected nil pipeline, got %+v", pipe)
		}
	})
}
