package deepseek

import (
	"errors"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// Drives the codec the adapter SHIPS. A test that constructs the gate itself
// proves the gate works and says nothing about whether NewSpec uses it.
func send(t *testing.T, door, body string) error {
	t.Helper()
	spec := NewSpec(nil)
	tgt := provcore.CallTarget{ProviderModelID: "deepseek-chat"}
	if door == "encode" {
		_, err := spec.SchemaCodec.EncodeRequest(typology.WireShapeOpenAIChat, []byte(body), tgt)
		return err
	}
	_, err := spec.SchemaCodec.RewriteNative(typology.WireShapeOpenAIChat, []byte(body), tgt, false)
	return err
}

// DeepSeek's chat body has no multimodal variants, and its deserializer says so
// by naming a Rust enum:
//
//	messages[N]: unknown variant `image_url` … / unknown variant `file` …
//
// That reached callers in production. It names neither their attachment nor
// anything they can do about it.
//
// BOTH DOORS are asserted because an OpenAI-shaped request to this
// OpenAI-compatible provider takes the native leg, which is the one a gate on
// EncodeRequest alone would miss — that exact mistake shipped once already and
// was caught by a production replay rather than by a test.
func TestUnsupportedContent_IsRefusedInOurWordsOnBothDoors(t *testing.T) {
	for _, tc := range []struct{ kind, part, wantIn string }{
		{"image_url", `{"type":"image_url","image_url":{"url":"data:image/png;base64,QQ=="}}`, "no image part"},
		{"file", `{"type":"file","file":{"file_data":"data:text/plain;base64,QQ=="}}`, "no document part"},
		{"input_audio", `{"type":"input_audio","input_audio":{"data":"QQ==","format":"wav"}}`, "no audio part"},
	} {
		for _, door := range []string{"encode", "native"} {
			t.Run(tc.kind+"/"+door, func(t *testing.T) {
				err := send(t, door, `{"model":"deepseek-chat","messages":[{"role":"user","content":[
				    `+tc.part+`,{"type":"text","text":"what is this?"}]}]}`)
				if err == nil {
					t.Fatalf("the %s door forwarded a %s part; DeepSeek answers 400 for it",
						door, tc.kind)
				}
				var pe *provcore.ProviderError
				if !errors.As(err, &pe) || pe.Status != 400 {
					t.Fatalf("err = %v, want a 400 ProviderError", err)
				}
				if !strings.Contains(pe.Message, tc.wantIn) {
					t.Errorf("message %q does not say this wire has no such part", pe.Message)
				}
			})
		}
	}
}

// Text is what this wire does carry, on both doors, and the gate must not touch
// it — including a message whose content is an array of text parts rather than a
// plain string.
func TestText_IsUnaffected(t *testing.T) {
	for _, door := range []string{"encode", "native"} {
		for _, body := range []string{
			`{"model":"deepseek-chat","messages":[{"role":"user","content":"plain text"}]}`,
			`{"model":"deepseek-chat","messages":[{"role":"user","content":[{"type":"text","text":"parts"}]}]}`,
			// A tool_calls turn carries `type` inside the tool call, not a
			// content part — the pre-scan trips on it and the walk must not.
			`{"model":"deepseek-chat","messages":[{"role":"assistant","tool_calls":[
			    {"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`,
		} {
			if err := send(t, door, body); err != nil {
				t.Errorf("%s door refused a text-only request: %v\n  body: %s", door, err, body)
			}
		}
	}
}
