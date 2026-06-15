package cursor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming"
)

// gzipFrame gzip-compresses payload (mirroring Cursor's per-frame gzip).
func gzipBytes(payload []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(payload)
	_ = w.Close()
	return buf.Bytes()
}

// connectFrame wraps payload in a Connect-RPC envelope with the given flags.
func connectFrame(flags byte, payload []byte) []byte {
	hdr := make([]byte, 5)
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr, payload...)
}

// buildNested wraps payload in protobuf bytes fields fieldNums (outermost first),
// so the JSON is reachable only by recursive descent — mirroring the real
// /Run frame where the message JSON sits several sub-messages deep.
func buildNested(payload []byte, fieldNums ...int) []byte {
	b := payload
	for i := len(fieldNums) - 1; i >= 0; i-- {
		b = protowire.AppendTag(nil, protowire.Number(fieldNums[i]), protowire.BytesType)
		b = protowire.AppendBytes(b, payload)
		payload = b
	}
	return b
}

func TestExtractCursorAgentFrame_RoleMessagesAndLexical(t *testing.T) {
	a := &Adapter{}
	// Two messages + a Lexical typed-message, each nested under protobuf bytes
	// fields, concatenated in one frame.
	roleUser := []byte(`{"role":"user","content":[{"type":"text","text":"hello agent"}]}`)
	roleAsst := []byte(`{"role":"assistant","content":[{"type":"redacted-reasoning","data":"enc"},{"type":"text","text":"done: hello agent"}]}`)
	lexical := []byte(`{"root":{"children":[{"children":[{"type":"text","text":"echo hello agent"}],"type":"paragraph"}],"type":"root"}}`)
	frame := append(buildNested(roleUser, 4, 3, 8), buildNested(roleAsst, 4, 3, 8)...)
	frame = append(frame, buildNested(lexical, 4, 3, 8)...)

	nc, err := a.ExtractStreamChunk(context.Background(), frame, "/agent.v1.AgentService/Run")
	if err != nil {
		t.Fatalf("ExtractStreamChunk: %v", err)
	}
	want := map[string]bool{
		"[user] hello agent":            false,
		"[assistant] done: hello agent": false, // redacted-reasoning contributes no text
		"[user] echo hello agent":       false, // from the Lexical typed message
	}
	for _, s := range nc.Segments {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for seg, found := range want {
		if !found {
			t.Errorf("missing expected segment %q; got %q", seg, nc.Segments)
		}
	}
}

// TestExtractStreamChunk_AgentVsChatVsOther pins the per-service routing.
func TestExtractStreamChunk_AgentVsChatVsOther(t *testing.T) {
	a := &Adapter{}
	// A foreign protobuf frame (field 1 = "j") must yield nothing on a metrics
	// path, the StreamChatResponse text on a chat path, and (here) nothing on the
	// agent path because it carries no role/Lexical JSON.
	frame := []byte{0x0a, 0x01, 'j'}
	if nc, _ := a.ExtractStreamChunk(context.Background(), frame, "/aiserver.v1.MetricsService/Report"); len(nc.Segments) != 0 {
		t.Errorf("metrics path leaked %q", nc.Segments)
	}
	if nc, _ := a.ExtractStreamChunk(context.Background(), frame, "/aiserver.v1.AiService/StreamChat"); len(nc.Segments) != 1 || nc.Segments[0] != "j" {
		t.Errorf("chat path = %q, want [j]", nc.Segments)
	}
	if nc, _ := a.ExtractStreamChunk(context.Background(), frame, "/agent.v1.AgentService/Run"); len(nc.Segments) != 0 {
		t.Errorf("agent path with no JSON leaked %q", nc.Segments)
	}
}

// TestNormalize_AgentRunResponse_RealWireShape feeds a multi-frame, per-frame
// gzip-compressed Connect-RPC response that mirrors the on-wire shape captured
// from agentn.global.api5.cursor.sh: a streamed snapshot conversation where the
// assistant turn is restreamed every frame (must de-dup), tool calls render as
// readable lines, the tool result is surfaced, the model rides in
// providerOptions, and a 0x02 trailer ends the stream.
func TestNormalize_AgentRunResponse_RealWireShape(t *testing.T) {
	a := &Adapter{}

	asstWithTool := buildNested([]byte(`{"role":"assistant","content":[`+
		`{"type":"redacted-reasoning","data":"x","providerOptions":{"cursor":{"modelName":"composer-2.5-fast"}}},`+
		`{"type":"tool-call","toolCallId":"t1","toolName":"Shell","args":{"command":"echo curosr app BBBB"}}]}`), 4, 3, 8)
	toolResult := buildNested([]byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"t1",`+
		`"result":"Exit code: 0\n\nCommand output:\n\n`+"```"+`\ncurosr app BBBB\n`+"```"+`"}]}`), 4, 3, 8)
	asstFinal := buildNested([]byte(`{"role":"assistant","content":[{"type":"text","text":"**Output:**\n`+
		"```"+`\ncurosr app BBBB\n`+"```"+`"}]}`), 4, 3, 8)

	var body bytes.Buffer
	body.Write(connectFrame(streaming.ConnectFlagCompressed, gzipBytes(asstWithTool)))
	body.Write(connectFrame(streaming.ConnectFlagCompressed, gzipBytes(asstWithTool))) // duplicate snapshot → de-dup
	body.Write(connectFrame(0x00, toolResult))                                         // uncompressed frame
	body.Write(connectFrame(streaming.ConnectFlagCompressed, gzipBytes(asstFinal)))
	body.Write(connectFrame(streaming.ConnectFlagEndStream, nil)) // trailer

	p, err := a.Normalize(context.Background(), body.Bytes(), normalize.Meta{
		AdapterType:  "cursor",
		ContentType:  "application/connect+proto",
		Direction:    normalize.DirectionResponse,
		EndpointPath: "/agent.v1.AgentService/Run",
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != normalize.KindAIChat || p.Protocol != adapterID || p.DetectedSpec != adapterID {
		t.Fatalf("kind/protocol/spec = %s/%s/%s, want ai-chat/cursor/cursor", p.Kind, p.Protocol, p.DetectedSpec)
	}
	if p.Model != "composer-2.5-fast" {
		t.Errorf("model = %q, want composer-2.5-fast", p.Model)
	}
	// De-dup collapses the two identical assistant snapshots to one; the
	// distinct tool-result and final assistant turns remain.
	if len(p.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (deduped); got %+v", len(p.Messages), p.Messages)
	}
	joined := func(i int) string {
		var sb strings.Builder
		for _, c := range p.Messages[i].Content {
			sb.WriteString(c.Text)
		}
		return sb.String()
	}
	if got := joined(0); !strings.Contains(got, "→ Shell: echo curosr app BBBB") {
		t.Errorf("msg0 = %q, want rendered tool-call line", got)
	}
	if string(p.Messages[1].Role) != "tool" || !strings.Contains(joined(1), "curosr app BBBB") {
		t.Errorf("msg1 role/text = %s/%q, want tool result", p.Messages[1].Role, joined(1))
	}
	if got := joined(2); !strings.Contains(got, "**Output:**") || !strings.Contains(got, "curosr app BBBB") {
		t.Errorf("msg2 = %q, want assistant reply", got)
	}
	// Protobuf body is not text — the Raw view must be a BinaryRef.
	if p.HTTP == nil || p.HTTP.BodyView == nil || p.HTTP.BodyView.BinaryRef == nil {
		t.Fatalf("expected BinaryRef bodyView for protobuf body")
	}
	if p.HTTP.BodyView.BinaryRef.Size != int64(body.Len()) {
		t.Errorf("binaryRef size = %d, want %d", p.HTTP.BodyView.BinaryRef.Size, body.Len())
	}
}

// TestNormalize_AgentRunRequest_LexicalUserMessage decodes the typed-message
// (Lexical) request body — the path that previously fell through to
// generic-http.
func TestNormalize_AgentRunRequest_LexicalUserMessage(t *testing.T) {
	a := &Adapter{}
	lexical := buildNested([]byte(`{"root":{"children":[{"children":[`+
		`{"type":"text","text":"echo curosr app BBBB"}],"type":"paragraph"}],"type":"root"}}`), 4, 2)

	body := connectFrame(streaming.ConnectFlagCompressed, gzipBytes(lexical))
	p, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  "cursor",
		ContentType:  "application/connect+proto",
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/agent.v1.AgentService/Run",
	})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if p.Kind != normalize.KindAIChat || p.Protocol != adapterID {
		t.Fatalf("kind/protocol = %s/%s, want ai-chat/cursor", p.Kind, p.Protocol)
	}
	if len(p.Messages) != 1 || string(p.Messages[0].Role) != "user" {
		t.Fatalf("messages = %+v, want one user message", p.Messages)
	}
	var text strings.Builder
	for _, c := range p.Messages[0].Content {
		text.WriteString(c.Text)
	}
	if text.String() != "echo curosr app BBBB" {
		t.Errorf("user text = %q, want 'echo curosr app BBBB'", text.String())
	}
}

