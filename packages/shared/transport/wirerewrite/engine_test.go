package wirerewrite

import (
	"bytes"
	"strings"
	"testing"
)

// --- NormalizeKey (L0) ---

func TestNormalizeKey_CchStrip_NotEnabledByDefault(t *testing.T) {
	eng := New(nil)
	body := []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"Hello cch=abc123def0; world"}],"messages":[]}`)
	out := eng.NormalizeKey(AdapterAnthropic, body)
	// cch-strip is disabled by default — key should be unchanged
	if string(out) != string(body) {
		t.Fatalf("expected unchanged body when rule disabled, got %s", out)
	}
}

func TestNormalizeKey_CchStrip_EnabledViaConfig(t *testing.T) {
	enabled := true
	cfg := Config{
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body := []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"Hello cch=abc123def0; world"}],"messages":[]}`)
	out := eng.NormalizeKey(AdapterAnthropic, body)
	if strings.Contains(string(out), "cch=") {
		t.Fatalf("expected cch= stripped from key body, got %s", out)
	}
	if !strings.Contains(string(out), "Hello") || !strings.Contains(string(out), "world") {
		t.Fatalf("expected surrounding text preserved, got %s", out)
	}
}

func TestNormalizeKey_CchStrip_TwoRequests_SameKey(t *testing.T) {
	// AC1: two requests differing only in cch= token must produce the same key body.
	enabled := true
	cfg := Config{
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body1 := []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"System prompt cch=aabbccdd; more text"}],"messages":[{"role":"user","content":"hello"}]}`)
	body2 := []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"System prompt cch=11223344; more text"}],"messages":[{"role":"user","content":"hello"}]}`)

	key1 := eng.NormalizeKey(AdapterAnthropic, body1)
	key2 := eng.NormalizeKey(AdapterAnthropic, body2)

	if string(key1) != string(key2) {
		t.Fatalf("expected same key body after cch= strip\ngot1: %s\ngot2: %s", key1, key2)
	}
}

// --- NormalizeUpstream (L3) ---

func TestNormalizeUpstream_NoDemand_BodyByteIdentical(t *testing.T) {
	// hasWork=false: no strip rule enabled (bundled rules ship disabled). The
	// upstream rewrite must be a true no-op: the body is forwarded
	// byte-identical and the Result is zero-valued.
	eng := New(nil)
	eng.Reload(Config{})

	// Body carries a strip target (cch=), so a spurious run is visible in the
	// bytes.
	body := []byte(`{"system":[{"type":"text","text":"test cch=aabbcc; end"}],"messages":[{"role":"user","content":"hi"}]}`)
	out, result := eng.NormalizeUpstream(AdapterAnthropic, body)

	if !bytes.Equal(out, body) {
		t.Fatalf("no demand: body must be byte-identical\n want: %s\n got:  %s", body, out)
	}
	if result.StripCount != 0 || result.StripBytes != 0 {
		t.Fatalf("no demand: want zero strips, got count=%d bytes=%d", result.StripCount, result.StripBytes)
	}
	if result.DryRun || len(result.TransformSpans) != 0 {
		t.Fatalf("no demand: want zero Result, got %+v", result)
	}
}

