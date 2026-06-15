package cursor

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// extractGetChatRequest + parseConversationMessage (binary protobuf path
// reached via ExtractRequest when the body sniffs as non-JSON AND the
// path matches a Connect-RPC chat endpoint).

// buildConvMsgRaw mirrors the encoder used in normalize_test.go but lives
// here so the in-package coverage tests do not depend on test-only helpers
// from the sibling file at link-time. role==0 ⇒ omit role field entirely
// (exercises the "default user" fallback in parseConversationMessage).
func buildConvMsgRaw(text string, role uint64) []byte {
	var out []byte
	if text != "" {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendString(out, text)
	}
	if role != 0 {
		out = protowire.AppendTag(out, 2, protowire.VarintType)
		out = protowire.AppendVarint(out, role)
	}
	return out
}

// buildGetChatRequestRaw assembles a GetChatRequest. extra=true appends
// an unknown high-numbered field at the top level so the default branch
// of extractGetChatRequest (ConsumeFieldValue path) runs.
func buildGetChatRequestRaw(msgs [][]byte, conversationID string, extra bool) []byte {
	var out []byte
	for _, m := range msgs {
		out = protowire.AppendTag(out, 2, protowire.BytesType)
		out = protowire.AppendBytes(out, m)
	}
	if conversationID != "" {
		out = protowire.AppendTag(out, 15, protowire.BytesType)
		out = protowire.AppendString(out, conversationID)
	}
	if extra {
		// field 100 varint — unknown, must be skipped via ConsumeFieldValue.
		out = protowire.AppendTag(out, 100, protowire.VarintType)
		out = protowire.AppendVarint(out, 42)
	}
	return out
}

func TestExtractRequest_BinaryProtobuf_ChatPath_HappyPath(t *testing.T) {
	body := buildGetChatRequestRaw([][]byte{
		buildConvMsgRaw("hello cursor", 1),                    // user
		buildConvMsgRaw("hi there", 2),                        // assistant
		buildConvMsgRaw("what about an unknown role?", 99),    // role=unknown
		buildConvMsgRaw("legacy entry without role field", 0), // default fallback
	}, "conv-abc", true)

	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamUnifiedChatWithTools")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 4 {
		t.Fatalf("Segments=%d want 4: %#v", len(nc.Segments), nc.Segments)
	}
	want := []string{
		"[user] hello cursor",
		"[assistant] hi there",
		"[unknown] what about an unknown role?",
		"[user] legacy entry without role field",
	}
	for i, w := range want {
		if nc.Segments[i] != w {
			t.Errorf("Segments[%d]=%q want %q", i, nc.Segments[i], w)
		}
	}
	if nc.Metadata["conversation_id"] != "conv-abc" {
		t.Errorf("conversation_id=%q", nc.Metadata["conversation_id"])
	}
}

func TestExtractRequest_BinaryProtobuf_ChatPath_NoMessages(t *testing.T) {
	// protobuf-shape body but with no conversation field — adapter must
	// fall through to ErrUnknownSchema and stamp binary_preview.
	body := buildGetChatRequestRaw(nil, "conv-only", true)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("missing binary_preview")
	}
}

func TestExtractRequest_BinaryProtobuf_NonChatPath(t *testing.T) {
	// Non-JSON body on a non-chat path: adapter returns ErrUnknownSchema
	// + binary_preview without trying to parse as GetChatRequest.
	body := []byte{0xde, 0xad, 0xbe, 0xef}
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/random-binary")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("missing binary_preview")
	}
}

func TestExtractRequest_BinaryProtobuf_MalformedTag(t *testing.T) {
	// Single garbage byte: protowire.ConsumeTag returns n<0 immediately.
	body := []byte{0xff}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema (no segments)", err)
	}
}

