package anthropic

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Normalize — Tier-1 entry point for the Anthropic Messages API.

// TestNormalize_RequestClaimsAnthropicMessages pins that a /v1/messages
// request body claims Tier-1 with DetectedSpec=anthropic and surfaces
// the user prompt + role.
func TestNormalize_RequestClaimsAnthropicMessages(t *testing.T) {
	body := []byte(`{
		"anthropic_version":"2023-06-01",
		"model":"claude-3-5-sonnet-20241022",
		"max_tokens":1024,
		"messages":[{"role":"user","content":[{"type":"text","text":"hello claude"}]}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "anthropic",
		Direction:    normalize.DirectionRequest,
		ContentType:  "application/json",
		EndpointPath: "/v1/messages",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Errorf("Kind=%v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "anthropic" {
		t.Errorf("DetectedSpec=%q want anthropic", payload.DetectedSpec)
	}
	if len(payload.Messages) != 1 {
		t.Fatalf("messages=%d want 1", len(payload.Messages))
	}
	if payload.Messages[0].Role != normalize.RoleUser {
		t.Errorf("role=%v want user", payload.Messages[0].Role)
	}
}

// TestNormalize_ResponseClaimsAnthropicMessages pins the response-side
// route: a non-stream Messages response claims Tier-1 against the
// `anthropic-messages-nonstream` spec.
func TestNormalize_ResponseClaimsAnthropicMessages(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"model":"claude-3-5-sonnet-20241022",
		"content":[{"type":"text","text":"hi from claude"}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":4,"output_tokens":3}
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "anthropic",
		Direction:    normalize.DirectionResponse,
		ContentType:  "application/json",
		EndpointPath: "/v1/messages",
	})
	if err != nil {
		t.Fatalf("Normalize err=%v", err)
	}
	if payload.DetectedSpec != "anthropic" {
		t.Errorf("DetectedSpec=%q want anthropic", payload.DetectedSpec)
	}
}

// TestNormalize_NonAnthropicBody pins that a body that doesn't match
// any anthropic spec falls through with normalize.ErrUnsupported so the
// Coordinator advances to Tier 2.
func TestNormalize_NonAnthropicBody(t *testing.T) {
	body := []byte(`{"foo":"bar","count":42}`)
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "anthropic",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for non-anthropic body")
	}
}

// ExtractRequest — branches missed by existing tests:
//   - service_tier and anthropic_version surface on Metadata
//   - a message with no `content` key is skipped without erroring

// TestExtractRequest_ServiceTierAndAnthropicVersionMetadata pins both
// metadata fields land on the returned Metadata map.
func TestExtractRequest_ServiceTierAndAnthropicVersionMetadata(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"service_tier":"priority",
		"anthropic_version":"2023-06-01",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["service_tier"] != "priority" {
		t.Errorf("service_tier=%q want priority", nc.Metadata["service_tier"])
	}
	if nc.Metadata["anthropic_version"] != "2023-06-01" {
		t.Errorf("anthropic_version=%q want 2023-06-01", nc.Metadata["anthropic_version"])
	}
}

// TestExtractRequest_MessageWithoutContent pins that a malformed
// message lacking the `content` field is skipped silently without
// erroring out — the extractor walks the rest of the messages array.
func TestExtractRequest_MessageWithoutContent(t *testing.T) {
	body := []byte(`{
		"model":"claude-3-5-sonnet-20241022",
		"messages":[
			{"role":"user"},
			{"role":"assistant","content":"hi"}
		]
	}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "hi" {
		t.Errorf("Segments=%v want [hi]", nc.Segments)
	}
}

// ExtractStreamChunk — message_delta carrying stop_sequence (non-empty).

// TestExtractStreamChunk_MessageDeltaStopSequence pins the stop_sequence
// branch in message_delta — both stop_reason AND stop_sequence land on
// Metadata when present (existing tests only cover stop_reason).
func TestExtractStreamChunk_MessageDeltaStopSequence(t *testing.T) {
	chunk := []byte(`{"type":"message_delta","delta":{"stop_reason":"stop_sequence","stop_sequence":"END"},"usage":{"output_tokens":3}}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/v1/messages")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if nc.Metadata["stop_reason"] != "stop_sequence" {
		t.Errorf("stop_reason=%q want stop_sequence", nc.Metadata["stop_reason"])
	}
	if nc.Metadata["stop_sequence"] != "END" {
		t.Errorf("stop_sequence=%q want END", nc.Metadata["stop_sequence"])
	}
}

// DetectResponseUsage — empty body and malformed body branches.

func TestDetectResponseUsage_EmptyBody(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, nil)
	if um.Status != traffic.UsageStatusNoBody {
		t.Errorf("Status=%q want no_body", um.Status)
	}
}

func TestDetectResponseUsage_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	um := a.DetectResponseUsage(nil, []byte(`not json`))
	if um.Status != traffic.UsageStatusParseFailed {
		t.Errorf("Status=%q want parse_failed", um.Status)
	}
}

// RewriteRequestBody — branches missed by existing tests.

// TestRewriteRequestBody_SystemString_OutOfSegments pins early return
// when the system-string position has no replacement segment available.
// The original body is returned with written=0 (no mutation).
func TestRewriteRequestBody_SystemString_OutOfSegments(t *testing.T) {
	body := []byte(`{"system":"original","messages":[{"role":"user","content":"hi"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: nil})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0 (no segments to write)", n)
	}
	if got := gjson.GetBytes(out, "system").String(); got != "original" {
		t.Errorf("system mutated unexpectedly: %q", got)
	}
}

// TestRewriteRequestBody_SystemArray_NonTextSkipped pins that a
// non-text part inside the system array (e.g. an unknown future
// part type) is skipped — only `type:"text"` parts get rewritten.
func TestRewriteRequestBody_SystemArray_NonTextSkipped(t *testing.T) {
	body := []byte(`{
		"system":[
			{"type":"image","source":{"data":"x"}},
			{"type":"text","text":"replace me"}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"replaced", "hi"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	// non-text part untouched
	if got := gjson.GetBytes(out, "system.0.source.data").String(); got != "x" {
		t.Errorf("non-text system part mutated: %q", got)
	}
	// text part rewritten with first segment
	if got := gjson.GetBytes(out, "system.1.text").String(); got != "replaced" {
		t.Errorf("system.1.text=%q want replaced", got)
	}
}

// TestRewriteRequestBody_SystemArray_OutOfSegments pins early return
// when the segments slice runs out mid-array.
func TestRewriteRequestBody_SystemArray_OutOfSegments(t *testing.T) {
	body := []byte(`{
		"system":[
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"first"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1 (only first system slot filled)", n)
	}
	if got := gjson.GetBytes(out, "system.0.text").String(); got != "first" {
		t.Errorf("system.0.text=%q want first", got)
	}
	if got := gjson.GetBytes(out, "system.1.text").String(); got != "b" {
		t.Errorf("system.1.text=%q want b (untouched)", got)
	}
}

// TestRewriteRequestBody_MessageWithoutContent pins that a message
// lacking the `content` key is silently skipped in the rewrite walk
// — the rest of the messages array still receives replacements.
func TestRewriteRequestBody_MessageWithoutContent(t *testing.T) {
	body := []byte(`{
		"messages":[
			{"role":"user"},
			{"role":"assistant","content":"reply me"}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"replaced"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "messages.1.content").String(); got != "replaced" {
		t.Errorf("messages.1.content=%q want replaced", got)
	}
}

// TestRewriteRequestBody_StringContent_OutOfSegments pins early return
// at the string-content position when segments run out.
func TestRewriteRequestBody_StringContent_OutOfSegments(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]}`)
	a := &Adapter{}
	out, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"only-first"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
	if got := gjson.GetBytes(out, "messages.0.content").String(); got != "only-first" {
		t.Errorf("messages.0.content=%q want only-first", got)
	}
	if got := gjson.GetBytes(out, "messages.1.content").String(); got != "b" {
		t.Errorf("messages.1.content=%q want b (untouched)", got)
	}
}

// TestRewriteRequestBody_ArrayContent_OutOfSegments pins early return
// at the array-content text position.
func TestRewriteRequestBody_ArrayContent_OutOfSegments(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		]}]
	}`)
	a := &Adapter{}
	_, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"only-first"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
}

// TestRewriteRequestBody_ToolResultString_OutOfSegments pins the
// tool_result string-content out-of-segments path.
func TestRewriteRequestBody_ToolResultString_OutOfSegments(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t","content":"raw text"}
		]}]
	}`)
	a := &Adapter{}
	_, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: nil})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 0 {
		t.Errorf("n=%d want 0", n)
	}
}

