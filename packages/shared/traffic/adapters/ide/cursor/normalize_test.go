package cursor

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// buildConvMsg encodes one ConversationMessage:
//
//	field 1 (string) → text
//	field 2 (varint) → role (1=user, 2=assistant)
func buildConvMsg(text string, role uint64) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendString(out, text)
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	out = protowire.AppendVarint(out, role)
	return out
}

// buildGetChatRequest assembles a GetChatRequest protobuf with the
// provided sequence of (role, text) pairs at field 2 (repeated
// ConversationMessage). Optionally appends a ModelDetails at field 7
// (model_name at sub-field 1).
func buildGetChatRequest(t *testing.T, modelName string, msgs []struct {
	role string
	text string
}) []byte {
	t.Helper()
	var out []byte
	for _, m := range msgs {
		var roleEnum uint64 = 1
		if m.role == "assistant" {
			roleEnum = 2
		}
		msgBytes := buildConvMsg(m.text, roleEnum)
		out = protowire.AppendTag(out, 2, protowire.BytesType)
		out = protowire.AppendBytes(out, msgBytes)
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

// buildConnectFrame wraps a payload in a Connect-RPC envelope:
//
//	byte 0:    flags (0x00 normal, 0x01 end-of-stream)
//	bytes 1-4: big-endian uint32 payload length
//	bytes 5+:  payload
func buildConnectFrame(payload []byte, endOfStream bool) []byte {
	hdr := make([]byte, 5)
	if endOfStream {
		hdr[0] = 0x01
	}
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	return append(hdr, payload...)
}

// buildStreamChatResponseFrame builds one StreamChatResponse payload
// carrying a text delta at field 1, wrapped in a Connect-RPC envelope.
func buildStreamChatResponseFrame(text string, endOfStream bool) []byte {
	var p []byte
	p = protowire.AppendTag(p, 1, protowire.BytesType)
	p = protowire.AppendString(p, text)
	return buildConnectFrame(p, endOfStream)
}

func TestCursorNormalize_Request_GetChatRequest(t *testing.T) {
	body := buildGetChatRequest(t, "claude-sonnet-4-6", []struct {
		role string
		text string
	}{
		{"user", "explain Connect-RPC in 1 sentence"},
		{"assistant", "Connect-RPC is a Buf protocol that wraps protobuf in HTTP/1.1- and HTTP/2-friendly frames."},
		{"user", "and JSON-Patch?"},
	})

	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType:  adapterID,
		Direction:    normalize.DirectionRequest,
		EndpointPath: "/aiserver.v1.AiService/StreamUnifiedChatWithTools",
	})
	if err != nil {
		t.Fatalf("Normalize err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("Kind: %v want ai-chat", payload.Kind)
	}
	if payload.DetectedSpec != "cursor" {
		t.Errorf("DetectedSpec: %q want cursor", payload.DetectedSpec)
	}
	if payload.Model != "claude-sonnet-4-6" {
		t.Errorf("Model: %q want claude-sonnet-4-6", payload.Model)
	}
	if len(payload.Messages) != 3 {
		t.Fatalf("messages: %d want 3 (%+v)", len(payload.Messages), payload.Messages)
	}
	if payload.Messages[0].Role != normalize.RoleUser ||
		payload.Messages[1].Role != normalize.RoleAssistant ||
		payload.Messages[2].Role != normalize.RoleUser {
		t.Errorf("roles: %v / %v / %v", payload.Messages[0].Role, payload.Messages[1].Role, payload.Messages[2].Role)
	}
	if !strings.Contains(payload.Messages[1].Content[0].Text, "Connect-RPC is a Buf protocol") {
		t.Errorf("assistant content: %q", payload.Messages[1].Content[0].Text)
	}
	if payload.Confidence < 0.85 {
		t.Errorf("confidence: %v want >= 0.85 (multi-message + model)", payload.Confidence)
	}
	// Dual view: raw bytes referenced via BinaryRef (protobuf is not
	// human-readable as text, so we keep size + content-type only).
	if payload.HTTP == nil || payload.HTTP.BodyView == nil || payload.HTTP.BodyView.BinaryRef == nil {
		t.Fatalf("BodyView.BinaryRef not populated: %+v", payload.HTTP)
	}
	if payload.HTTP.BodyView.BinaryRef.Size != int64(len(body)) {
		t.Errorf("BinaryRef.Size: %d want %d", payload.HTTP.BodyView.BinaryRef.Size, len(body))
	}
}

func TestCursorNormalize_Request_NoMessages_FallsThrough(t *testing.T) {
	// A protobuf that doesn't carry any conversation field (e.g. a
	// settings ping) should ErrUnsupported so the Coordinator falls
	// through to Tier 2 / Tier 3.
	var body []byte
	body = protowire.AppendTag(body, 100, protowire.VarintType)
	body = protowire.AppendVarint(body, 42)

	a := &Adapter{}
	_, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for non-chat protobuf")
	}
}

