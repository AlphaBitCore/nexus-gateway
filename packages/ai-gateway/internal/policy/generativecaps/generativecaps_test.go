package generativecaps

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// resetResolveForTest re-runs env resolution — the package caches the effective
// table once, so a test that sets env must reset before Lookup reflects it.
func resetResolveForTest() {
	resolveOnce = sync.Once{}
	resolved = nil
}

func TestLookup_BuiltInDefaults(t *testing.T) {
	resetResolveForTest()
	cases := []struct {
		kind       typology.EndpointKind
		generative bool
		conc       int
		maxBytes   int64
	}{
		{typology.EndpointKindImageGeneration, true, 4, 256 << 10},
		{typology.EndpointKindTTS, true, 8, 256 << 10},
		{typology.EndpointKindVideoGeneration, true, 2, 16 << 20}, // multipart-appropriate ceiling (optional input_reference image part)
		{typology.EndpointKindSTT, true, 4, 26 << 20},             // multipart-appropriate ceiling (audio, not JSON)
		{typology.EndpointKindGuardrail, true, 4, 1 << 20},        // judge-budget bound (not generative, but paid-judge DoS surface)
		{typology.EndpointKindRealtime, true, 2, 16 << 20},        // sessions-per-VK bound; bytes = per-WS-frame ceiling (SetReadLimit)
		{typology.EndpointKindChat, false, 0, 0},
		{typology.EndpointKindEmbeddings, false, 0, 0},
	}
	for _, c := range cases {
		caps, gen := Lookup(c.kind)
		if gen != c.generative {
			t.Fatalf("%s: generative=%v want %v", c.kind, gen, c.generative)
		}
		if gen && (caps.MaxConcurrentPerVK != c.conc || caps.MaxRequestBytes != c.maxBytes) {
			t.Fatalf("%s: caps=%+v want conc=%d bytes=%d", c.kind, caps, c.conc, c.maxBytes)
		}
	}
}

func TestLookup_EnvOverride(t *testing.T) {
	resetResolveForTest()
	t.Setenv("AI_GATEWAY_GENERATIVE_CAP_IMAGE_GENERATION_CONCURRENCY", "1")
	t.Setenv("AI_GATEWAY_GENERATIVE_CAP_IMAGE_GENERATION_MAX_BYTES", "1024")
	caps, gen := Lookup(typology.EndpointKindImageGeneration)
	if !gen || caps.MaxConcurrentPerVK != 1 || caps.MaxRequestBytes != 1024 {
		t.Fatalf("env override not applied: %+v gen=%v", caps, gen)
	}
}

func TestLookup_GarbageEnvFallsBack(t *testing.T) {
	resetResolveForTest()
	t.Setenv("AI_GATEWAY_GENERATIVE_CAP_TTS_CONCURRENCY", "not-a-number")
	t.Setenv("AI_GATEWAY_GENERATIVE_CAP_TTS_MAX_BYTES", "-5")
	caps, _ := Lookup(typology.EndpointKindTTS)
	if caps.MaxConcurrentPerVK != 8 || caps.MaxRequestBytes != 256<<10 {
		t.Fatalf("garbage env must fall back to built-in default, got %+v", caps)
	}
}

func TestLookup_EnvZeroDisablesConcurrency(t *testing.T) {
	resetResolveForTest()
	t.Setenv("AI_GATEWAY_GENERATIVE_CAP_IMAGE_GENERATION_CONCURRENCY", "0")
	caps, gen := Lookup(typology.EndpointKindImageGeneration)
	// Still a generative kind (size cap intact), but concurrency explicitly
	// unlimited — an operator's deliberate choice.
	if !gen || caps.MaxConcurrentPerVK != 0 || caps.MaxRequestBytes != 256<<10 {
		t.Fatalf("env 0 should disable concurrency only, got %+v gen=%v", caps, gen)
	}
}

func TestVKConcurrency_AcquireReleaseOverLimit(t *testing.T) {
	c := NewVKConcurrency()
	k, vk := typology.EndpointKindImageGeneration, "vk-1"
	// 3 admits up to the cap.
	for i := range 3 {
		if !c.Acquire(k, vk, 3) {
			t.Fatalf("acquire %d should admit under cap 3", i)
		}
	}
	// 4th over the cap.
	if c.Acquire(k, vk, 3) {
		t.Fatal("4th acquire must be rejected at cap 3")
	}
	// Release one → a slot frees.
	c.Release(k, vk)
	if !c.Acquire(k, vk, 3) {
		t.Fatal("acquire after release should admit")
	}
	// A different VK is independent.
	if !c.Acquire(k, "vk-2", 3) {
		t.Fatal("a different VK must not share vk-1's slots")
	}
	// A different kind for the same VK is independent.
	if !c.Acquire(typology.EndpointKindTTS, vk, 1) {
		t.Fatal("a different kind must not share the image bucket")
	}
}

func TestVKConcurrency_UnlimitedWhenMaxNonPositive(t *testing.T) {
	c := NewVKConcurrency()
	for range 1000 {
		if !c.Acquire(typology.EndpointKindImageGeneration, "vk", 0) {
			t.Fatal("max<=0 must always admit (unlimited)")
		}
	}
}

func TestVKConcurrency_DoubleReleaseUnderflowGuard(t *testing.T) {
	c := NewVKConcurrency()
	k, vk := typology.EndpointKindImageGeneration, "vk"
	c.Acquire(k, vk, 2)
	c.Release(k, vk)
	c.Release(k, vk) // spurious extra release — must not drive the counter negative
	// The counter must not have gone negative (which would over-admit): with a
	// cap of 1, exactly one acquire should now be admitted and the second not.
	if !c.Acquire(k, vk, 1) {
		t.Fatal("counter went negative — over-admits after a double release")
	}
	if c.Acquire(k, vk, 1) {
		t.Fatal("second acquire past cap 1 should be rejected — counter was left negative")
	}
}

// TestVKConcurrency_RaceSafe asserts the invariant that matters: the number of
// requests SIMULTANEOUSLY HOLDING a slot (Acquire returned true, Release not
// yet called) never exceeds the cap. The internal counter may briefly overshoot
// under contention (goroutines that lose the increment-then-check race add then
// immediately subtract) — that is the admission-gate pattern and is harmless
// because those goroutines are rejected; only the true-returning holders count.
func TestVKConcurrency_RaceSafe(t *testing.T) {
	c := NewVKConcurrency()
	k, vk := typology.EndpointKindImageGeneration, "vk"
	const capN = 5
	var held atomic.Int64 // true holders currently in the critical section
	var peak atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !c.Acquire(k, vk, capN) {
				return
			}
			cur := held.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			held.Add(-1)
			c.Release(k, vk)
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > capN {
		t.Fatalf("observed %d slot-holders, cap %d — admitted over the cap", got, capN)
	}
	if got := peak.Load(); got == 0 {
		t.Fatal("expected some concurrency to be observed")
	}
}