// TestNormalize_AgentRunBareFallback exercises the non-framed body path: when a
// captured body is bare protobuf (no Connect envelope) the walker still finds
// the embedded conversation JSON.
func TestNormalize_AgentRunBareFallback(t *testing.T) {
	bare := buildNested([]byte(`{"role":"user","content":[{"type":"text","text":"bare message"}]}`), 4, 3)
	conv, ok := decodeAgentRunBody(bare)
	if !ok {
		t.Fatalf("decodeAgentRunBody(bare) ok=false, want true")
	}
	if len(conv.Contents) != 1 || conv.Contents[0] != "bare message" || conv.Roles[0] != "user" {
		t.Errorf("conv = %+v, want one user 'bare message'", conv)
	}
}

// TestDecodeAgentRunBody_NoConversation returns ok=false so the caller falls
// through to the generic detector.
func TestDecodeAgentRunBody_NoConversation(t *testing.T) {
	// A frame carrying only metadata (no role/Lexical JSON).
	meta := buildNested([]byte(`{"some":"metadata"}`), 1)
	body := connectFrame(0x00, meta)
	if _, ok := decodeAgentRunBody(body); ok {
		t.Errorf("decodeAgentRunBody on metadata-only body ok=true, want false")
	}
}

// TestRenderToolCall covers tool-name + arg-key resolution and fallbacks.
func TestRenderToolCall(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"shell command", `{"type":"tool-call","toolName":"Shell","args":{"command":"ls -la"}}`, "→ Shell: ls -la"},
		{"read path", `{"type":"tool-call","toolName":"Read","input":{"path":"/etc/hosts"}}`, "→ Read: /etc/hosts"},
		{"name fallback", `{"type":"tool-call","name":"Grep","args":{"pattern":"foo"}}`, "→ Grep: foo"},
		{"no args", `{"type":"tool-call","toolName":"Noop"}`, "→ Noop"},
		{"no name", `{"type":"tool-call","args":{"command":"x"}}`, "→ tool: x"},
		{"bad json", `{`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := renderToolCall([]byte(c.raw)); got != c.want {
				t.Errorf("renderToolCall = %q, want %q", got, c.want)
			}
		})
	}
}