// TestRewriteRequestBody_ToolResultArray_OutOfSegments pins the
// tool_result array-content out-of-segments path mid-walk.
func TestRewriteRequestBody_ToolResultArray_OutOfSegments(t *testing.T) {
	body := []byte(`{
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"t","content":[
				{"type":"text","text":"a"},
				{"type":"text","text":"b"}
			]}
		]}]
	}`)
	a := &Adapter{}
	_, n, err := a.RewriteRequestBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"only-first"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
}

// RewriteResponseBody — non-stream Messages responses.

func TestRewriteResponseBody_TextBlocks(t *testing.T) {
	body := []byte(`{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[
			{"type":"text","text":"raw SSN 123-45-6789"},
			{"type":"text","text":"and email a@b.com"}
		],
		"stop_reason":"end_turn"
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"raw SSN [REDACTED]", "and email [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 2 {
		t.Errorf("n=%d want 2", n)
	}
	if got := gjson.GetBytes(out, "content.0.text").String(); got != "raw SSN [REDACTED]" {
		t.Errorf("content.0.text=%q", got)
	}
	if got := gjson.GetBytes(out, "content.1.text").String(); got != "and email [REDACTED]" {
		t.Errorf("content.1.text=%q", got)
	}
}

// TestRewriteResponseBody_NonTextBlockSkipped pins that non-text
// content blocks (thinking, tool_use) are skipped by the rewriter —
// they live on ReasoningSegments / ToolCallSegments, not Segments.
func TestRewriteResponseBody_NonTextBlockSkipped(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"thinking","thinking":"internal trace"},
			{"type":"text","text":"raw email a@b.com"},
			{"type":"tool_use","id":"toolu_1","name":"f","input":{}}
		]
	}`)
	a := &Adapter{}
	out, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"raw email [REDACTED]"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1 (only the text block was rewritten)", n)
	}
	// thinking + tool_use untouched
	if got := gjson.GetBytes(out, "content.0.thinking").String(); got != "internal trace" {
		t.Errorf("thinking mutated: %q", got)
	}
	if got := gjson.GetBytes(out, "content.2.name").String(); got != "f" {
		t.Errorf("tool_use mutated: %q", got)
	}
	if got := gjson.GetBytes(out, "content.1.text").String(); got != "raw email [REDACTED]" {
		t.Errorf("text not rewritten: %q", got)
	}
}

