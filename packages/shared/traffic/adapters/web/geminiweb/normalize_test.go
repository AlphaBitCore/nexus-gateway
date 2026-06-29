package geminiweb

import (
	"context"
	"fmt"
	"github.com/goccy/go-json"
	"net/url"
	"strings"
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Fixture builders

// buildBatchExecuteRequest assembles a gemini.google.com chat POST
// body matching the captured prod shape (traffic_event
// 78911179-d123-4810-bf31-7bf4defde85a):
//
//	f.req=<URL-ENCODED [null, "<INNER>"]>&at=<csrf>
//
//	inner = [[ "<PROMPT>", 0, null, null, null, null, 0 ], ["en"], ...]
//
// Returns the URL-encoded form bytes ready to be sent through the
// Normalize entrypoint as an audit request body.
func buildBatchExecuteRequest(t *testing.T, prompt string, locale string) []byte {
	t.Helper()
	// inner array; only the first element matters for prompt extraction.
	innerArr := []any{
		[]any{prompt, 0, nil, nil, nil, nil, 0},
		[]any{locale},
	}
	innerJSON, err := json.Marshal(innerArr)
	if err != nil {
		t.Fatalf("inner marshal: %v", err)
	}
	outer := []any{nil, string(innerJSON)}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		t.Fatalf("outer marshal: %v", err)
	}
	form := url.Values{}
	form.Set("f.req", string(outerJSON))
	form.Set("at", "AOOh0PGQVV_oCeqqld81UdRSGItv:1778839245557")
	return []byte(form.Encode())
}

// buildBatchExecuteResponse assembles a single-chunk batchexecute
// response body matching the prod chunk shape:
//
//	)]}'
//	<length>
//	[["wrb.fr",null,"<INNER-JSON-AS-STRING>"]]
//
// inner = [null, ["c_<id>", "r_<id>"], null, null,
//
//	[["rc_<id>", ["<CUMULATIVE TEXT>"], null, null, null, null,
//	  null, null, [1], ..., "<MODEL>", true, ...]]]
//
// Multiple chunks concatenate naturally — pass the same builder
// repeatedly to simulate streaming.
func buildBatchExecuteChunk(t *testing.T, cumulativeText, modelName string) []byte {
	t.Helper()
	// Build the cand array; cand[1] = [text], plus a model-looking
	// string later in the array for the model sweep to find.
	cand := []any{
		"rc_753ca38d500abfb1",
		[]any{cumulativeText},
		nil, nil, nil, nil, nil, nil,
		[]any{1},
	}
	// Pad to plausible length, then drop the model name in.
	for range 22 {
		cand = append(cand, nil)
	}
	cand = append(cand, modelName)

	candWrapper := []any{cand}
	inner := []any{
		nil,
		[]any{"c_dc723068efd49598", "r_11bfbbd242ff5fb7"},
		nil, nil,
		candWrapper,
	}
	innerJSON, err := json.Marshal(inner)
	if err != nil {
		t.Fatalf("inner marshal: %v", err)
	}
	outer := [][]any{
		{"wrb.fr", nil, string(innerJSON)},
	}
	outerJSON, err := json.Marshal(outer)
	if err != nil {
		t.Fatalf("outer marshal: %v", err)
	}
	return []byte(fmt.Sprintf("%d\n%s\n", len(outerJSON), string(outerJSON)))
}

// Request side

func TestNormalize_BatchExecuteRequest_PromptExtracted(t *testing.T) {
	body := buildBatchExecuteRequest(t, "great do do do do do", "en")

	a := &Adapter{}
	p, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/x-www-form-urlencoded",
		EndpointPath: "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if p.Kind != normalize.KindAIChat {
		t.Fatalf("Kind: %v", p.Kind)
	}
	if p.DetectedSpec != adapterID {
		t.Errorf("DetectedSpec: %q want %q", p.DetectedSpec, adapterID)
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != normalize.RoleUser {
		t.Fatalf("messages: %+v", p.Messages)
	}
	got := p.Messages[0].Content[0].Text
	if got != "great do do do do do" {
		t.Errorf("user prompt: %q", got)
	}
	if p.Confidence < 0.8 {
		t.Errorf("confidence: %v", p.Confidence)
	}
}