// TestToolResultText handles string, object, and empty results.
func TestToolResultText(t *testing.T) {
	if got := toolResultText([]byte(`"plain output"`)); got != "plain output" {
		t.Errorf("string result = %q", got)
	}
	if got := toolResultText([]byte(`{"k":"v"}`)); got != `{"k":"v"}` {
		t.Errorf("object result = %q, want compact JSON", got)
	}
	if got := toolResultText(nil); got != "" {
		t.Errorf("nil result = %q, want empty", got)
	}
}

// TestCompactToolArg truncates oversized argument blobs.
func TestCompactToolArg(t *testing.T) {
	long := strings.Repeat("a", 300)
	got := compactToolArg([]byte(`{"command":"` + long + `"}`))
	if !strings.HasSuffix(got, "…") || len(got) > 170 {
		t.Errorf("compactToolArg did not truncate: len=%d", len(got))
	}
	if got := compactToolArg(nil); got != "" {
		t.Errorf("nil args = %q, want empty", got)
	}
}

// TestParseCursorRoleMessage_Model pulls the model from a content part's
// providerOptions even when the part itself carries no readable text.
func TestParseCursorRoleMessage_Model(t *testing.T) {
	role, text, tools, model := parseCursorRoleMessage(
		`{"role":"assistant","content":[{"type":"redacted-reasoning","data":"x","providerOptions":{"cursor":{"modelName":"composer-x"}}}]}`)
	if role != "assistant" || text != "" || len(tools) != 0 || model != "composer-x" {
		t.Errorf("got role=%q text=%q tools=%d model=%q", role, text, len(tools), model)
	}
	// Empty / role-less inputs return zero values.
	if r, _, _, _ := parseCursorRoleMessage(`{"content":[]}`); r != "" {
		t.Errorf("role-less message returned role=%q, want empty", r)
	}
}
