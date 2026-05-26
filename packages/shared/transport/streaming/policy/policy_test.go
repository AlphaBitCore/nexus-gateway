package policy_test

import (
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

func TestResolve_NilOverrideReturnsGlobal(t *testing.T) {
	g := policy.DefaultPolicy()
	got := policy.Resolve(g, nil)
	if got != g {
		t.Fatalf("nil override should be no-op; got %+v want %+v", got, g)
	}
}

func TestResolve_FullOverride(t *testing.T) {
	g := policy.DefaultPolicy()
	mode := policy.ModeChunkedAsync
	chunk := 16384
	timeout := 5000
	maxBuf := 32 << 20
	fail := policy.FailClose
	tr := true
	override := &policy.Override{
		Mode: &mode, ChunkBytes: &chunk, HookTimeoutMs: &timeout,
		MaxBufferBytes: &maxBuf, FailBehavior: &fail,
		CaptureRequestBody: &tr, CaptureResponseBody: &tr, RawSpillEnabled: &tr,
	}
	got := policy.Resolve(g, override)
	if got.Mode != mode || got.ChunkBytes != chunk || got.HookTimeoutMs != timeout ||
		got.MaxBufferBytes != maxBuf || got.FailBehavior != fail ||
		!got.CaptureRequestBody || !got.CaptureResponseBody || !got.RawSpillEnabled {
		t.Fatalf("full override not applied: %+v", got)
	}
}

func TestResolve_PartialOverrideInheritsRest(t *testing.T) {
	g := policy.DefaultPolicy()
	mode := policy.ModeBufferFullBlock
	override := &policy.Override{Mode: &mode}
	got := policy.Resolve(g, override)
	if got.Mode != mode {
		t.Fatalf("mode override not applied: %s", got.Mode)
	}
	if got.ChunkBytes != g.ChunkBytes || got.HookTimeoutMs != g.HookTimeoutMs ||
		got.FailBehavior != g.FailBehavior || got.CaptureRequestBody != g.CaptureRequestBody {
		t.Fatalf("partial override leaked into other fields: %+v", got)
	}
}

func TestPolicy_IsValid(t *testing.T) {
	if !policy.DefaultPolicy().IsValid() {
		t.Fatal("default policy must be valid")
	}
	bad := policy.DefaultPolicy()
	bad.Mode = "garbage"
	if bad.IsValid() {
		t.Fatal("garbage mode should be invalid")
	}
	bad = policy.DefaultPolicy()
	bad.FailBehavior = "wat"
	if bad.IsValid() {
		t.Fatal("garbage fail behavior should be invalid")
	}
	bad = policy.DefaultPolicy()
	bad.ChunkBytes = -1
	if bad.IsValid() {
		t.Fatal("negative ChunkBytes should be invalid")
	}
}
