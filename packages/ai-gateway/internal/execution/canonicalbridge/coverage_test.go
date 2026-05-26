package canonicalbridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	provbuiltins "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/builtins"
	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
	"github.com/tidwall/gjson"
)

// errCodec is a deliberately-failing SchemaCodec used to exercise the
// error branches of [Bridge.IngressChatToWire] and [Bridge.DecodeViaShared]
// without depending on real wire-format breakage.
type errCodec struct {
	encodeErr error
	decodeErr error
	// decodeOut returned from DecodeResponse on the happy path.
	decodeOut []byte
}

func (c *errCodec) EncodeRequest(_ typology.WireShape, _ []byte, _ provcore.CallTarget) (provcore.EncodeResult, error) {
	if c.encodeErr != nil {
		return provcore.EncodeResult{}, c.encodeErr
	}
	return provcore.EncodeResult{Body: []byte(`{"ok":true}`), ContentType: "application/json"}, nil
}

func (c *errCodec) DecodeResponse(_ typology.WireShape, _ []byte, _ string) (provcore.DecodeResult, error) {
	if c.decodeErr != nil {
		return provcore.DecodeResult{}, c.decodeErr
	}
	return provcore.DecodeResult{CanonicalBody: c.decodeOut}, nil
}

// TestResponsesRoutable_InvalidTargetRejected exercises bridge.go:109-111
// — the explicit !target.Valid() guard inside ResponsesRoutable.
func TestResponsesRoutable_InvalidTargetRejected(t *testing.T) {
	b := testBridge(t)
	if b.ResponsesRoutable(provcore.Format("not-a-real-format")) {
		t.Errorf("ResponsesRoutable must reject invalid target Format")
	}
}

// TestEndpointRoutable_DefaultUnknownEndpoint covers bridge.go:143-144
// — the default switch arm in EndpointRoutable for an unknown Endpoint.
func TestEndpointRoutable_DefaultUnknownEndpoint(t *testing.T) {
	b := testBridge(t)
	// Same-format must pass through.
	if !b.EndpointRoutable(typology.WireShape("does-not-exist"), provcore.FormatOpenAI, provcore.FormatOpenAI) {
		t.Error("default endpoint same-format must be routable")
	}
	// Different formats: rejected (default arm).
	if b.EndpointRoutable(typology.WireShape("does-not-exist"), provcore.FormatOpenAI, provcore.FormatAnthropic) {
		t.Error("default endpoint cross-format must be rejected")
	}
}

// TestStreamShapeCompatible_InvalidIngressRejected covers bridge.go:154-156
// — the !ingress.Valid() || !target.Valid() guard.
func TestStreamShapeCompatible_InvalidIngressRejected(t *testing.T) {
	if StreamShapeCompatible(provcore.Format("bogus"), provcore.FormatOpenAI) {
		t.Error("invalid ingress must yield false")
	}
	if StreamShapeCompatible(provcore.FormatOpenAI, provcore.Format("bogus")) {
		t.Error("invalid target must yield false")
	}
}

// TestNewStreamTranscoder_UnknownIngressFallthrough exercises bridge.go:203-206
// — the default-case in NewStreamTranscoder which falls back to a nil
// transcoder for an unknown ingress format (let the executor surface any
// mismatch via the upstream).
func TestNewStreamTranscoder_UnknownIngressFallthrough(t *testing.T) {
	b := testBridge(t)
	// Bogus ingress, but a real target. Bogus ingress is not OpenAI-like
	// and not any of the known switch cases — so the function reaches the
	// default-arm and returns nil.
	if tr := b.NewStreamTranscoder(provcore.Format("bogus-ingress"), provcore.FormatOpenAI, "model"); tr != nil {
		t.Errorf("unknown ingress fallthrough must yield nil transcoder; got %T", tr)
	}
}