func TestNormalize_BatchExecuteRequest_MalformedFReq(t *testing.T) {
	// f.req present but not valid outer JSON → ErrUnsupported,
	// Coordinator falls through to Tier 2 / Tier 3.
	body := []byte("f.req=not-json&at=xyz")
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/x-www-form-urlencoded",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for malformed f.req")
	}
}

// Response side

func TestNormalize_BatchExecuteResponse_SingleChunk(t *testing.T) {
	chunk := buildBatchExecuteChunk(t, "Haha, sounds good!", "3 Flash")
	body := append([]byte(")]}'\n\n"), chunk...)

	a := &Adapter{}
	p, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionResponse,
		ContentType:  "application/json",
		EndpointPath: "/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if p.Kind != normalize.KindAIChat {
		t.Fatalf("Kind: %v", p.Kind)
	}
	if !p.Stream {
		t.Errorf("Stream flag should be true for response")
	}
	if len(p.Messages) != 1 || p.Messages[0].Role != normalize.RoleAssistant {
		t.Fatalf("messages: %+v", p.Messages)
	}
	if p.Messages[0].Content[0].Text != "Haha, sounds good!" {
		t.Fatalf("assistant text: %q", p.Messages[0].Content[0].Text)
	}
	if p.Model != "3 Flash" {
		t.Errorf("model: %q want '3 Flash'", p.Model)
	}
}

func TestNormalize_BatchExecuteResponse_MultiChunkCumulative(t *testing.T) {
	// Mirrors the real traffic_event 78911179 capture pattern: each
	// chunk repeats the entire reply so far. The LAST chunk wins.
	c1 := buildBatchExecuteChunk(t, "Haha", "3 Flash")
	c2 := buildBatchExecuteChunk(t, "Haha, sounds like", "3 Flash")
	c3 := buildBatchExecuteChunk(t, "Haha, sounds like you've got some serious momentum going!", "3 Flash")
	c4 := buildBatchExecuteChunk(
		t,
		"Haha, sounds like you've got some serious momentum going! \n\nWhat's on the agenda to get done today? Hit me with it, and let's cross some things off that to-do list!",
		"3 Flash",
	)
	body := append([]byte(")]}'\n\n"), c1...)
	body = append(body, c2...)
	body = append(body, c3...)
	body = append(body, c4...)

	a := &Adapter{}
	p, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	final := p.Messages[0].Content[0].Text
	if !strings.Contains(final, "let's cross some things off that to-do list") {
		t.Fatalf("did not pick up FINAL cumulative chunk; got: %q", final)
	}
	if !strings.Contains(final, "Haha, sounds like") {
		t.Errorf("text: %q", final)
	}
	if p.Model != "3 Flash" {
		t.Errorf("model: %q", p.Model)
	}
	// 4+ frames + model → high confidence
	if p.Confidence < 0.9 {
		t.Errorf("confidence: %v want >= 0.9 (4 chunks + model)", p.Confidence)
	}
}

func TestNormalize_BatchExecuteResponse_MissingXSSIPrefix(t *testing.T) {
	chunk := buildBatchExecuteChunk(t, "hello", "3 Flash")
	body := append([]byte("177\n"), chunk...) // no )]}' prefix
	a := &Adapter{}
	// Direction unset → adapter tries request sniff (no), falls to
	// extract fallback (also fails — not JSON-chat shape) → eventually
	// ErrUnsupported. Direction=Response would behave the same since
	// isXSSIPrefixed is false.
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionResponse,
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for response without XSSI prefix")
	}
}

func TestNormalize_NotGeminiWeb_JSONFallback(t *testing.T) {
	// Body that ISN'T batchexecute and ISN'T XSSI-prefixed — the
	// defensive fallback path (extract.NormalizeForAdapter) takes over.
	// A real gemini-generate body should claim there.
	body := []byte(`{
		"contents": [{"role": "user", "parts": [{"text": "hi from gemini API shape"}]}],
		"model": "gemini-2.5-flash"
	}`)
	a := &Adapter{}
	p, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("expected fallback claim, got err: %v", err)
	}
	if p.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", p.Kind)
	}
}
