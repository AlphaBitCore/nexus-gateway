package codec

import (
	"bytes"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic prompt caching, as this codec turns it on.
//
// The mechanism is Anthropic's AUTOMATIC caching: a single `cache_control` at
// the ROOT of the request body. Anthropic then places the breakpoint on the
// last cacheable block itself and moves it forward as the conversation grows,
// so the cached prefix covers the whole request rather than a position we
// guessed. It needs no beta header and is honoured by every model this gateway
// routes to Anthropic today.
//
// This replaced an explicit two-breakpoint scheme (stamp the last system text
// block, optionally stamp the second-to-last user message). Measured against
// every Anthropic model configured on the provider, one arm per request so no
// arm could read what another wrote, the root marker cached the system prompt
// AND the message turn while the system-block marker cached only the system
// prompt — 14597 vs 12489 tokens on Sonnet 4.6, 27529 vs 23573 on Opus 4.7,
// the same ~85% ratio on all ten. The explicit scheme's second breakpoint was
// worse than that: anchored one turn behind the request, it wrote a new entry
// almost every turn and read one back rarely, and staging traffic showed it
// writing 2.8 tokens for every token it read.
//
// Why the codec owns this: `cache_control` is a field of the Anthropic
// Messages wire, so it belongs to the codec that speaks that wire. Bedrock
// Claude inherits it through the same delegation that gives it every other
// Anthropic body rule. Placing it anywhere else costs a list of provider names
// at each site that has to ask "is this an Anthropic-shaped body".
//
// Both doors apply it — the cross-format leg (EncodeRequest, e.g. an OpenAI
// `/v1/chat/completions` caller routed to Claude) and the same-spec leg
// (RewriteNative, a native `/v1/messages` caller). A rule on one door only is
// silently absent from the other, and production traffic uses whichever door
// the CALLER's ingress picks.

// promptCacheControlKey is the exact byte sequence a JSON encoder writes for a
// `cache_control` KEY. Standard encoders never escape these characters, so its
// absence proves the body carries no cache_control anywhere — at the root or
// on any block — without parsing the body.
var promptCacheControlKey = []byte(`"cache_control"`)

// promptCacheMarkerRaw is the marker itself: the default 5-minute ephemeral
// cache. No `ttl` field — a 1-hour TTL doubles the write price, and it only
// pays back when follow-up turns land outside the 5-minute window.
var promptCacheMarkerRaw = []byte(`{"type":"ephemeral"}`)

// promptCacheMarker is the same value for the cross-format door, which builds
// a map and marshals it once rather than editing encoded bytes. A value type,
// not a shared map, so no call site can mutate what every later request sends.
type promptCacheControl struct {
	Type string `json:"type"`
}

var promptCacheMarker = promptCacheControl{Type: "ephemeral"}

// promptCacheMarkerApplies reports whether this request should get the root
// marker. Both doors ask this one function so they cannot drift.
//
// The caller's own caching intent always wins. Adding a second breakpoint to a
// body that already carries one is not merely redundant: Anthropic answers 400
// when the last block's `cache_control` names a different TTL than the root
// marker, and again when four explicit breakpoints already occupy every slot.
//
// The check is a byte scan, and it is deliberately conservative in one
// direction: a request whose CONTENT happens to contain the literal
// `"cache_control"` is treated as already-marked and forwarded untouched. That
// costs one request its caching; the opposite mistake costs it a 400.
func promptCacheMarkerApplies(body []byte, enabled bool) bool {
	return enabled && len(body) > 0 && !bytes.Contains(body, promptCacheControlKey)
}

// promptCacheBoundaryPath returns the sjson path of the block that should carry
// the SECOND, explicit breakpoint, and whether there is one.
//
// Why a second breakpoint exists at all. Anthropic's automatic caching writes
// one entry, at the last cacheable block, and finds the previous turn's entry by
// walking backward from there — but only 20 blocks. A turn that appends more
// than that, which an agent round with many tool_use / tool_result blocks does
// routinely, pushes the previous write out of reach and the conversation stops
// hitting without any error to notice. The provider's own guidance for that case
// is a second breakpoint nearer the last write.
//
// Where it goes: the last content block of the last ASSISTANT message, i.e. the
// end of the previous turn. That position is stable — it is finished text the
// next turn will not edit — and it sits behind whatever the current turn
// appended, however much that was.
//
// NOT the second-to-last user message, which is where this knob used to put it.
// That anchor was measured on staging traffic writing 2.8 tokens of cache for
// every token it read: it moved every turn and cached a prefix one turn shorter
// than the automatic breakpoint already covered.
//
// Returns ok=false when the conversation has no assistant turn yet — a first
// request has nothing behind it to anchor, and the automatic breakpoint alone
// already covers it.
func promptCacheBoundaryPath(body []byte) (string, bool) {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return "", false
	}
	arr := msgs.Array()
	for i := len(arr) - 1; i >= 0; i-- {
		if arr[i].Get("role").String() != "assistant" {
			continue
		}
		content := arr[i].Get("content")
		switch {
		case content.IsArray():
			blocks := content.Array()
			if len(blocks) == 0 {
				return "", false
			}
			// Only a block Anthropic will accept a marker on. thinking blocks
			// cannot carry cache_control, and marking one is a 400.
			for j := len(blocks) - 1; j >= 0; j-- {
				switch blocks[j].Get("type").String() {
				case "text", "tool_use", "tool_result", "image", "document":
					return fmt.Sprintf("messages.%d.content.%d.cache_control", i, j), true
				}
			}
			return "", false
		case content.Type == gjson.String:
			// A string content block has no addressable sub-block; the marker
			// would have to convert it to an array, which rewrites bytes the
			// caller chose. Leave it to the automatic breakpoint.
			return "", false
		}
		return "", false
	}
	return "", false
}

// applyPromptCacheBoundary stamps the second, explicit breakpoint when the
// operator asked for one. Both doors call THIS, not the path finder, so the
// two cannot drift on when the marker is written or what it contains.
//
// It runs only after the root marker was added, i.e. only on a body the caller
// had not already marked — so this never adds a breakpoint beside a caller's
// own, which is the arrangement Anthropic answers 400 for when the TTLs differ.
// A body with no assistant turn yet is returned unchanged.
func applyPromptCacheBoundary(body []byte, want bool) ([]byte, error) {
	if !want {
		return body, nil
	}
	path, ok := promptCacheBoundaryPath(body)
	if !ok {
		return body, nil
	}
	return sjson.SetRawBytes(body, path, promptCacheMarkerRaw)
}
