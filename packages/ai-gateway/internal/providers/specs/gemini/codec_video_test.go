package gemini

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// This wire reads video, so a caller who sends one must reach it.
//
// Measured straight at generativelanguage.googleapis.com with a 7.9 KB mp4 as
// inlineData: gemini-2.5-flash answered 200 and began describing the footage,
// and gemini-3.5-flash answered 200 with the input accounted under
// promptTokensDetails as modality VIDEO — the provider itself billing it as
// video is the strongest statement available that the part was read.
//
// The refusal that used to stand here cited no measurement, in a policy whose
// every other entry carries one and whose stated rule is that refusing what
// works is worse than forwarding what does not.
const tinyVideoDataURL = "data:video/mp4;base64,AAAAIGZ0eXBpc29tAAACAGlzb21pc28yYXZjMW1wNDE="

func geminiVideoBody() []byte {
	return []byte(`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[` +
		`{"type":"video_url","video_url":{"url":"` + tinyVideoDataURL + `"}},` +
		`{"type":"text","text":"what happens here"}]}]}`)
}

func TestGemini_EncodeRequest_CarriesVideoToTheWire(t *testing.T) {
	got, err := NewSpec(nil).SchemaCodec.EncodeRequest(typology.WireShapeGeminiGenerateContent, geminiVideoBody(),
		provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("encode refused a video this wire reads: %v", err)
	}
	var found bool
	gjson.GetBytes(got.Body, "contents.0.parts").ForEach(func(_, p gjson.Result) bool {
		if strings.HasPrefix(p.Get("inlineData.mimeType").String(), "video/") ||
			strings.HasPrefix(p.Get("fileData.mimeType").String(), "video/") {
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("no video part reached the Gemini wire — the caller's video was dropped\n%s", got.Body)
	}
}

// The control, without which "video now works" cannot be told apart from
// "nothing is refused any more": the OpenAI chat wire genuinely has no video
// part, and its refusal is the truth about that wire rather than a gap in ours.
func TestGemini_TheVideoFixIsNotABlanketAcceptance(t *testing.T) {
	pol := contentPolicyFor(provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	if pol.Deny["video_url"] != "" {
		t.Fatalf("gemini still denies video_url: %q", pol.Deny["video_url"])
	}
	if !pol.Allow["video_url"] {
		t.Fatalf("gemini must allow video_url for the part translator to be reached")
	}
}
