package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// A /v1/responses request carrying audio must reach the wire that carries
// audio, and come back in the shape the caller asked for.
//
// This is an end-to-end assertion on purpose. ServesResponses is resolved at
// FOUR sites — the routing stage, the cache-prep stage, IngressChatToWire and
// the executor — and the upstream wire shape they choose has to agree. Their
// comments used to ask each other to agree in prose ("all three sites must
// agree", "dispatch site 1 of 3"), and the executor's own comment records what
// happened when one of them keyed off the wrong field: a verbatim Responses
// body posted to the chat URL, 400. Passing the body as an argument makes a
// site that forgets fail to compile; this proves they also agree on WHICH body,
// which the compiler cannot check.
func TestResponsesIngress_AudioTakesTheChatWire(t *testing.T) {
	var gotPath, gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-audio-mini",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"regression fixture"},` +
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`))
	}))
	defer up.Close()

	deps := makeOpenAIDeps(t, up.URL, emptyHookCache(t))
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})

	reqBody := `{"model":"gpt-audio-mini","input":[{"role":"user","content":[` +
		`{"type":"input_text","text":"what is said?"},` +
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// The upstream leg: chat wire, chat body. A Responses body here means one
	// of the four sites still thinks the Responses wire serves this request.
	if !strings.Contains(gotPath, "chat/completions") {
		t.Errorf("upstream path = %q, want the chat wire — the Responses wire has no audio "+
			"content part, so sending it there is sending it to a refusal", gotPath)
	}
	if strings.Contains(gotBody, `"input"`) || !strings.Contains(gotBody, `"messages"`) {
		t.Errorf("upstream body was not canonicalized to chat: %s", gotBody)
	}
	if !strings.Contains(gotBody, "input_audio") {
		t.Errorf("the audio never reached the upstream; the downgrade must move the request "+
			"to another wire, not drop its content: %s", gotBody)
	}

	// The client leg: the caller asked on /v1/responses and must be answered in
	// that shape. A downgrade the caller can see is a broken contract, not a fix.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("client body is not JSON: %s", w.Body.String())
	}
	if out["object"] != "response" {
		t.Errorf("client received object=%v, want \"response\" — the caller is on /v1/responses "+
			"and the wire we chose upstream is not their concern: %s", out["object"], w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "regression fixture") {
		t.Errorf("the model's answer did not survive the round trip: %s", w.Body.String())
	}
}

// The mirror, and the reason the content check cannot be a blanket downgrade:
// an ordinary Responses request must still take the native wire, or built-in
// tools and stateful fields are lost for every caller.
func TestResponsesIngress_TextOnlyStaysOnTheResponsesWire(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","model":"gpt-5.4","status":"completed",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
			`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`))
	}))
	defer up.Close()

	deps := makeOpenAIDeps(t, up.URL, emptyHookCache(t))
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-5.4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(gotPath, "responses") {
		t.Errorf("upstream path = %q, want the native Responses wire — downgrading an ordinary "+
			"request would cost every caller the built-in tools and stateful fields that wire carries",
			gotPath)
	}
}

// The downgrade and the existing cross-format refusal must COMPOSE, not compete.
//
// A request can carry audio (which the Responses wire has no part for) AND a
// built-in tool (which only the Responses wire can run). It cannot be served
// either way, and the caller must be told so — by the refusal that already
// exists for the second condition, not by a silent downgrade that drops the
// tool.
//
// This is the claim the downgrade rests on: because it reports "does not serve",
// stage_routing's validateResponsesIngressForCrossFormat runs, and its wording
// covers the conflict without a second refusal path being written.
func TestResponsesIngress_AudioPlusABuiltinToolIsRefusedNotSilentlyDowngraded(t *testing.T) {
	var reached bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"response","output":[]}`))
	}))
	defer up.Close()

	deps := makeOpenAIDeps(t, up.URL, emptyHookCache(t))
	h := NewHandler(deps).ServeProxy(Ingress{
		WireShape:  typology.WireShapeOpenAIResponses,
		BodyFormat: provcore.FormatOpenAIResponses,
	})
	body := `{"model":"gpt-audio-mini","tools":[{"type":"web_search"}],"input":[{"role":"user","content":[` +
		`{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vk")
	w := httptest.NewRecorder()
	h(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 — the request needs the Responses wire for its tool and "+
			"cannot use it for its audio; serving it either way loses something the caller asked for. body=%s",
			w.Code, w.Body.String())
	}
	if reached {
		t.Error("the request reached an upstream; a conflict we can see before dispatch must be " +
			"refused before dispatch")
	}
	if !strings.Contains(w.Body.String(), "FEATURE_REQUIRES_NATIVE_RESPONSES_TARGET") {
		t.Errorf("the refusal did not come from the existing cross-format guard, so the two "+
			"conditions are not composing as claimed: %s", w.Body.String())
	}
}
