package chatgptweb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestChatGPTWeb_FullConversationDecode replays a realistic browser ChatGPT
// exchange end-to-end through the adapter — the exact path a real chatgpt.com
// session takes once the Linux agent MITM-bumps chatgpt.com:
//
//  1. The browser POSTs /backend-api/f/conversation with the user's prompt.
//  2. chatgpt.com streams back an "add" seed frame, several "append" content
//     deltas, interleaved telemetry/marker frames, then completes.
//
// It asserts the adapter (a) extracts the user prompt + model from the
// request, (b) reassembles the assistant reply from the ordered SSE deltas,
// (c) drops every telemetry/marker frame, and (d) reports chatgpt.com as a
// non-metered consumer surface. This is the integration-level proof that a
// monitored chatgpt.com conversation is actually decoded — the per-function
// tests pin each piece, this pins the whole flow.
func TestChatGPTWeb_FullConversationDecode(t *testing.T) {
	a := &Adapter{}
	ctx := context.Background()
	const path = "/backend-api/f/conversation"

	// ---- 1. Request: user prompt + model -------------------------------
	reqBody := []byte(`{
		"action": "next",
		"model": "gpt-4o",
		"model_slug": "gpt-4o",
		"conversation_id": "c-2f1a",
		"parent_message_id": "p-9c3",
		"messages": [
			{"author": {"role": "user"},
			 "content": {"content_type": "text", "parts": ["用一句话介绍长城"]}}
		]
	}`)

	nc, err := a.ExtractRequest(ctx, reqBody, path)
	if err != nil {
		t.Fatalf("ExtractRequest: %v", err)
	}
	if got := strings.Join(nc.Segments, ""); got != "用一句话介绍长城" {
		t.Fatalf("request prompt = %q, want the user message", got)
	}
	if nc.Metadata["model"] != "gpt-4o" || nc.Metadata["conversation_id"] != "c-2f1a" {
		t.Fatalf("request meta = %v, want model gpt-4o + conversation_id c-2f1a", nc.Metadata)
	}

	// DetectRequestMeta disambiguates this from the public API surface.
	rm := a.DetectRequestMeta(httptest.NewRequest(http.MethodPost, path, nil), reqBody)
	if rm.Provider != "chatgpt-web" || rm.Model != "gpt-4o" {
		t.Fatalf("DetectRequestMeta = %+v, want provider chatgpt-web + model gpt-4o", rm)
	}

	// ---- 2. Response: ordered SSE delta stream -------------------------
	// Real chatgpt.com wire order: version marker, telemetry/routing frames
	// (must be dropped), an "add" seed carrying the first content part, then
	// "append" deltas that grow /message/content/parts/0, then completion.
	frames := [][]byte{
		[]byte(`"v1"`), // version marker -> drop
		[]byte(`{"type":"resume_conversation_token","token":"eyJ"}`), // routing JWT -> drop
		[]byte(`{"o":"add","p":"","v":{"message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["长城"]}}}}`),
		[]byte(`{"type":"message_marker","data":{"type":"first_token"}}`), // telemetry -> drop
		[]byte(`{"o":"append","p":"/message/content/parts/0","v":"是中国"}`),
		[]byte(`{"o":"append","p":"/message/content/parts/0","v":"古代的"}`),
		[]byte(`{"o":"append","p":"/message/content/parts/0","v":"军事防御工程。"}`),
		[]byte(`{"type":"conversation_detail_metadata","data":{}}`), // telemetry -> drop
		[]byte(`{"type":"message_stream_complete"}`),                // completion -> drop
		[]byte(`[DONE]`), // sentinel -> drop
	}

	var assistant strings.Builder
	for i, f := range frames {
		chunk, sErr := a.ExtractStreamChunk(ctx, f, path)
		if sErr != nil {
			t.Fatalf("frame %d ExtractStreamChunk: %v", i, sErr)
		}
		for _, seg := range chunk.Segments {
			assistant.WriteString(seg)
		}
		if len(chunk.ToolCallSegments) != 0 {
			t.Fatalf("frame %d produced tool calls %v, want none", i, chunk.ToolCallSegments)
		}
	}

	if got := assistant.String(); got != "长城是中国古代的军事防御工程。" {
		t.Fatalf("reassembled assistant reply = %q, want the full sentence from the deltas", got)
	}

	// ---- 3. Usage: consumer product, no metered token usage ------------
	usage := a.DetectResponseUsage(&http.Response{}, nil)
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Fatalf("DetectResponseUsage status = %v, want non-LLM sentinel (chatgpt.com is unmetered)", usage.Status)
	}
}

// TestChatGPTWeb_NonConversationPathNotDecoded pins the behaviour I observed
// live: a bare GET of the chatgpt.com root (or any request without a
// `messages` body) is NOT decoded as a conversation — ExtractRequest returns
// ErrUnknownSchema so the audit pipeline records no chat content for it. This
// is why a plain `curl https://chatgpt.com/` produces no traffic_event content
// while a real /backend-api/f/conversation POST does.
func TestChatGPTWeb_NonConversationPathNotDecoded(t *testing.T) {
	a := &Adapter{}
	// Page-load / account-info shaped body — valid JSON, but no messages.
	_, err := a.ExtractRequest(context.Background(), []byte(`{"account":{"plan":"plus"}}`), "/")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("non-conversation body: err = %v, want ErrUnknownSchema", err)
	}
}
