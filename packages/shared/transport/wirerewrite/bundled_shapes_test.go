package wirerewrite

import (
	"strings"
	"testing"
)

// The cch nonce reaches this engine in two different JSON shapes depending on
// which door the request came through, and the rule has to strip it from both.
//
//   - A Claude Code caller on /v1/messages sends `system` as an ARRAY of content
//     blocks, and the native leg forwards that shape verbatim.
//   - The same conversation arriving on /v1/chat/completions is rebuilt by this
//     gateway's own Anthropic codec, which emits `system` as a plain STRING.
//
// The rule used to declare only "system.#.text", a selector whose gjson `#`
// requires an array. It therefore stripped the nonce for the native caller and
// did nothing whatsoever for the cross-format one — on the upstream body AND on
// the L1 cache key, since both are derived from the same prepared bytes.
//
// Measured on the live Anthropic wire: a rotating cch= makes the provider
// re-create the prompt cache every turn and never read it (turn 2: creation
// 11792, read 0), while the same conversation with the nonce stripped reads the
// whole prefix back (turn 2: creation 0, read 11774).
func TestCchStrip_BothSystemShapes(t *testing.T) {
	enabled := true
	eng := New(nil)
	eng.Reload(Config{Rules: map[string]map[string]RuleOverride{
		AdapterAnthropic: {RuleAnthropicCchStrip: {Enabled: &enabled}},
	}})

	for _, tc := range []struct {
		name string
		body string
		door string
	}{
		{
			name: "array of content blocks",
			door: "native /v1/messages — the client's own shape",
			body: `{"model":"claude-sonnet-4-6","system":[{"type":"text","text":"cch=deadbeef01; You are a coding assistant.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "plain string",
			door: "cross-format — rebuilt by our Anthropic codec",
			body: `{"model":"claude-sonnet-4-6","system":"cch=deadbeef01; You are a coding assistant.","messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The upstream body: the nonce must not reach the provider, because
			// its rotation is what defeats the provider's prompt cache.
			out, res := eng.NormalizeUpstream(AdapterAnthropic, []byte(tc.body))
			if strings.Contains(string(out), "cch=") {
				t.Fatalf("%s: nonce survived to the upstream body: %s", tc.door, out)
			}
			if res.StripCount == 0 {
				t.Fatalf("%s: the strip must be reported, got StripCount=0", tc.door)
			}
			// And the surrounding prompt must be intact — this is a surgical
			// strip of one token, not a rewrite of the caller's system prompt.
			if !strings.Contains(string(out), "You are a coding assistant.") {
				t.Fatalf("%s: the rule removed more than the nonce: %s", tc.door, out)
			}

			// The L1 cache key path derives from the same bytes and must agree,
			// or two sessions that differ only in the nonce land on different
			// keys and the gateway's own cache never hits either.
			keyBody := eng.NormalizeKey(AdapterAnthropic, []byte(tc.body))
			if strings.Contains(string(keyBody), "cch=") {
				t.Fatalf("%s: nonce survived into the cache key body: %s", tc.door, keyBody)
			}
		})
	}
}

// Two sessions differing ONLY in the nonce must hash the same, on either shape.
func TestCchStrip_RotatingNonceCollapsesToOneKey(t *testing.T) {
	enabled := true
	eng := New(nil)
	eng.Reload(Config{Rules: map[string]map[string]RuleOverride{
		AdapterAnthropic: {RuleAnthropicCchStrip: {Enabled: &enabled}},
	}})
	for _, shape := range []struct{ a, b string }{
		{`{"system":[{"type":"text","text":"cch=aaaa1111; ctx"}]}`, `{"system":[{"type":"text","text":"cch=bbbb2222; ctx"}]}`},
		{`{"system":"cch=aaaa1111; ctx"}`, `{"system":"cch=bbbb2222; ctx"}`},
	} {
		k1 := string(eng.NormalizeKey(AdapterAnthropic, []byte(shape.a)))
		k2 := string(eng.NormalizeKey(AdapterAnthropic, []byte(shape.b)))
		if k1 != k2 {
			t.Fatalf("rotating nonce must collapse to one key body:\n a=%s\n b=%s", k1, k2)
		}
	}
}
