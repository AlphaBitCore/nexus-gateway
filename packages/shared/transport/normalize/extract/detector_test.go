package extract

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// Fixture builders

// buildConvMsgWire returns a ConversationMessage protobuf:
//
//	field 1 (string) = text
//	field 2 (varint) = role (1=user, 2=assistant)
func buildConvMsgWire(text string, role uint64) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendString(out, text)
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	out = protowire.AppendVarint(out, role)
	return out
}

// buildGetChatReqWire returns a bare-protobuf GetChatRequest body
// matching the shape the cursor adapter speaks.
func buildGetChatReqWire(messages []struct {
	role string
	text string
}, modelName string) []byte {
	var out []byte
	for _, m := range messages {
		var roleEnum uint64 = 1
		if m.role == "assistant" {
			roleEnum = 2
		}
		msg := buildConvMsgWire(m.text, roleEnum)
		out = protowire.AppendTag(out, 2, protowire.BytesType)
		out = protowire.AppendBytes(out, msg)
	}
	if modelName != "" {
		var md []byte
		md = protowire.AppendTag(md, 1, protowire.BytesType)
		md = protowire.AppendString(md, modelName)
		out = protowire.AppendTag(out, 7, protowire.BytesType)
		out = protowire.AppendBytes(out, md)
	}
	return out
}

// buildConnectFrame wraps a payload in a Connect-RPC envelope. The end-of-stream
// (trailer) frame is flagged 0x02 per the Connect protocol — 0x01 is the
// per-message compression flag, a distinct bit.
func buildConnectFrame(payload []byte, eos bool) []byte {
	hdr := make([]byte, 5)
	if eos {
		hdr[0] = 0x02
	}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr, payload...)
}

// buildStreamChatRespFrame wraps a StreamChatResponse text delta
// in a Connect-RPC envelope frame.
func buildStreamChatRespFrame(text string, eos bool) []byte {
	var p []byte
	p = protowire.AppendTag(p, 1, protowire.BytesType)
	p = protowire.AppendString(p, text)
	return buildConnectFrame(p, eos)
}

// buildBatchReqWire builds a gemini-web batchexecute request body.
func buildBatchReqWire(prompt string) []byte {
	innerArr := []any{
		[]any{prompt, 0, nil, nil, nil, nil, 0},
		[]any{"en"},
	}
	innerJSON, _ := json.Marshal(innerArr)
	outer := []any{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)
	form := url.Values{}
	form.Set("f.req", string(outerJSON))
	form.Set("at", "csrf-token")
	return []byte(form.Encode())
}

// buildBatchRespChunk builds one wrb.fr chunk of a batchexecute response.
func buildBatchRespChunk(text, model string) []byte {
	cand := []any{"rc_abc", []any{text}, nil, nil, nil, nil, nil, nil, []any{1}}
	for range 22 {
		cand = append(cand, nil)
	}
	cand = append(cand, model)
	inner := []any{
		nil,
		[]any{"c_conv", "r_resp"},
		nil, nil,
		[]any{cand},
	}
	innerJSON, _ := json.Marshal(inner)
	outer := [][]any{
		{"wrb.fr", nil, string(innerJSON)},
	}
	outerJSON, _ := json.Marshal(outer)
	return []byte(fmt.Sprintf("%d\n%s\n", len(outerJSON), string(outerJSON)))
}

// Direct detector tests

func TestConnectRPCProtobufDetector_RecognizesGetChatRequest(t *testing.T) {
	body := buildGetChatReqWire([]struct {
		role string
		text string
	}{
		{"user", "explain Tier 2"},
		{"assistant", "Tier 2 recognises format shapes generically."},
	}, "claude-haiku-4-5")

	d := ConnectRPCProtobufDetector{}
	if !d.LooksLike(body) {
		t.Fatal("LooksLike returned false on a real GetChatRequest body")
	}
	det, ok := d.Decode(body, "request")
	if !ok {
		t.Fatal("Decode returned false")
	}
	if det.SpecID != "protobuf-connectrpc-chat" {
		t.Errorf("SpecID: %q", det.SpecID)
	}
	if det.Model != "claude-haiku-4-5" {
		t.Errorf("Model: %q", det.Model)
	}
	if len(det.MessageContents) != 2 ||
		!strings.Contains(det.MessageContents[0], "explain Tier 2") ||
		!strings.Contains(det.MessageContents[1], "Tier 2 recognises") {
		t.Errorf("messages: %v", det.MessageContents)
	}
	if det.Confidence < 0.75 {
		t.Errorf("confidence: %v", det.Confidence)
	}
}