func TestCursorNormalize_Response_ConnectRPCStream(t *testing.T) {
	// Three Connect-RPC frames, each carrying a text delta. Final
	// frame has the end-of-stream flag set.
	body := buildStreamChatResponseFrame("Sure! ", false)
	body = append(body, buildStreamChatResponseFrame("Cursor talks ", false)...)
	body = append(body, buildStreamChatResponseFrame("Connect-RPC.", true)...)

	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "cursor" {
		t.Errorf("spec: %q", payload.DetectedSpec)
	}
	if !payload.Stream {
		t.Errorf("Stream flag should be true for response")
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Role != normalize.RoleAssistant {
		t.Fatalf("messages: %+v", payload.Messages)
	}
	got := payload.Messages[0].Content[0].Text
	want := "Sure! Cursor talks Connect-RPC."
	if got != want {
		t.Fatalf("assistant text: %q\nwant: %q", got, want)
	}
	if payload.Confidence < 0.9 {
		t.Errorf("confidence: %v want >= 0.9 (3 frames)", payload.Confidence)
	}
}

func TestCursorNormalize_Response_BareProtobufFallback(t *testing.T) {
	// Body is a raw StreamChatResponse payload with NO envelope wrapper
	// (some captures land this way when the relay strips the envelope).
	// Adapter must still extract field 1.
	var body []byte
	body = protowire.AppendTag(body, 1, protowire.BytesType)
	body = protowire.AppendString(body, "raw protobuf delta")

	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionResponse,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Messages[0].Content[0].Text != "raw protobuf delta" {
		t.Fatalf("text: %q", payload.Messages[0].Content[0].Text)
	}
}

func TestCursorNormalize_JSON_DelegatesToExtract(t *testing.T) {
	// Cursor's relay paths sometimes carry OpenAI-shape JSON. Normalize
	// must delegate to extract.NormalizeForAdapter when the body sniffs
	// as JSON.
	body := []byte(`{
		"model": "gpt-4o-mini",
		"messages": [{"role": "user", "content": "hi via cursor JSON relay"}]
	}`)
	a := &Adapter{}
	payload, err := a.Normalize(context.Background(), body, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
		ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if payload.Kind != normalize.KindAIChat {
		t.Fatalf("kind: %v", payload.Kind)
	}
	if payload.DetectedSpec != "cursor" {
		t.Errorf("spec: %q want cursor", payload.DetectedSpec)
	}
	if payload.Model != "gpt-4o-mini" {
		t.Errorf("model: %q", payload.Model)
	}
}

func TestCursorNormalize_EmptyBody(t *testing.T) {
	a := &Adapter{}
	_, err := a.Normalize(context.Background(), nil, normalize.Meta{
		AdapterType: adapterID,
		Direction:   normalize.DirectionRequest,
	})
	if err == nil {
		t.Fatal("expected ErrUnsupported for empty body")
	}
}
