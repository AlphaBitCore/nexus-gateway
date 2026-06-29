package geminiweb

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// gemini.google.com's wire format is undocumented; the adapter is
// defensive (see package doc). Tests cover three categories:
//   1. Forward-compat JSON shapes (prompt / messages / contents) the
//      adapter handles deterministically.
//   2. Form-encoded RPC heuristic extraction with controlled-shape
//      payloads. These verify the heuristic walker behaves correctly
//      on representative inputs; real-world wire shapes may differ
//      and require live-fixture follow-up.
//   3. Defensive paths: malformed input, unknown schemas, error
//      envelope handling, the XSSI prefix stripper.

func TestAdapter_ID(t *testing.T) {
	a := &Adapter{}
	if a.ID() != "gemini-web" {
		t.Errorf("ID=%q", a.ID())
	}
}

func TestAdapter_Configure(t *testing.T) {
	a := &Adapter{}
	if err := a.Configure(nil); err != nil {
		t.Errorf("Configure(nil)=%v", err)
	}
	if err := a.Configure(map[string]any{"foo": "bar"}); err != nil {
		t.Errorf("Configure(map)=%v", err)
	}
}

// JSON request shapes (forward-compat path)

func TestExtractRequest_PromptShape(t *testing.T) {
	body := []byte(`{"prompt":"What is the capital of France?","model":"gemini-3-pro"}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/AssistantNexusWebUi/data/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "What is the capital of France?" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["model"] != "gemini-3-pro" {
		t.Errorf("model=%q", nc.Metadata["model"])
	}
}

func TestExtractRequest_MessagesShape(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role":"user","content":[{"type":"text","text":"hi from messages shape"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/AssistantNexusWebUi/data/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from messages shape" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ContentsShape(t *testing.T) {
	// If the web client ever migrates to the public Gemini API shape.
	body := []byte(`{
		"contents": [
			{"role":"user","parts":[{"text":"hi from contents shape"}]}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/AssistantNexusWebUi/data/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi from contents shape" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_JSONUnknownShape(t *testing.T) {
	// Plain JSON object with no recognised content fields → ErrUnknownSchema.
	body := []byte(`{"session_id":"abc","at":"deadbeef"}`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Form-encoded RPC shape (heuristic extraction)

// TestExtractRequest_FormEncodedFreqPayload covers the typical
// gemini.google.com obfuscated body: an `f.req` parameter containing
// a JSON-stringified array, with the user's prompt as a long string
// leaf inside.
func TestExtractRequest_FormEncodedFreqPayload(t *testing.T) {
	// Outer: [null, "<inner-json>"]. Inner contains the user prompt
	// alongside other RPC fields. The heuristic walker collects
	// strings >= minHeuristicTextLen and ignores short tokens.
	body := []byte(`f.req=%5Bnull%2C%22%5B%5B%5C%22Tell+me+a+long+story+about+the+history+of+computing.%5C%22%2C0%5D%2Cnull%2C%5C%22en-US%5C%22%5D%22%5D&at=AT_TOKEN_PLACEHOLDER&_reqid=12345&rt=c`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/AssistantNexusWebUi/data/StreamGenerate")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "Tell me a long story about the history of computing.") {
			found = true
		}
	}
	if !found {
		t.Errorf("heuristic walker missed prompt; Segments=%v", nc.Segments)
	}
	// Short tokens (en-US, _reqid value) must NOT leak in.
	for _, s := range nc.Segments {
		if s == "en-US" || s == "12345" {
			t.Errorf("short token leaked into Segments: %q", s)
		}
	}
}

// TestExtractRequest_FormEncodedShortPromptSkipped pins that prompts
// shorter than minHeuristicTextLen are conservatively skipped — better
// to miss a tiny prompt than to flood Segments with id strings. This is
// a known limitation of the heuristic.
func TestExtractRequest_FormEncodedShortPromptSkipped(t *testing.T) {
	// "hi" is below the threshold.
	body := []byte(`f.req=%5Bnull%2C%22%5B%5B%5C%22hi%5C%22%5D%5D%22%5D`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for sub-threshold prompt", nc.Segments)
	}
}

// TestExtractRequest_FormEncodedMissingFreq pins behaviour when the
// form body has form keys but no `f.req`. → ErrUnknownSchema.
func TestExtractRequest_FormEncodedMissingFreq(t *testing.T) {
	body := []byte(`at=ABC&_reqid=12345`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Defensive paths

func TestExtractRequest_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), nil, "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_TotallyUnknownBody(t *testing.T) {
	// Not JSON, not form-encoded → ErrUnknownSchema.
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`<binary garbage>`), "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractStreamChunk_HeuristicWalk(t *testing.T) {
	// Streaming chunk with an XSSI prefix and a JSON array carrying the
	// assistant's text as a long string leaf.
	chunk := []byte(`)]}'
[["wrb.fr","StreamGenerate",["[[null,\"This is a fairly long assistant reply that should be captured by the heuristic walker.\"]]"]]]`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "long assistant reply") {
			found = true
		}
	}
	if !found {
		t.Errorf("heuristic walker missed assistant text; Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_OnlyShortStrings(t *testing.T) {
	// All leaves shorter than threshold → no Segments.
	chunk := []byte(`)]}'
[["wrb.fr","sg",["[]"]]]`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty for short-only", nc.Segments)
	}
}

func TestExtractStreamChunk_DefensiveOnInvalidJSON(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`not json at all`), "/_/...")
	if err != nil {
		t.Errorf("err=%v want nil for invalid JSON (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_EmptyChunk(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), nil, "/_/...")
	if err != nil {
		t.Errorf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// XSSI prefix stripper

func TestStripXSSIPrefix_Removes(t *testing.T) {
	out := stripXSSIPrefix([]byte(`)]}'
{"foo":1}`))
	if string(out) != `{"foo":1}` {
		t.Errorf("stripped=%q", out)
	}
}

func TestStripXSSIPrefix_Idempotent(t *testing.T) {
	in := []byte(`{"foo":1}`)
	out := stripXSSIPrefix(in)
	if string(out) != `{"foo":1}` {
		t.Errorf("stripped=%q want unchanged", out)
	}
}

// ExtractResponse — error envelope coverage

func TestExtractResponse_ErrorEnvelope(t *testing.T) {
	body := []byte(`)]}'
{"error":{"code":403,"message":"Permission denied"}}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "Permission denied" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error meta=%q", nc.Metadata["error"])
	}
}

func TestExtractResponse_DetailString(t *testing.T) {
	body := []byte(`{"detail":"forbidden"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "forbidden" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_NonErrorJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{"foo":"bar"}`), "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// DetectRequestMeta + DetectResponseUsage + Rewrite contracts

func TestDetectRequestMeta_ProviderAndModel(t *testing.T) {
	body := []byte(`{"prompt":"hi","model":"gemini-3-pro"}`)
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://gemini.google.com/_/...", nil)
	meta := a.DetectRequestMeta(r, body)
	if meta.Provider != "gemini-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "gemini-3-pro" {
		t.Errorf("Model=%q", meta.Model)
	}
}

func TestDetectRequestMeta_FormBody(t *testing.T) {
	// Form-encoded bodies have no JSON `model` field — adapter returns
	// Provider but empty Model.
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://gemini.google.com/_/...", nil)
	meta := a.DetectRequestMeta(r, []byte(`f.req=%5B%5D`))
	if meta.Provider != "gemini-web" {
		t.Errorf("Provider=%q", meta.Provider)
	}
	if meta.Model != "" {
		t.Errorf("Model=%q want empty", meta.Model)
	}
}

func TestDetectResponseUsage_NonLLMSentinel(t *testing.T) {
	a := &Adapter{}
	usage := a.DetectResponseUsage(nil, []byte(`{}`))
	if usage.Status != traffic.UsageStatusNonLLM {
		t.Errorf("Status=%q want non_llm", usage.Status)
	}
}

func TestRewriteRequestBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{"prompt":"hi"}`)
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/_/...", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

func TestRewriteResponseBody_Unsupported(t *testing.T) {
	a := &Adapter{}
	body := []byte(`{}`)
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/_/...", traffic.NormalizedContent{})
	if !errors.Is(err, traffic.ErrRewriteUnsupported) {
		t.Errorf("err=%v want ErrRewriteUnsupported", err)
	}
	if n != 0 || string(out) != string(body) {
		t.Errorf("body modified")
	}
}

// extractJSONRequest — gap-closing branches

// TestExtractRequest_JSONNonObjectTopLevel covers extractJSONRequest
// line 222-224: a valid JSON array/scalar at the top level (not an
// object) returns ErrUnknownSchema.
func TestExtractRequest_JSONNonObjectTopLevel(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), []byte(`[1,2,3]`), "/_/...")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema for top-level array", err)
	}
}

// TestExtractRequest_MessagesContentString covers extractJSONRequest
// lines 240-242: messages-shape body with content as a plain string
// (not an array of parts).
func TestExtractRequest_MessagesContentString(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":"plain string content"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "plain string content" {
		t.Errorf("Segments=%v want [plain string content]", nc.Segments)
	}
}

// isFormEncodedBody — large-body truncation

// TestIsFormEncodedBody_LargeBodyTruncation covers the >1024 fast-reject
// truncation path (lines 289-294). The first 1024 bytes contain f.req=,
// so the heuristic still claims the body.
func TestIsFormEncodedBody_LargeBodyTruncation(t *testing.T) {
	// Build a body whose first 1024 bytes contain "f.req=" early, then
	// a large blob after — the truncation must not skip the prefix.
	body := []byte("f.req=%5Bnull%2C%22%5B%5B%5C%22Tell+me+a+long+story+about+the+history+of+computing+please.%5C%22%5D%5D%22%5D&filler=" + strings.Repeat("x", 4096))
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "Tell me a long story") {
			found = true
		}
	}
	if !found {
		t.Errorf("large form body lost prompt; Segments=%v", nc.Segments)
	}
}

// extractFormEncodedRequest — outer-shape gap branches

// TestExtractRequest_FormEncodedMalformedPercentEscape covers the
// url.ParseQuery error branch (lines 317-319).
func TestExtractRequest_FormEncodedMalformedPercentEscape(t *testing.T) {
	// `%ZZ` is an invalid percent-escape — ParseQuery returns an error.
	body := []byte(`f.req=%ZZ&at=xyz`)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// TestExtractRequest_FormEncodedOuterPlainString covers extractFormEncodedRequest
// lines 331-332: outer is a JSON string (not an array). The string is
// used directly as `inner`.
func TestExtractRequest_FormEncodedOuterPlainString(t *testing.T) {
	// f.req is a JSON string whose decoded value parses as another JSON
	// string — used as inner.
	body := []byte(`f.req=%22%5B%5B%5C%22a+sufficiently+long+prompt+for+the+walker%5C%22%5D%5D%22`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "a sufficiently long prompt") {
			found = true
		}
	}
	if !found {
		t.Errorf("outer-string branch lost prompt; Segments=%v", nc.Segments)
	}
}

// TestExtractRequest_FormEncodedOuterFallthrough covers lines 333-336:
// outer is neither an array-with-string-at-[1] nor a plain string — the
// walker falls back to scanning the raw outer payload directly.
func TestExtractRequest_FormEncodedOuterFallthrough(t *testing.T) {
	// outer is a JSON object — the explicit fall-through path uses freq
	// bytes directly.
	body := []byte(`f.req=%7B%22prompt%22%3A%22a+sufficiently+long+fallthrough+prompt+here%22%7D`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "fallthrough prompt") {
			found = true
		}
	}
	if !found {
		t.Errorf("outer fallthrough lost prompt; Segments=%v", nc.Segments)
	}
}

// TestExtractRequest_FormEncodedInnerNotValidJSON covers lines 338-340:
// outer[1] is a string but its content is not valid JSON. Adapter
// falls back to walking the raw outer freq.
func TestExtractRequest_FormEncodedInnerNotValidJSON(t *testing.T) {
	// outer = [null, "this is just a long string, not valid inner JSON though"]
	// The walker then receives the original freq bytes and still finds
	// the long string.
	body := []byte(`f.req=%5Bnull%2C%22this+is+just+a+long+string+not+valid+inner+JSON+though%22%5D`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/_/...")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	found := false
	for _, s := range nc.Segments {
		if strings.Contains(s, "this is just a long string") {
			found = true
		}
	}
	if !found {
		t.Errorf("inner-invalid-JSON fall-back lost prompt; Segments=%v", nc.Segments)
	}
}

// ExtractResponse — malformed-after-strip branch

// TestExtractResponse_MalformedAfterXSSI covers ExtractResponse lines
// 398-400: stripping the XSSI prefix leaves invalid JSON.
func TestExtractResponse_MalformedAfterXSSI(t *testing.T) {
	body := []byte(`)]}'
not json at all just plain text`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/_/...")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

// trimASCIIWhitespace — trailing-trim loop

// TestTrimASCIIWhitespace_Trailing covers the trailing-trim loop at
// lines 435-441 of geminiweb.go. The leading-only test in
// TestStripXSSIPrefix_Idempotent covers the no-op path; this test
// covers actual trailing whitespace bytes.
func TestTrimASCIIWhitespace_Trailing(t *testing.T) {
	// All four whitespace flavours at the tail.
	out := trimASCIIWhitespace([]byte("hello \n\r\t"))
	if string(out) != "hello" {
		t.Errorf("trimmed=%q want %q", out, "hello")
	}
}

// Normalize — empty body + looksLikeJSON edge cases

// TestNormalize_EmptyRawBody covers normalize.go line 28-30: empty raw
// body short-circuits to ErrUnsupported.
func TestNormalize_EmptyRawBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), nil, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
	})
	if !errors.Is(err, normalize.ErrUnsupported) {
		t.Errorf("err=%v want ErrUnsupported", err)
	}
}

// TestLooksLikeJSON covers the looksLikeJSON helper, including the
// whitespace-continue branch (line 66-67) and the empty/all-whitespace
// fall-through (line 74).
func TestLooksLikeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"leading-whitespace-object", []byte("   \n\t{\"a\":1}"), true},
		{"leading-whitespace-array", []byte("\r\n  [1,2]"), true},
		{"plain-object", []byte(`{"a":1}`), true},
		{"plain-array", []byte(`[1,2]`), true},
		{"text-starts-with-letter", []byte("hello"), false},
		{"all-whitespace", []byte("   \n\t  "), false},
		{"empty", []byte{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeJSON(c.in); got != c.want {
				t.Errorf("looksLikeJSON(%q)=%v want %v", c.in, got, c.want)
			}
		})
	}
}