func TestConnectRPCProtobufDetector_RecognizesStreamResponse(t *testing.T) {
	body := buildStreamChatRespFrame("Sure! ", false)
	body = append(body, buildStreamChatRespFrame("Cursor speaks ", false)...)
	body = append(body, buildStreamChatRespFrame("Connect-RPC.", true)...)

	d := ConnectRPCProtobufDetector{}
	if !d.LooksLike(body) {
		t.Fatal("LooksLike returned false")
	}
	det, ok := d.Decode(body, "response")
	if !ok {
		t.Fatal("Decode false")
	}
	if det.AssistantText != "Sure! Cursor speaks Connect-RPC." {
		t.Fatalf("text: %q", det.AssistantText)
	}
	if det.Confidence < 0.85 {
		t.Errorf("confidence: %v", det.Confidence)
	}
}

func TestBatchExecuteDetector_RecognizesRequest(t *testing.T) {
	body := buildBatchReqWire("great do do do do do")
	d := BatchExecuteDetector{}
	if !d.LooksLike(body) {
		t.Fatal("LooksLike returned false")
	}
	det, ok := d.Decode(body, "request")
	if !ok {
		t.Fatal("Decode false")
	}
	if det.UserPrompts[0] != "great do do do do do" {
		t.Errorf("prompt: %q", det.UserPrompts[0])
	}
	if det.Confidence < 0.8 {
		t.Errorf("confidence: %v", det.Confidence)
	}
}

func TestBatchExecuteDetector_RecognizesResponse(t *testing.T) {
	c1 := buildBatchRespChunk("Haha", "3 Flash")
	c2 := buildBatchRespChunk("Haha, sounds good", "3 Flash")
	c3 := buildBatchRespChunk("Haha, sounds good — let's get to work!", "3 Flash")
	body := append([]byte(")]}'\n\n"), c1...)
	body = append(body, c2...)
	body = append(body, c3...)

	d := BatchExecuteDetector{}
	if !d.LooksLike(body) {
		t.Fatal("LooksLike returned false")
	}
	det, ok := d.Decode(body, "response")
	if !ok {
		t.Fatal("Decode false")
	}
	if !strings.Contains(det.AssistantText, "let's get to work") {
		t.Errorf("final text: %q", det.AssistantText)
	}
	if det.Model != "3 Flash" {
		t.Errorf("model: %q", det.Model)
	}
}

// TestBatchExecuteDetector_HandlesOffByOneChunkLengths locks in the
// d64abfd9 prod-traffic regression: gemini.google.com sometimes
// declares a chunk length that's 1–2 bytes more than the actual JSON
// content (it appears to count the leading newline after `)]}'` into
// chunk 1). A length-based loop would read past the chunk boundary,
// fail json.Unmarshal, and bail before reaching subsequent frames.
// The json.Decoder streaming approach ignores the length headers and
// reads one JSON value at a time, so off-by-one length lines do not
// break the parse.
func TestBatchExecuteDetector_HandlesOffByOneChunkLengths(t *testing.T) {
	c1 := buildBatchRespChunk("Did your", "")
	// Synthesize an off-by-one length prefix on chunk 1 (declare 2
	// bytes MORE than the chunk actually contains).
	nl := bytes.IndexByte(c1, '\n')
	if nl < 0 {
		t.Fatalf("test fixture broken")
	}
	chunkBody := c1[nl+1:]
	wrong := []byte(fmt.Sprintf("%d\n", len(chunkBody)+2))
	c1 = append(wrong, chunkBody...) //nolint:gocritic // intentionally rebuild c1 with a corrupted length prefix

	c2 := buildBatchRespChunk("Did your keyboard just get stuck, or are we inventing a new secret code?", "3 Flash")
	body := append([]byte(")]}'\n\n"), c1...)
	body = append(body, c2...)

	d := BatchExecuteDetector{}
	det, ok := d.Decode(body, "response")
	if !ok {
		t.Fatal("Decode false — off-by-one length broke the parser (d64abfd9 regression)")
	}
	if !strings.Contains(det.AssistantText, "new secret code") {
		t.Errorf("text: %q (chunk 2 was not reached)", det.AssistantText)
	}
	if det.Model != "3 Flash" {
		t.Errorf("model: %q", det.Model)
	}
}

