package codec

import (
	"bytes"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

const (
	promptCacheCanonical = `{"model":"claude-sonnet-4-6","max_tokens":16,` +
		`"messages":[{"role":"system","content":"stable operator playbook"},` +
		`{"role":"user","content":"hello"}]}`
	promptCacheNative = `{"model":"claude-sonnet-4-6","max_tokens":16,` +
		`"system":"stable operator playbook",` +
		`"messages":[{"role":"user","content":"hello"}]}`
)

func promptCacheTarget(on bool) provcore.CallTarget {
	return provcore.CallTarget{
		ProviderModelID:    "claude-sonnet-4-6",
		MaxOutputTokens:    64000,
		PromptCacheMarkers: on,
	}
}

// The marker Anthropic reads for automatic caching is a ROOT field. An earlier
// scheme stamped the last system text block instead, which cached the system
// prompt and left the message turn out of the cached prefix — measured at 12489
// vs 14597 tokens on this model. Asserting the marker's LOCATION is what keeps
// that regression from coming back as "a cache_control is present somewhere".
func TestPromptCache_MarkerLandsAtTheRoot_CrossFormatDoor(t *testing.T) {
	res, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages,
		[]byte(promptCacheCanonical), promptCacheTarget(true))
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("root cache_control.type = %q, want ephemeral; body=%s", got, res.Body)
	}
	if gjson.GetBytes(res.Body, `system.#(cache_control)`).Exists() {
		t.Errorf("the marker must not also be stamped on a system block: %s", res.Body)
	}
	if !res.PromptCacheMarked {
		t.Error("PromptCacheMarked must report the marker the codec just wrote")
	}
}

func TestPromptCache_MarkerLandsAtTheRoot_NativeDoor(t *testing.T) {
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(promptCacheNative), promptCacheTarget(true), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if got := gjson.GetBytes(res.Body, "cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("root cache_control.type = %q, want ephemeral; body=%s", got, res.Body)
	}
	if !res.PromptCacheMarked {
		t.Error("PromptCacheMarked must report the marker the codec just wrote")
	}
}

// Two doors, one rule. Which door a request takes is decided by the caller's
// ingress — `/v1/chat/completions` reaches the cross-format door and
// `/v1/messages` the native one — so a marker on one door only would turn
// caching on for half of production and nothing would notice.
func TestTwoDoorParity_PromptCacheMarkerOnBothDoors(t *testing.T) {
	for _, on := range []bool{false, true} {
		cross, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages,
			[]byte(promptCacheCanonical), promptCacheTarget(on))
		if err != nil {
			t.Fatalf("cross-format door (enabled=%v): %v", on, err)
		}
		native, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
			[]byte(promptCacheNative), promptCacheTarget(on), false)
		if err != nil {
			t.Fatalf("native door (enabled=%v): %v", on, err)
		}
		if cross.PromptCacheMarked != native.PromptCacheMarked {
			t.Fatalf("enabled=%v: doors disagree about marking — cross=%v native=%v",
				on, cross.PromptCacheMarked, native.PromptCacheMarked)
		}
		crossMark := gjson.GetBytes(cross.Body, "cache_control").Raw
		nativeMark := gjson.GetBytes(native.Body, "cache_control").Raw
		if crossMark != nativeMark {
			t.Fatalf("enabled=%v: doors emitted different markers — cross=%q native=%q",
				on, crossMark, nativeMark)
		}
		if on && crossMark == "" {
			t.Fatalf("enabled=true must produce a marker on both doors")
		}
	}
}

// A caller that already sent its own cache_control is not merely
// double-marked: Anthropic answers 400 when the last block's marker names a
// different TTL than the root one, and again when four explicit breakpoints
// already occupy every slot. Forwarding untouched is the only safe answer.
func TestPromptCache_CallerOwnMarkerIsNeverSecondGuessed(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"block level on a system part", `{"model":"claude-sonnet-4-6","max_tokens":16,` +
			`"system":[{"type":"text","text":"ctx","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
			`"messages":[{"role":"user","content":"hi"}]}`},
		{"root level already present", `{"model":"claude-sonnet-4-6","max_tokens":16,` +
			`"cache_control":{"type":"ephemeral","ttl":"1h"},"system":"ctx",` +
			`"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
				[]byte(tc.body), promptCacheTarget(true), false)
			if err != nil {
				t.Fatalf("RewriteNative: %v", err)
			}
			if res.PromptCacheMarked {
				t.Error("must not claim to have marked a body the caller had already marked")
			}
			if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
				t.Fatalf("the caller's single marker must survive alone, found %d: %s", n, res.Body)
			}
			if ttl := gjson.GetBytes(res.Body, `system.0.cache_control.ttl`).String(); ttl != "" && ttl != "1h" {
				t.Fatalf("the caller's TTL must not be rewritten, got %q", ttl)
			}
		})
	}
}