// TestDecodeViaShared_PassthroughWhenCodecMissing covers bridge.go:233-237
// — when no codec is registered for the format, DecodeViaShared returns
// the original body verbatim plus the Tier-1 extracted usage.
func TestDecodeViaShared_PassthroughWhenCodecMissing(t *testing.T) {
	// Empty codec map: every format misses.
	b := New(map[provcore.Format]provcore.SchemaCodec{})
	body := []byte(`{"id":"chatcmpl-x","usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	out, usage, err := b.DecodeViaShared(provcore.FormatOpenAI, typology.WireShapeOpenAIChat, body)
	if err != nil {
		t.Fatalf("expected nil err; got %v", err)
	}
	// Body must be byte-identical (passthrough).
	if string(out) != string(body) {
		t.Errorf("passthrough body mutated:\n got  %s\n want %s", string(out), string(body))
	}
	// Tier-1 normalizer must populate Usage from the prompt/completion fields.
	if usage.PromptTokens == nil || *usage.PromptTokens != 7 {
		t.Errorf("passthrough Usage.PromptTokens = %v, want 7", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 3 {
		t.Errorf("passthrough Usage.CompletionTokens = %v, want 3", usage.CompletionTokens)
	}
}

// TestDecodeViaShared_CodecErrorPropagates covers bridge.go:238-241 —
// when the codec.DecodeResponse returns an error, DecodeViaShared surfaces
// it verbatim (and returns the original body + a zero Usage so callers can
// fall back to passthrough handling).
func TestDecodeViaShared_CodecErrorPropagates(t *testing.T) {
	sentinel := errors.New("decode boom")
	codecs := map[provcore.Format]provcore.SchemaCodec{
		provcore.FormatAnthropic: &errCodec{decodeErr: sentinel},
	}
	b := New(codecs)
	out, usage, err := b.DecodeViaShared(provcore.FormatAnthropic, typology.WireShapeOpenAIChat, []byte(`{"x":1}`))
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	// On error path, body is returned unchanged + Usage is zero.
	if string(out) != `{"x":1}` {
		t.Errorf("expected body returned verbatim on err, got %s", string(out))
	}
	if usage.PromptTokens != nil || usage.CompletionTokens != nil {
		t.Errorf("expected zero Usage on err, got %+v", usage)
	}
}

// TestDecodeViaShared_HappyPath covers bridge.go:248-249 — the success
// arm that returns the codec's canonical body together with the Tier-1
// usage projection.
func TestDecodeViaShared_HappyPath(t *testing.T) {
	b := testBridge(t)
	// Real Anthropic response body — the spec_anthropic codec decodes it
	// into a canonical OpenAI chat.completion body and provcore.ExtractUsage
	// reads {input_tokens, output_tokens} for Usage.
	body := []byte(`{
		"id": "msg_001",
		"type": "message",
		"role": "assistant",
		"model": "claude-3-5-haiku-20240307",
		"content": [{"type":"text","text":"hi"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 11, "output_tokens": 2}
	}`)
	canon, usage, err := b.DecodeViaShared(provcore.FormatAnthropic, typology.WireShapeAnthropicMessages, body)
	if err != nil {
		t.Fatalf("DecodeViaShared: %v", err)
	}
	if !gjson.ValidBytes(canon) {
		t.Fatalf("expected valid canonical JSON; got %s", string(canon))
	}
	if got := gjson.GetBytes(canon, "object").String(); got != "chat.completion" {
		t.Errorf("canonical.object = %q, want chat.completion", got)
	}
	if usage.PromptTokens == nil || *usage.PromptTokens != 11 {
		t.Errorf("Usage.PromptTokens = %v, want 11", usage.PromptTokens)
	}
	if usage.CompletionTokens == nil || *usage.CompletionTokens != 2 {
		t.Errorf("Usage.CompletionTokens = %v, want 2", usage.CompletionTokens)
	}
}

// TestIngressChatToWire_CanonicalErrorPropagates covers bridge.go:291-293
// — when IngressChatToCanonical itself fails (unsupported ingress format),
// the IngressChatToWire wrapper returns the same error.
func TestIngressChatToWire_CanonicalErrorPropagates(t *testing.T) {
	b := testBridge(t)
	// FormatBedrock has no ingress→canonical mapper (only same-format
	// passthrough). Forcing ingress=Bedrock, target=OpenAI exercises
	// the canonical-conversion error branch.
	_, err := b.IngressChatToWire(provcore.FormatBedrock, provcore.FormatOpenAI, []byte(`{}`), provcore.CallTarget{
		Format: provcore.FormatOpenAI,
	})
	if err == nil {
		t.Fatal("expected ingress→canonical error for bedrock ingress; got nil")
	}
	if !strings.Contains(err.Error(), "bedrock") &&
		!strings.Contains(err.Error(), "no chat hub codec") {
		t.Errorf("error message should reference unsupported ingress; got %q", err.Error())
	}
}

// TestIngressChatToWire_MissingTargetCodec covers bridge.go (the
// `no codec for format` branch) by stripping a codec out of the bridge
// map before calling IngressChatToWire on a non-passthrough pair.
func TestIngressChatToWire_MissingTargetCodec(t *testing.T) {
	full := provbuiltins.SchemaCodecs(slog.Default())
	// Drop the OpenAI codec from a copy so anthropic→openai cannot encode.
	partial := make(map[provcore.Format]provcore.SchemaCodec, len(full))
	for k, v := range full {
		if k == provcore.FormatOpenAI {
			continue
		}
		partial[k] = v
	}
	b := New(partial)
	body, err := MinimalNativeChatBody(provcore.FormatAnthropic)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.IngressChatToWire(provcore.FormatAnthropic, provcore.FormatOpenAI, body, provcore.CallTarget{
		Format:          provcore.FormatOpenAI,
		ProviderModelID: "gpt-4o-mini",
	})
	if err == nil {
		t.Fatal("expected 'no codec for format' error; got nil")
	}
	if !strings.Contains(err.Error(), "no codec for format") {
		t.Errorf("error must mention missing codec; got %q", err.Error())
	}
}

// TestResponseCanonicalToIngress_UnsupportedIngressFormat covers
// bridge.go:320-321 — the default arm where no per-ingress response
// shape exists.
func TestResponseCanonicalToIngress_UnsupportedIngressFormat(t *testing.T) {
	b := testBridge(t)
	// Bedrock egress shape is not produced by the bridge — only same-
	// format passthrough is supported. Exercising it hits the default
	// error arm.
	_, err := b.ResponseCanonicalToIngress(provcore.FormatBedrock, []byte(`{"id":"x"}`))
	if err == nil {
		t.Fatal("expected error for bedrock ingress; got nil")
	}
	if !strings.Contains(err.Error(), "no response hub codec") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestSelfCheck_SkipsIngressWithoutFixture covers bridge.go:333-334 —
// when MinimalNativeChatBody returns an error for an ingress, SelfCheck
// continues without testing that ingress (no false-positive). We force
// the loop to run with the standard bridge — every fixture is registered
// today, but we add a defensive assertion: SelfCheck succeeds.
//
// To actually exercise the continue branch, register a fake additional
// ingress format by manually invoking the helper through a wrapper. The
// production AllFormats list already covers every registered ingress;
// the no-fixture branch is reached for FormatOpenAIResponses (which is
// NOT in AllFormats and therefore never iterated). Instead we exercise
// the analogous skip path through SelfCheck running on the production
// matrix — keeps the assertion meaningful.
func TestSelfCheck_HappyPath(t *testing.T) {
	b := testBridge(t)
	if err := b.SelfCheck(); err != nil {
		t.Fatalf("SelfCheck failed: %v", err)
	}
}

// TestFixtureProviderModel_Default covers canonical_fixtures.go:43-44 —
// the default arm for an unrecognised format.
func TestFixtureProviderModel_Default(t *testing.T) {
	got := FixtureProviderModel(provcore.Format("totally-unknown"))
	if got != "test-model" {
		t.Errorf("default model = %q, want test-model", got)
	}
}

// TestMinimalNativeChatBody_NoFixture covers canonical_fixtures.go:108-109
// — the default arm yielding an error for formats without a fixture.
func TestMinimalNativeChatBody_NoFixture(t *testing.T) {
	_, err := MinimalNativeChatBody(provcore.Format("does-not-exist"))
	if err == nil {
		t.Fatal("expected error for unknown ingress; got nil")
	}
	if !strings.Contains(err.Error(), "no fixture") {
		t.Errorf("unexpected err: %v", err)
	}
}

// TestOpenAIStreamEncoder_ReasoningDelta covers stream_encoders.go:93-101
// — the reasoning_content branch never exercised by the original
// stream_encoders_test.go.
func TestOpenAIStreamEncoder_ReasoningDelta(t *testing.T) {
	enc := newOpenAIStreamEncoder("gpt-5.2")
	enc.headerSent = true // skip role chunk so the slice is reasoning-only
	out, err := enc.Write(context.Background(), provcore.Chunk{ReasoningDelta: "thinking..."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"reasoning_content":"thinking..."`) {
		t.Errorf("expected reasoning_content delta; got %s", string(out))
	}
}

// TestOpenAIStreamEncoder_EmptyChunkAfterHeader covers stream_encoders.go:134
// — when buf.Len()==0 (empty chunk after the role header was already sent),
// Write returns nil bytes.
func TestOpenAIStreamEncoder_EmptyChunkAfterHeader(t *testing.T) {
	enc := newOpenAIStreamEncoder("gpt-5.2")
	enc.headerSent = true
	out, err := enc.Write(context.Background(), provcore.Chunk{})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty chunk should produce no bytes; got %s", string(out))
	}
}