func TestExtractRequest_BinaryProtobuf_TruncatedBytes(t *testing.T) {
	// tag(field 2, BytesType) followed by length 99 but no payload —
	// triggers the n<0 path inside the field-2 branch.
	body := []byte{
		byte(2<<3) | byte(protowire.BytesType), // tag for field 2
		99,                                     // length = 99
		'a', 'b',                               // only 2 bytes available
	}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_BinaryProtobuf_TruncatedConversationID(t *testing.T) {
	// tag(field 15, BytesType) followed by length 99 but no payload —
	// triggers the n<0 path inside the field-15 branch.
	body := []byte{
		byte(15<<3) | byte(protowire.BytesType), // tag for field 15
		99,                                      // length = 99
		'a',                                     // only 1 byte available
	}
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_BinaryProtobuf_DefaultBranchMalformed(t *testing.T) {
	// Field 100 BytesType with a bad length — exercises the default
	// branch's ConsumeFieldValue n<0 fail-fast.
	body := protowire.AppendTag(nil, 100, protowire.BytesType)
	body = append(body, 99) // claimed length 99
	body = append(body, 'a')
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

// parseConversationMessage edge cases reached via field-2 body containing
// malformed inner field tags.

func TestExtractRequest_ConversationMessage_TruncatedText(t *testing.T) {
	// Inner conv-msg: field 1 BytesType length 99 but truncated payload.
	innerBad := []byte{
		byte(1<<3) | byte(protowire.BytesType),
		99,
		'a',
	}
	body := buildGetChatRequestRaw([][]byte{innerBad}, "", false)

	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	// Even though parseConversationMessage gracefully bails, the outer
	// adapter has no segments → ErrUnknownSchema.
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

func TestExtractRequest_ConversationMessage_TruncatedRoleVarint(t *testing.T) {
	// field 1 with valid text, field 2 varint with no payload (truncated).
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, "good text")
	inner = append(inner, byte(2<<3)|byte(protowire.VarintType)) // tag only, no varint bytes

	body := buildGetChatRequestRaw([][]byte{inner}, "", false)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || !strings.Contains(nc.Segments[0], "good text") {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ConversationMessage_GarbageTag(t *testing.T) {
	// Single 0xff in the inner ConversationMessage triggers
	// ConsumeTag n<0 immediately. text stays empty → outer skips append.
	body := buildGetChatRequestRaw([][]byte{{0xff}}, "conv-only", false)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	// Garbage inner + no good messages → no segments, but conversation_id
	// is set → still ErrUnknownSchema because we gate on segments.
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
	if _, ok := nc.Extra["binary_preview"]; !ok {
		t.Errorf("missing binary_preview")
	}
}

func TestExtractRequest_ConversationMessage_UnknownInnerField(t *testing.T) {
	// Inner ConversationMessage with field 1 text + an unknown field 50.
	// Exercises parseConversationMessage's default branch + ConsumeFieldValue.
	var inner []byte
	inner = protowire.AppendTag(inner, 1, protowire.BytesType)
	inner = protowire.AppendString(inner, "ok")
	inner = protowire.AppendTag(inner, 50, protowire.VarintType)
	inner = protowire.AppendVarint(inner, 7)

	body := buildGetChatRequestRaw([][]byte{inner}, "", false)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "[user] ok" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractRequest_ConversationMessage_UnknownInnerFieldMalformed(t *testing.T) {
	// Inner: field 50 BytesType with bad length — exercises default
	// branch's n<0 break. protowire encodes (50<<3)|2 = 402 as 2-byte varint.
	inner := protowire.AppendTag(nil, 50, protowire.BytesType)
	inner = append(inner, 99) // claimed length 99
	inner = append(inner, 'x')
	body := buildGetChatRequestRaw([][]byte{inner}, "", false)
	a := &Adapter{}
	_, err := a.ExtractRequest(context.Background(), body, "/aiserver.v1.AiService/StreamChat")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

// parseStreamChatResponseText — reached via ExtractStreamChunk on non-JSON
// chunks.

func TestExtractStreamChunk_ProtobufFrameWithText(t *testing.T) {
	var chunk []byte
	chunk = protowire.AppendTag(chunk, 1, protowire.BytesType)
	chunk = protowire.AppendString(chunk, "stream delta")

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "stream delta" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ProtobufFrameSkipsUnknownThenReadsText(t *testing.T) {
	// Field 22 (server_bubble_id) BytesType first, then field 1 (text).
	var chunk []byte
	chunk = protowire.AppendTag(chunk, 22, protowire.BytesType)
	chunk = protowire.AppendString(chunk, "bubble-1")
	chunk = protowire.AppendTag(chunk, 1, protowire.BytesType)
	chunk = protowire.AppendString(chunk, "after-bubble text")

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "after-bubble text" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_ProtobufFrameMalformedTag(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte{0xff}, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v want nil (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_ProtobufFrameNoTextField(t *testing.T) {
	// Only field 22 — no field 1, parser returns "".
	var chunk []byte
	chunk = protowire.AppendTag(chunk, 22, protowire.BytesType)
	chunk = protowire.AppendString(chunk, "only-bubble")

	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_ProtobufFrameTextTruncated(t *testing.T) {
	// field 1 BytesType with bad length — ConsumeBytes n<0, fn returns "".
	chunk := []byte{
		byte(1<<3) | byte(protowire.BytesType),
		99,
		'x',
	}
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_ProtobufFrameSkipMalformedField(t *testing.T) {
	// field 22 BytesType with bad length — ConsumeFieldValue n<0, loop exits.
	chunk := protowire.AppendTag(nil, 22, protowire.BytesType)
	chunk = append(chunk, 99) // claimed length 99
	chunk = append(chunk, 'x')
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/aiserver.v1.AiService/StreamChat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v want empty", nc.Segments)
	}
}

func TestExtractStreamChunk_MalformedJSON(t *testing.T) {
	// JSON sniff true but ValidBytes false — fail-open, no error.
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), []byte(`{bad`), "/api/stream")
	if err != nil {
		t.Fatalf("err=%v want nil (fail-open)", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractStreamChunk_PlainContent(t *testing.T) {
	// "content" alongside "text" — exercises the `content` branch.
	chunk := []byte(`{"text":"a","content":"b"}`)
	a := &Adapter{}
	nc, err := a.ExtractStreamChunk(context.Background(), chunk, "/api/stream")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "a" || nc.Segments[1] != "b" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// ExtractResponse extra branches.

func TestExtractResponse_EmptyBody(t *testing.T) {
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), nil, "/api/x")
	if err != nil {
		t.Fatalf("err=%v want nil", err)
	}
	if len(nc.Segments) != 0 {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

func TestExtractResponse_MalformedJSON(t *testing.T) {
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), []byte(`{bad`), "/api/x")
	if !errors.Is(err, traffic.ErrMalformed) {
		t.Fatalf("err=%v want ErrMalformed", err)
	}
}

func TestExtractResponse_TopLevelMessageError(t *testing.T) {
	body := []byte(`{"message":"rate limited"}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 1 || nc.Segments[0] != "rate limited" {
		t.Errorf("Segments=%v", nc.Segments)
	}
	if nc.Metadata["error"] != "true" {
		t.Errorf("error meta missing")
	}
}

func TestExtractResponse_ChoicesWithToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"content":"calling","tool_calls":[
		{"id":"c1","type":"function","function":{"name":"do","arguments":"{}"}}
	]}}]}`)
	a := &Adapter{}
	nc, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.ToolCallSegments) != 1 || !strings.Contains(nc.ToolCallSegments[0], `"do"`) {
		t.Errorf("ToolCallSegments=%v", nc.ToolCallSegments)
	}
}

func TestExtractResponse_JSONNoKnownFields(t *testing.T) {
	body := []byte(`{"foo":"bar"}`)
	a := &Adapter{}
	_, err := a.ExtractResponse(context.Background(), body, "/api/x")
	if !errors.Is(err, traffic.ErrUnknownSchema) {
		t.Fatalf("err=%v want ErrUnknownSchema", err)
	}
}

// ExtractRequest extra branches.

func TestExtractRequest_OpenAICompatContentArray(t *testing.T) {
	// messages[].content is an array of parts with type=text — exercises
	// the `content.IsArray()` branch.
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"text","text":"part one"},
			{"type":"image","image":{"url":"x"}},
			{"type":"text","text":"part two"}
		]}
	]}`)
	a := &Adapter{}
	nc, err := a.ExtractRequest(context.Background(), body, "/api/chat")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(nc.Segments) != 2 || nc.Segments[0] != "part one" || nc.Segments[1] != "part two" {
		t.Errorf("Segments=%v", nc.Segments)
	}
}

// rpcPathToModel — uncovered branches.

func TestRpcPathToModel_AllBranches(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/aiserver.v1.AiService/StreamComposer", "cursor-composer"},
		{"/aiserver.v1.AiService/StreamChat", "cursor-chat"},
		{"/aiserver.v1.AiService/WarmChatCache", "cursor-warmchatcache"},
		{"/StreamUnusedMethod", "cursor-streamunusedmethod"},
		{"no-slash", "cursor"},
	}
	for _, c := range cases {
		got := rpcPathToModel(c.path)
		if got != c.want {
			t.Errorf("rpcPathToModel(%q)=%q want %q", c.path, got, c.want)
		}
	}
}

func TestDetectRequestMeta_StreamComposerPath(t *testing.T) {
	a := &Adapter{}
	r, _ := http.NewRequest(http.MethodPost, "https://api2.cursor.sh/aiserver.v1.AiService/StreamComposer", nil)
	meta := a.DetectRequestMeta(r, []byte{0xff}) // not JSON → no body model
	if meta.Model != "cursor-composer" {
		t.Errorf("Model=%q want cursor-composer", meta.Model)
	}
}

// preview — high-byte branch.

func TestPreview_StripsHighBytes(t *testing.T) {
	// Non-ASCII bytes (>0x7e) must be replaced with '.'.
	body := []byte{'a', 0xff, 'b', 0x80, 'c'}
	p := preview(body)
	if p != "a.b.c" {
		t.Errorf("preview=%q want a.b.c", p)
	}
}

func TestPreview_PreservesTabsAndNewlines(t *testing.T) {
	body := []byte{'a', '\t', 'b', '\n', 'c'}
	p := preview(body)
	if p != "a\tb\nc" {
		t.Errorf("preview=%q want a\\tb\\nc", p)
	}
}