// Markers off is the default and the answer before any cache config arrives.
// The no-op path must leave the native body byte-identical: this door's
// contract is that native features ride through verbatim.
func TestPromptCache_Disabled_NativeBodyIsUntouched(t *testing.T) {
	in := []byte(promptCacheNative)
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages, in, promptCacheTarget(false), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if bytes.Contains(res.Body, []byte(`"cache_control"`)) {
		t.Fatalf("markers off must add nothing: %s", res.Body)
	}
	if res.PromptCacheMarked {
		t.Error("markers off must not report a marker")
	}
}

const boundaryNative = `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx",` +
	`"messages":[` +
	`{"role":"user","content":"one"},` +
	`{"role":"assistant","content":[{"type":"text","text":"first answer"}]},` +
	`{"role":"user","content":"two"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"x","input":{}},{"type":"text","text":"second answer"}]},` +
	`{"role":"user","content":"three"}]}`

func boundaryTarget(markers, boundary bool) provcore.CallTarget {
	t := promptCacheTarget(markers)
	t.PromptCacheBoundary = boundary
	return t
}

// The second breakpoint anchors at the END OF THE PREVIOUS TURN — the last
// markable block of the last assistant message — not at a user message.
//
// Anthropic's automatic breakpoint sits on the last cacheable block and finds
// the previous turn's entry by walking back at most 20 blocks. A turn that
// appends more than that pushes the previous write out of reach and the
// conversation stops hitting with no error to notice. Anchoring behind the
// current turn is what keeps a write within reach however much this turn added.
func TestPromptCacheBoundary_AnchorsAtEndOfPreviousAssistantTurn(t *testing.T) {
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(boundaryNative), boundaryTarget(true, true), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	// The automatic breakpoint is still the root field.
	if got := gjson.GetBytes(res.Body, "cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("the root marker must still be present, got %q", got)
	}
	// The explicit one is on the LAST markable block of the LAST assistant turn.
	if got := gjson.GetBytes(res.Body, "messages.3.content.1.cache_control.type").String(); got != "ephemeral" {
		t.Fatalf("boundary must anchor on the last assistant turn's last block; body=%s", res.Body)
	}
	// And nowhere else — two markers total, no stray one on a user message.
	if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 2 {
		t.Fatalf("want exactly 2 markers (root + boundary), found %d: %s", n, res.Body)
	}
	if gjson.GetBytes(res.Body, "messages.2.content.cache_control").Exists() ||
		gjson.GetBytes(res.Body, "messages.4.content.cache_control").Exists() {
		t.Errorf("the boundary must not land on a user message: %s", res.Body)
	}
}

// Off is off: the knob adds nothing when the operator has not asked for it.
func TestPromptCacheBoundary_OffAddsOnlyTheRootMarker(t *testing.T) {
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(boundaryNative), boundaryTarget(true, false), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
		t.Fatalf("boundary off must leave exactly the root marker, found %d: %s", n, res.Body)
	}
}

// A first request has no turn behind it to anchor, and the automatic breakpoint
// already covers it. Adding a marker anyway would spend a breakpoint slot for
// nothing.
func TestPromptCacheBoundary_FirstTurnHasNothingToAnchor(t *testing.T) {
	first := `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[{"role":"user","content":"one"}]}`
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(first), boundaryTarget(true, true), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
		t.Fatalf("a first turn must carry only the root marker, found %d: %s", n, res.Body)
	}
}