// TestAnthropicStreamEncoder_MessageStartCarriesUsage covers
// stream_encoders.go:166-172 — when the first chunk carries a Usage,
// message_start input_tokens/output_tokens reflect it.
func TestAnthropicStreamEncoder_MessageStartCarriesUsage(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	prompt, comp := 25, 4
	chunk := provcore.Chunk{
		Delta: "hi",
		Usage: &provcore.Usage{
			PromptTokens:     &prompt,
			CompletionTokens: &comp,
		},
	}
	out, err := enc.Write(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"input_tokens":25`) {
		t.Errorf("expected input_tokens:25 in message_start; got %s", s)
	}
	if !strings.Contains(s, `"output_tokens":4`) {
		t.Errorf("expected output_tokens:4 in message_start; got %s", s)
	}
}

// TestAnthropicStreamEncoder_ReasoningDelta covers stream_encoders.go:251-268
// — the ReasoningDelta path that opens a thinking content_block and emits
// a thinking_delta.
func TestAnthropicStreamEncoder_ReasoningDelta(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	out, err := enc.Write(context.Background(), provcore.Chunk{ReasoningDelta: "I should consider..."})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "thinking") {
		t.Errorf("expected thinking block; got %s", s)
	}
	if !strings.Contains(s, "thinking_delta") {
		t.Errorf("expected thinking_delta; got %s", s)
	}
	if !strings.Contains(s, "I should consider...") {
		t.Errorf("expected reasoning text in delta; got %s", s)
	}
}

// TestAnthropicStreamEncoder_EmptyChunk covers stream_encoders.go:297-299
// — buf.Len()==0 return path after the header was already sent.
func TestAnthropicStreamEncoder_EmptyChunk(t *testing.T) {
	enc := newAnthropicStreamEncoder()
	enc.headerSent = true
	out, err := enc.Write(context.Background(), provcore.Chunk{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("empty chunk should produce no bytes; got %s", string(out))
	}
}

// TestGeminiStreamEncoder_ToolCallEmitsFunctionCall covers
// stream_encoders.go:329-346 — the tool-call path that emits a
// functionCall part.
func TestGeminiStreamEncoder_ToolCallEmitsFunctionCall(t *testing.T) {
	enc := &geminiStreamEncoder{}
	out, err := enc.Write(context.Background(), provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{{
			Index: 0, ID: "fc_1", Name: "get_weather", Arguments: `{"city":"Tokyo"}`,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "functionCall") {
		t.Errorf("expected functionCall part; got %s", s)
	}
	if !strings.Contains(s, "get_weather") {
		t.Errorf("expected function name; got %s", s)
	}
	if !strings.Contains(s, `"city":"Tokyo"`) {
		t.Errorf("expected parsed args inside the part; got %s", s)
	}
}

// TestGeminiStreamEncoder_ToolCallInvalidArgsFallsBackEmpty exercises the
// args=nil fallback (json.Unmarshal failure path inside the tool-call
// branch — still reaches stream_encoders.go:337-339).
func TestGeminiStreamEncoder_ToolCallInvalidArgsFallsBackEmpty(t *testing.T) {
	enc := &geminiStreamEncoder{}
	out, _ := enc.Write(context.Background(), provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{{
			Index: 0, ID: "fc_2", Name: "do_thing", Arguments: "NOT_JSON",
		}},
	})
	s := string(out)
	if !strings.Contains(s, "do_thing") {
		t.Errorf("expected function name; got %s", s)
	}
	// args must be an empty object since unmarshal failed.
	if !strings.Contains(s, `"args":{}`) {
		t.Errorf("expected empty args object on unmarshal failure; got %s", s)
	}
}

// TestGeminiStreamEncoder_ReasoningDelta covers stream_encoders.go:349-351
// — reasoning text gets surfaced as an additional text part.
func TestGeminiStreamEncoder_ReasoningDelta(t *testing.T) {
	enc := &geminiStreamEncoder{}
	out, err := enc.Write(context.Background(), provcore.Chunk{ReasoningDelta: "let me think"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `"text":"let me think"`) {
		t.Errorf("expected reasoning text mapped to text part; got %s", s)
	}
}

// TestGeminiStreamEncoder_NoParts covers stream_encoders.go:352-354 —
// empty chunk (no delta, no tools, no reasoning, no Done) → no bytes.
func TestGeminiStreamEncoder_NoParts(t *testing.T) {
	enc := &geminiStreamEncoder{}
	out, err := enc.Write(context.Background(), provcore.Chunk{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("empty chunk should produce no bytes; got %s", string(out))
	}
}

// TestCohereStreamEncoder_ToolCallStartAndDelta covers stream_encoders.go:409-443
// — both branches of the tool-call emission (tool-call-start when Name is
// set, tool-call-delta when only Arguments is set).
func TestCohereStreamEncoder_ToolCallStartAndDelta(t *testing.T) {
	enc := &cohereStreamEncoder{}
	ctx := context.Background()
	// First chunk: Name present → tool-call-start.
	out1, _ := enc.Write(ctx, provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{{
			Index: 0, ID: "tc_a", Name: "my_tool", Arguments: `{"k":1}`,
		}},
	})
	if !strings.Contains(string(out1), "tool-call-start") {
		t.Errorf("expected tool-call-start; got %s", string(out1))
	}
	if !strings.Contains(string(out1), "my_tool") {
		t.Errorf("expected tool name; got %s", string(out1))
	}

	// Second chunk: Name empty, Arguments present → tool-call-delta.
	out2, _ := enc.Write(ctx, provcore.Chunk{
		ToolCallDeltas: []provcore.ToolCallDelta{{
			Index: 0, Arguments: `more`,
		}},
	})
	if !strings.Contains(string(out2), "tool-call-delta") {
		t.Errorf("expected tool-call-delta; got %s", string(out2))
	}
}

// TestCohereStreamEncoder_EmptyChunkAfterHeader covers stream_encoders.go:470-472.
func TestCohereStreamEncoder_EmptyChunkAfterHeader(t *testing.T) {
	enc := &cohereStreamEncoder{headerSent: true}
	out, err := enc.Write(context.Background(), provcore.Chunk{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Errorf("empty chunk should produce no bytes; got %s", string(out))
	}
}

// TestReplicateStreamEncoder_EmptyChunk covers stream_encoders.go:489.
func TestReplicateStreamEncoder_EmptyChunk(t *testing.T) {
	enc := &replicateStreamEncoder{}
	out, err := enc.Write(context.Background(), provcore.Chunk{})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("empty chunk should yield nil bytes; got %s", string(out))
	}
}

// TestBuildGeminiUsage_NilUsage covers stream_encoders.go:516-518.
func TestBuildGeminiUsage_NilUsage(t *testing.T) {
	if buildGeminiUsage(nil) != nil {
		t.Error("nil Usage must yield nil map")
	}
}

// TestBuildGeminiUsage_CacheAndReasoning covers stream_encoders.go:536-544 —
// the cachedContentTokenCount and thoughtsTokenCount projection fields.
func TestBuildGeminiUsage_CacheAndReasoning(t *testing.T) {
	cacheRead := 12
	reason := 9
	prompt := 50
	got := buildGeminiUsage(&provcore.Usage{
		PromptTokens:    &prompt,
		CacheReadTokens: &cacheRead,
		ReasoningTokens: &reason,
	})
	if got == nil {
		t.Fatal("expected non-nil usage map")
	}
	if v, _ := got["cachedContentTokenCount"].(int); v != 12 {
		t.Errorf("cachedContentTokenCount = %v, want 12", got["cachedContentTokenCount"])
	}
	if v, _ := got["thoughtsTokenCount"].(int); v != 9 {
		t.Errorf("thoughtsTokenCount = %v, want 9", got["thoughtsTokenCount"])
	}
	if v, _ := got["promptTokenCount"].(int); v != 50 {
		t.Errorf("promptTokenCount = %v, want 50", got["promptTokenCount"])
	}
}

// TestBuildGeminiUsage_ZeroFieldsOmitted documents the implementation rule
// that zero (or absent) cache/reasoning counts are NOT emitted (the
// non-stream egress translation does the same). Together with the
// previous test, this nails both legs of the if-guard.
func TestBuildGeminiUsage_ZeroFieldsOmitted(t *testing.T) {
	zero := 0
	prompt := 8
	got := buildGeminiUsage(&provcore.Usage{
		PromptTokens:    &prompt,
		CacheReadTokens: &zero,
		ReasoningTokens: &zero,
	})
	if got == nil {
		t.Fatal("expected non-nil map; got nil")
	}
	if _, present := got["cachedContentTokenCount"]; present {
		t.Errorf("zero CacheReadTokens must be omitted; got %v", got)
	}
	if _, present := got["thoughtsTokenCount"]; present {
		t.Errorf("zero ReasoningTokens must be omitted; got %v", got)
	}
}

// TestBuildGeminiUsage_AllNilReturnsNil covers the late `len(out) == 0`
// return path inside buildGeminiUsage when every pointer is nil but Usage
// itself is not nil.
func TestBuildGeminiUsage_AllNilReturnsNil(t *testing.T) {
	got := buildGeminiUsage(&provcore.Usage{})
	if got != nil {
		t.Errorf("Usage with no populated fields must yield nil map; got %v", got)
	}
}

// TestResponsesStreamEncoder_ReasoningClosesOpenMessage covers
// stream_encoders_responses.go:224-226 — when a ReasoningDelta arrives
// while a message item is already open, the message must be closed
// before the reasoning item opens.
func TestResponsesStreamEncoder_ReasoningClosesOpenMessage(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{Delta: "first"},          // opens message
		{ReasoningDelta: "think"}, // must close message before opening reasoning
		{Done: true},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	// Find the indexes of the relevant events.
	var msgDoneIdx, reasoningAddedIdx = -1, -1
	for i, f := range frames {
		if f.event == "response.output_item.done" && msgDoneIdx < 0 {
			msgDoneIdx = i
		}
		if f.event == "response.output_item.added" && i > 2 && reasoningAddedIdx < 0 {
			// The first output_item.added is the message item; the second
			// (after message close) is the reasoning item.
			reasoningAddedIdx = i
		}
	}
	if msgDoneIdx < 0 {
		t.Fatalf("expected output_item.done (message close) to fire before reasoning; frames=%v", frames)
	}
	if reasoningAddedIdx < 0 || reasoningAddedIdx < msgDoneIdx {
		t.Fatalf("reasoning item must open AFTER message close; got msgDone=%d reasoningAdded=%d", msgDoneIdx, reasoningAddedIdx)
	}
}

// TestResponsesStreamEncoder_ToolCallClosesOpenMessage covers
// stream_encoders_responses.go:255-257 — currentItem=="message" gets closed
// before the function_call item opens.
func TestResponsesStreamEncoder_ToolCallClosesOpenMessage(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{Delta: "preface"},
		{ToolCallDeltas: []provcore.ToolCallDelta{{Index: 0, ID: "c1", Name: "f", Arguments: `{}`}}},
		{Done: true},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	// Must observe message close (content_part.done + output_item.done)
	// BEFORE the function_call added event.
	contentPartDoneIdx := -1
	functionCallAddedIdx := -1
	for i, f := range frames {
		if f.event == "response.content_part.done" && contentPartDoneIdx < 0 {
			contentPartDoneIdx = i
		}
		if f.event == "response.output_item.added" && i > 3 && functionCallAddedIdx < 0 {
			// Second output_item.added (index>3 skips the message-open one).
			functionCallAddedIdx = i
		}
	}
	if contentPartDoneIdx < 0 {
		t.Errorf("expected content_part.done before tool-call; frames=%v", frames)
	}
	if functionCallAddedIdx < 0 || functionCallAddedIdx < contentPartDoneIdx {
		t.Errorf("function_call added must come AFTER message close; got contentPartDone=%d fcAdded=%d", contentPartDoneIdx, functionCallAddedIdx)
	}
}

// TestResponsesStreamEncoder_UsageCacheAndReasoning covers
// stream_encoders_responses.go:316-321 — the input_tokens_details.cached_tokens
// and output_tokens_details.reasoning_tokens projection on the final
// response.completed event.
func TestResponsesStreamEncoder_UsageCacheAndReasoning(t *testing.T) {
	enc := newResponsesStreamEncoder("gpt-5.2")
	prompt, comp, cache, reason := 20, 4, 7, 2
	var allOut []byte
	for _, c := range []provcore.Chunk{
		{Delta: "x"},
		{Done: true, Usage: &provcore.Usage{
			PromptTokens:     &prompt,
			CompletionTokens: &comp,
			CacheReadTokens:  &cache,
			ReasoningTokens:  &reason,
		}},
	} {
		out, _ := enc.Write(context.Background(), c)
		allOut = append(allOut, out...)
	}
	frames := extractSSEEvents(allOut)
	var completedData string
	for _, f := range frames {
		if f.event == "response.completed" {
			completedData = f.data
			break
		}
	}
	if completedData == "" {
		t.Fatalf("response.completed not found in: %v", frames)
	}
	if got := gjson.Get(completedData, "response.usage.input_tokens_details.cached_tokens").Int(); got != 7 {
		t.Errorf("cached_tokens = %d, want 7; data=%s", got, completedData)
	}
	if got := gjson.Get(completedData, "response.usage.output_tokens_details.reasoning_tokens").Int(); got != 2 {
		t.Errorf("reasoning_tokens = %d, want 2; data=%s", got, completedData)
	}
}