// TestRewriteResponseBody_OutOfSegments pins early return when the
// segments slice runs out mid-walk.
func TestRewriteResponseBody_OutOfSegments(t *testing.T) {
	body := []byte(`{
		"content":[
			{"type":"text","text":"a"},
			{"type":"text","text":"b"}
		]
	}`)
	a := &Adapter{}
	_, n, err := a.RewriteResponseBody(context.Background(), body, "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"only-first"}})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Errorf("n=%d want 1", n)
	}
}

func TestRewriteResponseBody_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`not json`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestRewriteResponseBody_MissingContent pins ErrUnknownSchema when
// the response carries no `content` array (e.g. an error envelope).
func TestRewriteResponseBody_MissingContent(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"id":"msg_1"}`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if err == nil {
		t.Fatal("expected ErrUnknownSchema for body lacking content[]")
	}
}

// TestRewriteResponseBody_ContentNotArray pins ErrUnknownSchema when
// `content` exists but is not an array — defensive against future
// spec drift.
func TestRewriteResponseBody_ContentNotArray(t *testing.T) {
	a := &Adapter{}
	_, _, err := a.RewriteResponseBody(context.Background(), []byte(`{"content":"oops"}`), "/v1/messages",
		traffic.NormalizedContent{Segments: []string{"x"}})
	if err == nil {
		t.Fatal("expected ErrUnknownSchema for content=string")
	}
}