// Both doors place the boundary identically, for the same reason the root
// marker does: which door a request takes is the caller's ingress, not
// something the codec can see.
func TestTwoDoorParity_PromptCacheBoundary(t *testing.T) {
	canonical := `{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[` +
		`{"role":"system","content":"ctx"},` +
		`{"role":"user","content":"one"},` +
		`{"role":"assistant","content":"first answer"},` +
		`{"role":"user","content":"two"}]}`
	cross, err := Codec{}.EncodeRequest(typology.WireShapeAnthropicMessages,
		[]byte(canonical), boundaryTarget(true, true))
	if err != nil {
		t.Fatalf("cross-format door: %v", err)
	}
	native := `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[` +
		`{"role":"user","content":"one"},` +
		`{"role":"assistant","content":[{"type":"text","text":"first answer"}]},` +
		`{"role":"user","content":"two"}]}`
	nat, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(native), boundaryTarget(true, true), false)
	if err != nil {
		t.Fatalf("native door: %v", err)
	}
	// Both doors must reach the same assistant turn, index 1 in each shape.
	for _, c := range []struct {
		name string
		body []byte
	}{{"cross-format", cross.Body}, {"native", nat.Body}} {
		if got := gjson.GetBytes(c.body, "messages.1.content.0.cache_control.type").String(); got != "ephemeral" {
			t.Errorf("%s door: boundary missing from the assistant turn; body=%s", c.name, c.body)
		}
		if n := bytes.Count(c.body, []byte(`"cache_control"`)); n != 2 {
			t.Errorf("%s door: want root + boundary, found %d markers", c.name, n)
		}
	}
}

// Shapes the boundary must decline rather than guess at. Each one leaves the
// automatic breakpoint to do the work alone, which is correct — a breakpoint
// slot spent on a position that cannot carry a marker, or on a body we would
// have to restructure to mark, is worse than not spending it.
func TestPromptCacheBoundary_DeclinesShapesItCannotAnchor(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		why  string
	}{
		{
			name: "assistant content is a plain string",
			why:  "marking it would mean converting the caller's string into a block array — rewriting bytes they chose",
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[` +
				`{"role":"user","content":"one"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}]}`,
		},
		{
			name: "assistant turn is only a thinking block",
			why:  "thinking blocks cannot carry cache_control; marking one is a 400",
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[` +
				`{"role":"user","content":"one"},` +
				`{"role":"assistant","content":[{"type":"thinking","thinking":"...","signature":"s"}]},` +
				`{"role":"user","content":"two"}]}`,
		},
		{
			name: "assistant turn has an empty content array",
			why:  "there is no block to anchor on",
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[` +
				`{"role":"user","content":"one"},{"role":"assistant","content":[]},{"role":"user","content":"two"}]}`,
		},
		{
			name: "assistant content is neither an array nor a string",
			why:  "a shape the wire does not define — decline rather than guess at a path inside it",
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[` +
				`{"role":"user","content":"one"},{"role":"assistant","content":42},{"role":"user","content":"two"}]}`,
		},
		{
			name: "no assistant turn at all",
			why:  "a first request has nothing behind it; the automatic breakpoint already covers it",
			body: `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":[{"role":"user","content":"one"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
				[]byte(tc.body), boundaryTarget(true, true), false)
			if err != nil {
				t.Fatalf("RewriteNative: %v", err)
			}
			if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
				t.Fatalf("%s — %s: want only the root marker, found %d: %s", tc.name, tc.why, n, res.Body)
			}
		})
	}
}

// A body whose `messages` is not an array at all must not panic or mark; the
// upstream will reject it on its own terms.
func TestPromptCacheBoundary_MessagesNotAnArray(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","max_tokens":16,"system":"ctx","messages":{"role":"user"}}`
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(body), boundaryTarget(true, true), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
		t.Fatalf("want only the root marker, found %d: %s", n, res.Body)
	}
}

// The boundary is skipped entirely when the caller already marked the body,
// because the root marker was skipped too — the codec never adds a breakpoint
// beside a caller's own, which is the arrangement Anthropic 400s on when the
// TTLs differ.
func TestPromptCacheBoundary_NotAddedBesideACallersOwnMarker(t *testing.T) {
	body := `{"model":"claude-sonnet-4-6","max_tokens":16,` +
		`"system":[{"type":"text","text":"ctx","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
		`"messages":[{"role":"user","content":"one"},` +
		`{"role":"assistant","content":[{"type":"text","text":"answer"}]},` +
		`{"role":"user","content":"two"}]}`
	res, err := Codec{}.RewriteNative(typology.WireShapeAnthropicMessages,
		[]byte(body), boundaryTarget(true, true), false)
	if err != nil {
		t.Fatalf("RewriteNative: %v", err)
	}
	if res.PromptCacheMarked {
		t.Error("must not claim to have marked a body the caller had already marked")
	}
	if n := bytes.Count(res.Body, []byte(`"cache_control"`)); n != 1 {
		t.Fatalf("the caller's single marker must survive alone, found %d: %s", n, res.Body)
	}
}