// TestBatchExecuteDetector_FlatShapeFinalDelta locks in the second
// half of the d64abfd9 fix: the final-delta chunk on a real prod
// response flattens its metadata into a top-level inner array,
// placing the model name at arr[42] and the assistant text inside a
// nested [[[[null,[null,0,"<text>"]]]]] structure at arr[26]. The
// well-known arr[4][0][1][0] candidate-wrapper path is empty in this
// shape — Path B (scanForLongestText + scanForModelString on the
// flat array) is what makes extraction work.
func TestBatchExecuteDetector_FlatShapeFinalDelta(t *testing.T) {
	const finalText = "Did your keyboard just get stuck, or are we inventing a new secret code?"
	// Flat shape: arr[0..46] with metadata at fixed offsets observed
	// in prod traffic_event d64abfd9. The well-known arr[4][0][1][0]
	// candidate-wrapper path is intentionally empty (arr[4] = null)
	// so only Path B can pull the values out.
	flat := make([]any, 47)
	flat[4] = nil
	flat[26] = [][][][][]any{{{{nil, {nil, 0.0, finalText}}}}}
	flat[42] = "3 Flash"
	innerJSON, _ := json.Marshal(flat)
	outer := [][]any{
		{"wrb.fr", nil, string(innerJSON)},
	}
	outerJSON, _ := json.Marshal(outer)
	chunk := []byte(fmt.Sprintf("%d\n%s\n", len(outerJSON), string(outerJSON)))
	body := append([]byte(")]}'\n\n"), chunk...)

	d := BatchExecuteDetector{}
	det, ok := d.Decode(body, "response")
	if !ok {
		t.Fatal("Decode false — flat-shape fallback missing (d64abfd9 regression)")
	}
	if !strings.Contains(det.AssistantText, "new secret code") {
		t.Errorf("text: %q (Path B deep-scan failed)", det.AssistantText)
	}
	if det.Model != "3 Flash" {
		t.Errorf("model: %q (top-level scanForModelString failed)", det.Model)
	}
}

func TestDetector_LooksLike_RejectsJSONBodies(t *testing.T) {
	jsonBody := []byte(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`)
	if (ConnectRPCProtobufDetector{}).LooksLike(jsonBody) {
		t.Error("protobuf detector falsely claimed a JSON body")
	}
	if (BatchExecuteDetector{}).LooksLike(jsonBody) {
		t.Error("batchexecute detector falsely claimed a JSON body")
	}
}

// Integration: PatternNormalizer claims Tier 2 via detectors

func TestPatternNormalizer_Tier2_ClaimsProtobufWithoutAdapter(t *testing.T) {
	// Body is a real-shape protobuf chat request. No host adapter is
	// registered in this test — PatternNormalizer alone should claim
	// via the ConnectRPCProtobufDetector.
	body := buildGetChatReqWire([]struct {
		role string
		text string
	}{
		{"user", "Tier 2 works for unknown hosts too"},
	}, "claude-opus-4-7")

	pn := NewPatternNormalizer()
	pn.MinConfidence = 0.7
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "unknown-cursor-clone",
		Direction:   normalize.DirectionRequest,
	})
	if err != nil {
		t.Fatalf("err: %v (Tier 2 detector should have claimed)", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("Kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "pattern:protobuf-connectrpc-chat" {
		t.Errorf("DetectedSpec: %q", payload.DetectedSpec)
	}
	if payload.Model != "claude-opus-4-7" {
		t.Errorf("model: %q", payload.Model)
	}
	if len(payload.Messages) != 1 || !strings.Contains(payload.Messages[0].Content[0].Text, "Tier 2 works") {
		t.Fatalf("messages: %+v", payload.Messages)
	}
}

func TestPatternNormalizer_Tier2_ClaimsBatchExecuteWithoutAdapter(t *testing.T) {
	body := buildBatchReqWire("Hello from an unknown Google AI host")
	pn := NewPatternNormalizer()
	pn.MinConfidence = 0.7
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "unknown-google-host",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/x-www-form-urlencoded",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "pattern:google-batchexecute-chat" {
		t.Errorf("DetectedSpec: %q", payload.DetectedSpec)
	}
	if !strings.Contains(payload.Messages[0].Content[0].Text, "unknown Google AI host") {
		t.Errorf("prompt: %q", payload.Messages[0].Content[0].Text)
	}
}

func TestPatternNormalizer_Tier2_JSONStillClaims(t *testing.T) {
	// Existing JSON-shape probe behaviour must not regress when
	// detectors are added — the consumer-web chatgpt-web spec still
	// claims its shape through the JSON probe pass.
	body := []byte(`{
		"model": "gpt-5-5",
		"messages": [{"author": {"role": "user"}, "content": {"parts": ["still a JSON chat"]}}],
		"suggestion_type": "autocomplete"
	}`)
	pn := NewPatternNormalizer()
	pn.MinConfidence = 0.7
	payload, err := pn.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "unknown",
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.DetectedSpec != "pattern:chatgpt-web" {
		t.Errorf("DetectedSpec: %q want pattern:chatgpt-web", payload.DetectedSpec)
	}
}

func TestPatternNormalizer_Tier2_GibberishFallsThrough(t *testing.T) {
	// Random bytes that aren't JSON, protobuf, or batchexecute: every
	// detector LooksLike returns false → Coordinator falls to Tier 3.
	body := bytes.Repeat([]byte{0xff, 0xfe, 0xfa}, 50)
	pn := NewPatternNormalizer()
	pn.MinConfidence = 0.7
	_, err := pn.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: "whatever",
		Direction:   normalize.DirectionRequest,
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for gibberish")
	}
}
