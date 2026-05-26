package gemini

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

func TestRewriteRequestBody_ContentsParts(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[` +
		`{"text":"my email is a@b.com"},` +
		`{"inlineData":{"mimeType":"image/png","data":"abc"}},` +
		`{"text":"ok"}]}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/models/gemini:generateContent",
		traffic.NormalizedContent{Segments: []string{"my email is [REDACTED]", "ok"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "contents.0.parts.0.text").String(); got != "my email is [REDACTED]" {
		t.Errorf("parts[0] = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "contents.0.parts.1.inlineData.data").String(); got != "abc" {
		t.Errorf("inlineData mutated: %q", got)
	}
	if got := gjson.GetBytes(rewritten, "contents.0.parts.2.text").String(); got != "ok" {
		t.Errorf("parts[2] = %q", got)
	}
}

func TestRewriteRequestBody_SystemInstruction(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"be helpful"}]},` +
		`"contents":[{"role":"user","parts":[{"text":"ssn 1234"}]}]}`)

	a := &Adapter{}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/models/gemini:generateContent",
		traffic.NormalizedContent{Segments: []string{"be helpful", "ssn [REDACTED]"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("n = %d, want 2", n)
	}
	if got := gjson.GetBytes(rewritten, "systemInstruction.parts.0.text").String(); got != "be helpful" {
		t.Errorf("sys = %q", got)
	}
	if got := gjson.GetBytes(rewritten, "contents.0.parts.0.text").String(); got != "ssn [REDACTED]" {
		t.Errorf("content = %q", got)
	}
}

func TestRewriteRequestBody_RoundTrip(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"a"}]},` +
		`"contents":[{"role":"user","parts":[{"text":"b"},{"text":"c"}]}]}`)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/models/gemini:generateContent")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	rewritten, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/models/gemini:generateContent", nc)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if n != len(nc.Segments) {
		t.Errorf("n = %d, want %d", n, len(nc.Segments))
	}
	nc2, err := a.ExtractRequest(context.Background(), rewritten, "/v1/models/gemini:generateContent")
	if err != nil {
		t.Fatalf("re-extract: %v", err)
	}
	for i := range nc.Segments {
		if nc.Segments[i] != nc2.Segments[i] {
			t.Errorf("segment[%d] mismatch: %q vs %q", i, nc.Segments[i], nc2.Segments[i])
		}
	}
}

func TestRewriteRequestBody_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`not json`), "/v1/models/gemini:generateContent",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

func TestRewriteRequestBody_MissingContents(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteRequestBody(context.Background(), []byte(`{"model":"gemini"}`), "/v1/models/gemini:generateContent",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("expected ErrUnknownSchema, got %v", err)
	}
}

// RewriteResponseBody — candidates[].content.parts[].text rewrite path.

func TestRewriteResponseBody_Basic(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[
			{"text":"answer is secret"},
			{"functionCall":{"name":"f","args":{}}},
			{"text":"more secret"}
		]}}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"answer is [REDACTED]", "more [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.text").String(); got != "answer is [REDACTED]" {
		t.Errorf("parts[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.2.text").String(); got != "more [REDACTED]" {
		t.Errorf("parts[2]=%q", got)
	}
	// functionCall must be left untouched.
	if got := gjson.GetBytes(out, "candidates.0.content.parts.1.functionCall.name").String(); got != "f" {
		t.Errorf("functionCall mutated: %q", got)
	}
}

func TestRewriteResponseBody_Malformed(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{bad`), "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Errorf("err=%v want ErrMalformed", err)
	}
}

func TestRewriteResponseBody_MissingCandidates(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"promptFeedback":{}}`), "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// candidates present but not an array (e.g. error envelope) → ErrUnknownSchema.
func TestRewriteResponseBody_CandidatesNotArray(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"candidates":"oops"}`), "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Errorf("err=%v want ErrUnknownSchema", err)
	}
}

// Candidate without a content.parts array must be skipped without
// consuming a segment so subsequent valid candidates align.
func TestRewriteResponseBody_SkipsCandidateWithoutParts(t *testing.T) {
	body := []byte(`{
		"candidates":[
			{"finishReason":"SAFETY"},
			{"content":{"role":"model","parts":[{"text":"valid"}]}}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"VALID"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "candidates.1.content.parts.0.text").String(); got != "VALID" {
		t.Errorf("parts.0.text=%q", got)
	}
}

// Running out of segments mid-rewrite must stop iteration and preserve
// later text parts.
func TestRewriteResponseBody_FewerSegments_Stops(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"parts":[
			{"text":"a"},
			{"text":"b"}
		]}}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"A"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.0.text").String(); got != "A" {
		t.Errorf("[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "candidates.0.content.parts.1.text").String(); got != "b" {
		t.Errorf("[1] should survive: %q", got)
	}
}

