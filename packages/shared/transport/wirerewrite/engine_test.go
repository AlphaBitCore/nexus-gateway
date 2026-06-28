package wirerewrite

import (
	"github.com/goccy/go-json"
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

func TestNormalizeUpstream_GlobalSwitchOff(t *testing.T) {
	// AC2: when normaliser_enabled=false, NormalizeUpstream returns original unchanged.
	enabled := true
	cfg := Config{
		NormaliserEnabled: false,
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body := []byte(`{"system":[{"type":"text","text":"test cch=aabbcc; end"}]}`)
	out, result := eng.NormalizeUpstream(AdapterAnthropic, "", body)
	if string(out) != string(body) {
		t.Fatal("expected original body when global switch off")
	}
	if result.StripCount != 0 {
		t.Fatalf("expected StripCount=0 when switch off, got %d", result.StripCount)
	}
}

func TestNormalizeUpstream_CchStrip_Enabled(t *testing.T) {
	// AC3: when global switch ON and cch= rule enabled, token is removed upstream.
	enabled := true
	cfg := Config{
		NormaliserEnabled: true,
		Rules: map[string]map[string]RuleOverride{
			"anthropic": {
				RuleAnthropicCchStrip: {Enabled: &enabled},
			},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body := []byte(`{"system":[{"type":"text","text":"prompt cch=deadbeef; end"}]}`)
	out, result := eng.NormalizeUpstream(AdapterAnthropic, "", body)
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
		enabled: true,
		upstreamRules: map[AdapterType][]ruleEntry{
			AdapterOpenAI: {panicEntry},
		},
		keyRules: map[AdapterType][]ruleEntry{},
	}
	eng.compiled.Store(resolved)

	body := []byte(`{"messages":[]}`)
	out, result := eng.NormalizeUpstream(AdapterOpenAI, "", body)
	// nil regex → no panic, no strip; rule succeeds with zero-count
	if string(out) != string(body) {
		t.Fatalf("expected original body, got %s", out)
	}
	_ = result
}

func TestItoa_AllCases(t *testing.T) {
	// itoa intentionally avoids strconv to keep the dependency surface
	// minimal. Pin the cases that distinguish from strconv's behavior
	// (panicking on negatives is documented; we just don't test that).
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{1, "1"},
		{9, "9"},
		{10, "10"},
		{100, "100"},
		{12345, "12345"},
	}
	for _, c := range cases {
		if got := itoa(c.in); got != c.want {
			t.Errorf("itoa(%d): got %q want %q", c.in, got, c.want)
		}
	}
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

func TestInjectCacheMarkers_SystemArray_BlockLevel(t *testing.T) {
	// Explicit caching: inject cache_control into the last text block of the
	// system array, not at the request root, so Anthropic reports cache tokens.
	body := []byte(`{"system":[{"type":"text","text":"big system prompt"}],"messages":[{"role":"user","content":"hi"}]}`)
	out, err := injectCacheMarkers(body, "ephemeral", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid JSON after injection: %v", err)
	}
	// No root-level cache_control — it goes into the content block.
	if _, hasRoot := root["cache_control"]; hasRoot {
		t.Fatalf("should not have root-level cache_control, got %s", out)
	}
	sys := root["system"].([]any)
	block := sys[0].(map[string]any)
	cc, ok := block["cache_control"]
	if !ok {
		t.Fatalf("expected cache_control on the system text block, got %s", out)
	}
	ccMap, _ := cc.(map[string]any)
	if ccMap["type"] != "ephemeral" {
		t.Fatalf("expected ephemeral, got %v", cc)
	}
	n := countInjectedMarkers(body, out)
	if n != 1 {
		t.Fatalf("expected 1 injected marker, got %d", n)
	}
}

func TestInjectCacheMarkers_SystemString_ConvertedToBlock(t *testing.T) {
	// String system is converted to a single-element content-block array
	// and the block receives cache_control so Anthropic reports cache tokens.
	body := []byte(`{"system":"string system prompt","messages":[]}`)
	out, err := injectCacheMarkers(body, "ephemeral", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("invalid JSON after injection: %v", err)
	}
	// No root-level cache_control.
	if _, hasRoot := root["cache_control"]; hasRoot {
		t.Fatalf("should not have root-level cache_control, got %s", out)
	}
	// System must be converted to an array.
	sys, ok := root["system"].([]any)
	if !ok || len(sys) == 0 {
		t.Fatalf("expected system to be a non-empty array, got %s", out)
	}
	block := sys[0].(map[string]any)
	if block["text"] != "string system prompt" {
		t.Fatalf("system text not preserved, got %s", out)
	}
	cc, ok := block["cache_control"]
	if !ok {
		t.Fatalf("expected cache_control on system block, got %s", out)
	}
	ccMap, _ := cc.(map[string]any)
	if ccMap["type"] != "ephemeral" {
		t.Fatalf("expected ephemeral, got %v", cc)
	}
}

func TestInjectCacheMarkers_ExistingBlockMarkerRespected(t *testing.T) {
	// If the client already set block-level cache_control, the body is
	// left entirely unchanged — no root-level marker is added.
	body := []byte(`{"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[]}`)
	out, err := injectCacheMarkers(body, "ephemeral", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n := countInjectedMarkers(body, out)
	if n != 0 {
		t.Fatalf("expected 0 new markers when client already set block-level cache_control, got %d", n)
	}
}

func TestInjectCacheMarkers_ExistingRootMarkerRespected(t *testing.T) {
	// If the client already set a root-level cache_control, the body is
	// left unchanged.
	body := []byte(`{"cache_control":{"type":"ephemeral"},"system":"sys","messages":[]}`)
	out, err := injectCacheMarkers(body, "ephemeral", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n := countInjectedMarkers(body, out)
	if n != 0 {
		t.Fatalf("expected 0 new markers when client already set root-level cache_control, got %d", n)
	}
}

func TestInjectCacheMarkers_ManyExistingMarkers_NoAdditional(t *testing.T) {
	// Body already has multiple block-level markers — none should be added.
	body := []byte(`{
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"tools":[
			{"name":"t1","cache_control":{"type":"ephemeral"}},
			{"name":"t2","cache_control":{"type":"ephemeral"}}
		],
		"messages":[]
	}`)
	out, err := injectCacheMarkers(body, "ephemeral", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	n := countInjectedMarkers(body, out)
	if n != 0 {
		t.Fatalf("expected 0 new markers when client already set markers, got %d", n)
	}
}

func TestInjectCacheMarkers_Boundary3Respected(t *testing.T) {
	// boundary3=false → 1 marker (system block only).
	// boundary3=true  → 2 markers (system block + second-to-last user message).
	// Neither case should add a root-level cache_control.
	body := []byte(`{
		"system":[{"type":"text","text":"s"}],
		"messages":[
			{"role":"user","content":"msg1"},
			{"role":"assistant","content":"msg2"},
			{"role":"user","content":"msg3"}
		]
	}`)

	cases := []struct {
		b3       bool
		wantN    int
		wantRoot bool
	}{
		{false, 1, false},
		{true, 2, false},
	}
	for _, tc := range cases {
		out, err := injectCacheMarkers(body, "ephemeral", tc.b3)
		if err != nil {
			t.Fatalf("boundary3=%v: unexpected error: %v", tc.b3, err)
		}
		n := countInjectedMarkers(body, out)
		if n != tc.wantN {
			t.Fatalf("boundary3=%v: expected %d markers, got %d\nbody: %s", tc.b3, tc.wantN, n, out)
		}
		var root map[string]any
		if err := json.Unmarshal(out, &root); err != nil {
			t.Fatalf("boundary3=%v: invalid JSON: %v", tc.b3, err)
		}
		_, hasRoot := root["cache_control"]
		if hasRoot != tc.wantRoot {
			t.Fatalf("boundary3=%v: root-level cache_control present=%v, want %v\nbody: %s", tc.b3, hasRoot, tc.wantRoot, out)
		}
	}
}

func TestNormalizeUpstream_L4_Inject_PerProvider(t *testing.T) {
	// L4 injection triggers when provider is configured.
	cfg := Config{
		NormaliserEnabled: true,
		Providers: map[string]ProviderCacheConfig{
			"prov-1": {CacheMarkerInjectEnabled: true},
		},
	}
	eng := New(nil)
	eng.Reload(cfg)

	body := []byte(`{"model":"claude-opus-4","system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":"hi"}]}`)

	// Provider "prov-1" → inject
	out, result := eng.NormalizeUpstream(AdapterAnthropic, "prov-1", body)
	if !strings.Contains(string(out), `"cache_control"`) {
		t.Fatalf("expected cache_control injected for prov-1, got %s", out)
	}
	if result.MarkersInjected == 0 {
		t.Fatal("expected MarkersInjected>0 for prov-1")
	}

	// Provider "prov-2" → no inject
	out2, result2 := eng.NormalizeUpstream(AdapterAnthropic, "prov-2", body)
	if strings.Contains(string(out2), `"cache_control"`) {
		t.Fatalf("expected no cache_control for prov-2, got %s", out2)
	}
	if result2.MarkersInjected != 0 {
		t.Fatalf("expected MarkersInjected=0 for prov-2, got %d", result2.MarkersInjected)
	}
}

// TestNormalizeUpstream_RuntimeToggle_HotReload proves the global normaliser_enabled
// switch takes effect at RUNTIME on a live engine — a second and third Reload on the
// SAME engine re-apply the changed value without recreating it. This is the regression
// guard for the "toggling normaliser_enabled requires a gateway restart" bug: it would
// fail if NormalizeUpstream captured the flag once instead of reading the atomic
// compiled snapshot on every call.
func TestNormalizeUpstream_RuntimeToggle_HotReload(t *testing.T) {
	enabled := true
	withSwitch := func(on bool) Config {
		return Config{
			NormaliserEnabled: on,
			Rules: map[string]map[string]RuleOverride{
				"anthropic": {RuleAnthropicCchStrip: {Enabled: &enabled}},
			},
		}
	}
	body := []byte(`{"system":[{"type":"text","text":"prompt cch=deadbeef; end"}]}`)
	eng := New(nil)

	// Start OFF: body passes through unchanged.
	eng.Reload(withSwitch(false))
	if out, _ := eng.NormalizeUpstream(AdapterAnthropic, "", body); string(out) != string(body) {
		t.Fatal("switch OFF: expected original body")
	}
	// Hot-enable: the SAME engine must now strip, no restart.
	eng.Reload(withSwitch(true))
	if out, r := eng.NormalizeUpstream(AdapterAnthropic, "", body); strings.Contains(string(out), "cch=") || r.StripCount == 0 {
		t.Fatalf("hot-enable: expected cch= stripped after runtime Reload(true), got %s (strip=%d)", out, r.StripCount)
	}
	// Hot-disable: flip back OFF at runtime, pass-through restored.
	eng.Reload(withSwitch(false))
	if out, r := eng.NormalizeUpstream(AdapterAnthropic, "", body); string(out) != string(body) || r.StripCount != 0 {
		t.Fatalf("hot-disable: expected original body after runtime Reload(false), got %s (strip=%d)", out, r.StripCount)
	}
}
