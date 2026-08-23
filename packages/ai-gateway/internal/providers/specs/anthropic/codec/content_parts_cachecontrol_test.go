package codec

import (
	"testing"

	"github.com/tidwall/gjson"
)

// A caller-set cache_control must reach the Anthropic wire from ANY ingress, not
// only from /v1/messages.
//
// The gateway states the contract in the wire-rewrite injector: it returns the
// body unchanged when the client has already set a marker, "respecting explicit
// caller intent". That only held where the body arrived Anthropic-shaped. On
// every other ingress this projection rebuilt each part and the marker was gone
// before the injector could look — measured on prod, the same marker cached on
// /v1/messages (cache_creation_tokens 12924) and did nothing on
// /v1/chat/completions (provider_cache_status na).
func TestCacheControlSurvivesTheProjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		part string
	}{
		{"text", `{"type":"text","text":"long prefix","cache_control":{"type":"ephemeral"}}`},
		{"image data url", `{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"},` +
			`"cache_control":{"type":"ephemeral"}}`},
		{"image http url", `{"type":"image_url","image_url":{"url":"https://example.invalid/a.png"},` +
			`"cache_control":{"type":"ephemeral"}}`},
		{"document data url", `{"type":"file","file":{"file_data":"data:application/pdf;base64,AAAA"},` +
			`"cache_control":{"type":"ephemeral"}}`},
		{"document file id", `{"type":"file","file":{"file_id":"file_1"},` +
			`"cache_control":{"type":"ephemeral"}}`},
		{"document file url", `{"type":"file","file":{"file_url":"https://example.invalid/a.pdf"},` +
			`"cache_control":{"type":"ephemeral"}}`},
		{"tool_result", `{"type":"tool_result","tool_call_id":"c1","content":"42",` +
			`"cache_control":{"type":"ephemeral"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts, err := openAIPartsToAnthropicContent(gjson.Parse("[" + tc.part + "]"))
			if err != nil {
				t.Fatalf("projection returned %v", err)
			}
			if len(parts) != 1 {
				t.Fatalf("got %d blocks, want 1: %+v", len(parts), parts)
			}
			cc, ok := parts[0]["cache_control"]
			if !ok {
				t.Fatalf("the caller's marker was dropped; prompt caching silently does not "+
					"happen and the caller pays full input price: %+v", parts[0])
			}
			m, ok := cc.(map[string]any)
			if !ok || m["type"] != "ephemeral" {
				t.Errorf("marker was altered in transit: %#v", cc)
			}
		})
	}
}

// A part WITHOUT a marker must not grow one. The injector decides where markers
// go when the caller sets none, and a block stamped here would take that
// decision away from it — and could exceed Anthropic's breakpoint limit.
func TestCacheControlIsNotInvented(t *testing.T) {
	parts, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":{"type":"ephemeral"}}]`))
	if err != nil {
		t.Fatalf("projection returned %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d blocks, want 2", len(parts))
	}
	if _, ok := parts[0]["cache_control"]; ok {
		t.Errorf("an unmarked part grew a marker: %+v", parts[0])
	}
	if _, ok := parts[1]["cache_control"]; !ok {
		t.Errorf("the marked part lost its marker: %+v", parts[1])
	}
}

// Forwarded verbatim, not normalised. It is Anthropic's field on Anthropic's
// wire; a value we rewrote would be our opinion about someone else's schema,
// and Anthropic's own rejection is the honest answer to a bad one.
func TestCacheControlIsForwardedVerbatim(t *testing.T) {
	parts, err := openAIPartsToAnthropicContent(gjson.Parse(
		`[{"type":"text","text":"a","cache_control":{"type":"persistent","ttl":"1h"}}]`))
	if err != nil {
		t.Fatalf("projection returned %v", err)
	}
	m, _ := parts[0]["cache_control"].(map[string]any)
	if m["type"] != "persistent" || m["ttl"] != "1h" {
		t.Errorf("marker was rewritten: %#v", parts[0]["cache_control"])
	}
}