// RewriteRequestBody — systemInstruction part edge cases (skip non-text)
// and fewer-segments short-circuits across the systemInstruction stage.

// systemInstruction.parts[i] with no `text` field (e.g. inlineData) must
// be skipped without consuming a segment.
func TestRewriteRequestBody_SystemInstruction_SkipsNonText(t *testing.T) {
	body := []byte(`{
		"systemInstruction":{"parts":[
			{"inlineData":{"mimeType":"image/png","data":"abc"}},
			{"text":"be helpful"}
		]},
		"contents":[{"role":"user","parts":[{"text":"hi"}]}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"BE NICE", "HELLO"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "systemInstruction.parts.1.text").String(); got != "BE NICE" {
		t.Errorf("sys[1]=%q", got)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.text").String(); got != "HELLO" {
		t.Errorf("content=%q", got)
	}
}

// systemInstruction stage runs out of segments — must stop without
// touching contents.
func TestRewriteRequestBody_SystemInstruction_FewerSegments_Stops(t *testing.T) {
	body := []byte(`{
		"systemInstruction":{"parts":[
			{"text":"sys1"},
			{"text":"sys2"}
		]},
		"contents":[{"role":"user","parts":[{"text":"keep"}]}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"SYS1"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.text").String(); got != "keep" {
		t.Errorf("contents must survive: %q", got)
	}
}

// RewriteRequestBody — contents[i].parts[] edge paths.

// `parts` not an array (malformed turn) must be skipped without erroring.
func TestRewriteRequestBody_ContentPartsNotArray_Skipped(t *testing.T) {
	body := []byte(`{
		"contents":[
			{"role":"user","parts":"oops"},
			{"role":"user","parts":[{"text":"valid"}]}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"VALID"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "contents.1.parts.0.text").String(); got != "VALID" {
		t.Errorf("valid=%q", got)
	}
}

// contents stage runs out of segments mid-text-part-walk.
func TestRewriteRequestBody_Contents_FewerSegments_Stops(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[
			{"text":"a"},
			{"text":"b"},
			{"text":"c"}
		]}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
		traffic.NormalizedContent{Segments: []string{"A"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.0.text").String(); got != "A" {
		t.Errorf("[0]=%q", got)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.1.text").String(); got != "b" {
		t.Errorf("[1] survives: %q", got)
	}
	if got := gjson.GetBytes(out, "contents.0.parts.2.text").String(); got != "c" {
		t.Errorf("[2] survives: %q", got)
	}
}

// functionResponse.response stage runs out of segments — must stop
// without writing the wrapped response.
func TestRewriteRequestBody_FunctionResponse_FewerSegments_Stops(t *testing.T) {
	t.Run("string_response_short_circuit", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"text":"first"}]},
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":"keep"}}]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
			traffic.NormalizedContent{Segments: []string{"FIRST"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.response").String(); got != "keep" {
			t.Errorf("functionResponse.response should survive: %q", got)
		}
	})
	t.Run("result_wrapper_short_circuit", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"text":"first"}]},
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"keep"}}}]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1beta/models/gemini-pro:generateContent",
			traffic.NormalizedContent{Segments: []string{"FIRST"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "contents.1.parts.0.functionResponse.response.result").String(); got != "keep" {
			t.Errorf("result should survive: %q", got)
		}
	})
}

// TestRewriteRequestBody_FunctionResponse covers the audit gap: tool
// returns extracted from functionResponse.response (or .response.result)
// must be writable back through Rewrite so PII redaction round-trips.
func TestRewriteRequestBody_FunctionResponse(t *testing.T) {
	t.Run("result_wrapper", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"raw addr 1 Main St"}}}]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/models/gemini:generateContent",
			traffic.NormalizedContent{Segments: []string{"raw addr [REDACTED]"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.0.functionResponse.response.result").String(); got != "raw addr [REDACTED]" {
			t.Errorf("response.result=%q", got)
		}
	})
	t.Run("string_response", func(t *testing.T) {
		body := []byte(`{
			"contents":[
				{"role":"user","parts":[{"functionResponse":{"name":"f","response":"raw text"}}]}
			]
		}`)
		a := &Adapter{}
		out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/models/gemini:generateContent",
			traffic.NormalizedContent{Segments: []string{"clean text"}})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("n=%d want 1", n)
		}
		if got := gjson.GetBytes(out, "contents.0.parts.0.functionResponse.response").String(); got != "clean text" {
			t.Errorf("response=%q", got)
		}
	})
}