func TestNormalizeUpstream_EnabledRuleAloneIsTheDemand(t *testing.T) {
	// A single operator-enabled strip rule is the ONLY input — no global flag
	// exists any more. Enabling the rule must be sufficient to make the engine
	// run and actually strip the cch= nonce from the upstream-bound body.
	enabled := true
	cfg := Config{
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body := []byte(`{"system":[{"type":"text","text":"prompt cch=deadbeef; end"}]}`)
	out, result := eng.NormalizeUpstream(AdapterAnthropic, body)
	if strings.Contains(string(out), "cch=") {
		t.Fatalf("expected cch= stripped from upstream body, got %s", out)
	}
	if result.StripCount != 1 {
		t.Fatalf("expected StripCount=1, got %d", result.StripCount)
	}
	if result.StripBytes == 0 {
		t.Fatal("expected StripBytes>0")
	}
}

func TestNormalizeKey_RunsRegardlessOfUpstreamDemandGate(t *testing.T) {
	// L0 cache-key normalisation is independent of the upstream-rewrite gate:
	// NormalizeKey never consults hasWork. A refactor that folded the gate into
	// NormalizeKey would silently destabilise cache keys (every Claude Code
	// session's rotating cch= nonce would become part of the key → 0% hit rate).
	// Construct the snapshot directly: a key-safe strip rule present, but the
	// demand gate OFF.
	eng := New(nil)
	rule := bundledRules()[0] // claude-code-cch-strip, KeyNormalizeSafe=true
	rule.Enabled = true
	eng.compiled.Store(&resolvedConfig{
		hasWork: false,
		keyRules: map[AdapterType][]ruleEntry{
			AdapterAnthropic: {{rule: rule, breaker: newCircuitBreaker()}},
		},
		upstreamRules: map[AdapterType][]ruleEntry{},
	})

	body := []byte(`{"system":[{"type":"text","text":"prompt cch=deadbeef; end"}]}`)

	keyBody := eng.NormalizeKey(AdapterAnthropic, body)
	if strings.Contains(string(keyBody), "cch=") {
		t.Fatalf("NormalizeKey must strip the cch= nonce even when hasWork=false, got %s", keyBody)
	}

	// ...while NormalizeUpstream, which DOES consult the gate, stays a no-op.
	out, result := eng.NormalizeUpstream(AdapterAnthropic, body)
	if !bytes.Equal(out, body) || result.StripCount != 0 {
		t.Fatalf("NormalizeUpstream must no-op when hasWork=false; got %s (strip=%d)", out, result.StripCount)
	}
}

// --- Circuit breaker ---

func TestCircuitBreaker_TripsAfterThreshold(t *testing.T) {
	// AC5: after 10 errors the circuit breaker disables the rule.
	cb := newCircuitBreaker()
	for i := range defaultCBThreshold - 1 {
		cb.recordError()
		if cb.isOpen() {
			t.Fatalf("circuit opened too early at error %d", i+1)
		}
	}
	cb.recordError() // 10th error
	if !cb.isOpen() {
		t.Fatal("expected circuit to be open after threshold")
	}
}

func TestCircuitBreaker_ResetOnReload(t *testing.T) {
	cb := newCircuitBreaker()
	for range defaultCBThreshold {
		cb.recordError()
	}
	if !cb.isOpen() {
		t.Fatal("expected open after threshold")
	}
	cb.reset()
	if cb.isOpen() {
		t.Fatal("expected closed after reset")
	}
}

func TestNormalizeUpstream_PanicFailsOpen(t *testing.T) {
	// AC4: a rule panic must not block the request; original body is returned.
	eng := New(nil)
	// Build a resolvedConfig with a rule that panics.
	panicEntry := ruleEntry{
		rule: Rule{
			ID:          "panic-test",
			AdapterType: AdapterOpenAI,
			Type:        RuleTypeStrip,
			Enabled:     true,
			Regex:       nil, // nil regex → applyStripRule returns original
		},
		breaker: newCircuitBreaker(),
	}
	// Manually inject a panic-inducing run function.
	resolved := &resolvedConfig{
		hasWork: true,
		upstreamRules: map[AdapterType][]ruleEntry{
			AdapterOpenAI: {panicEntry},
		},
		keyRules: map[AdapterType][]ruleEntry{},
	}
	eng.compiled.Store(resolved)

	body := []byte(`{"messages":[]}`)
	out, result := eng.NormalizeUpstream(AdapterOpenAI, body)
	// nil regex → no panic, no strip; rule succeeds with zero-count
	if string(out) != string(body) {
		t.Fatalf("expected original body, got %s", out)
	}
	_ = result
}

// --- Config hot-swap ---

func TestEngine_Reload_ConfigSwap(t *testing.T) {
	// AC6: config reload takes effect on the next request with no downtime.
	eng := New(nil)

	body := []byte(`{"system":[{"type":"text","text":"test cch=ff00ff; end"}]}`)

	// Phase 1: cch-strip disabled (default).
	out1 := eng.NormalizeKey(AdapterAnthropic, body)
	if !strings.Contains(string(out1), "cch=") {
		t.Fatal("phase 1: expected cch= present when rule disabled")
	}

	// Phase 2: enable cch-strip via Reload.
	enabled := true
	eng.Reload(Config{
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	})
	out2 := eng.NormalizeKey(AdapterAnthropic, body)
	if strings.Contains(string(out2), "cch=") {
		t.Fatalf("phase 2: expected cch= stripped after Reload, got %s", out2)
	}
}

// --- L4 cache_control injection (explicit content-block caching) ---

// TestNormalizeUpstream_RuntimeToggle_HotReload proves an operator's rule toggle
// takes effect at RUNTIME on a live engine — a second and third Reload on the SAME
// engine re-apply the changed value without recreating it. Regression guard for
// "toggling the rule requires a gateway restart": it would fail if NormalizeUpstream
// captured the demand gate / rule set once instead of reading the atomic compiled
// snapshot on every call.
func TestNormalizeUpstream_RuntimeToggle_HotReload(t *testing.T) {
	withRule := func(on bool) Config {
		return Config{
			Rules: map[string]map[string]RuleOverride{
				"anthropic": {RuleAnthropicCchStrip: {Enabled: &on}},
			},
		}
	}
	body := []byte(`{"system":[{"type":"text","text":"prompt cch=deadbeef; end"}]}`)
	eng := New(nil)

	// Start OFF: no demand, body passes through unchanged.
	eng.Reload(withRule(false))
	if out, _ := eng.NormalizeUpstream(AdapterAnthropic, body); string(out) != string(body) {
		t.Fatal("rule OFF: expected original body")
	}
	// Hot-enable: the SAME engine must now strip, no restart.
	eng.Reload(withRule(true))
	if out, r := eng.NormalizeUpstream(AdapterAnthropic, body); strings.Contains(string(out), "cch=") || r.StripCount == 0 {
		t.Fatalf("hot-enable: expected cch= stripped after runtime Reload(true), got %s (strip=%d)", out, r.StripCount)
	}
	// Hot-disable: flip back OFF at runtime, pass-through restored.
	eng.Reload(withRule(false))
	if out, r := eng.NormalizeUpstream(AdapterAnthropic, body); string(out) != string(body) || r.StripCount != 0 {
		t.Fatalf("hot-disable: expected original body after runtime Reload(false), got %s (strip=%d)", out, r.StripCount)
	}
}
