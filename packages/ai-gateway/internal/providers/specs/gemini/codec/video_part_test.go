// Named failure modes for the video content part:
//   - inline data: URL      → inlineData with the media type it declared
//   - hosted URL            → fileData with the type guessed from the URL
//   - missing url           → named error, not a part the model cannot use
//   - malformed data: URL   → named error, not a silently empty part
package codec_test

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/gemini/codec"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func encodeVideoPart(t *testing.T, videoURL string) ([]byte, error) {
	t.Helper()
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":[` +
		`{"type":"video_url","video_url":{"url":"` + videoURL + `"}},` +
		`{"type":"text","text":"what happens"}]}]}`
	res, err := codec.NewCodec().EncodeRequest(typology.WireShapeGeminiGenerateContent,
		[]byte(body), provcore.CallTarget{ProviderModelID: "gemini-2.5-flash"})
	return res.Body, err
}

// Inline bytes ride the same part shape an inline image rides, carrying the
// media type the caller declared rather than one we guess.
func TestVideoPart_InlineBytesBecomeInlineData(t *testing.T) {
	out, err := encodeVideoPart(t, "data:video/mp4;base64,QQ==")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p := gjson.GetBytes(out, `contents.0.parts.#(inlineData.mimeType=="video/mp4")`)
	if !p.Exists() {
		t.Fatalf("no inline video part on the wire\n%s", out)
	}
	if got := p.Get("inlineData.data").String(); got != "QQ==" {
		t.Fatalf("inline data = %q, want the caller's bytes", got)
	}
}

// A hosted video is a URI the wire fetches, so it rides fileData — the same
// split images and documents already use.
func TestVideoPart_HostedURLBecomesFileData(t *testing.T) {
	out, err := encodeVideoPart(t, "https://example.test/clip.mp4")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	p := gjson.GetBytes(out, `contents.0.parts.#(fileData.fileUri=="https://example.test/clip.mp4")`)
	if !p.Exists() {
		t.Fatalf("no hosted video part on the wire\n%s", out)
	}
	if mt := p.Get("fileData.mimeType").String(); !strings.HasPrefix(mt, "video/") {
		t.Fatalf("fileData.mimeType = %q, want a video type derived from the URL", mt)
	}
}

// A part with no URL is a request for the model to watch nothing. Naming it
// beats forwarding an empty part and letting the model answer about footage it
// never received.
func TestVideoPart_MissingURLIsNamed(t *testing.T) {
	_, err := encodeVideoPart(t, "")
	if err == nil {
		t.Fatal("a video part with no url was forwarded")
	}
	if !strings.Contains(err.Error(), "video_url.url") {
		t.Fatalf("error %q does not name the field that was missing", err.Error())
	}
}

// Same for a data: URL that does not parse — the caller hears which field was
// wrong rather than getting an answer about an empty video.
func TestVideoPart_MalformedDataURLIsNamed(t *testing.T) {
	_, err := encodeVideoPart(t, "data:not-a-valid-data-url")
	if err == nil {
		t.Fatal("a malformed data: URL was forwarded")
	}
	if !strings.Contains(err.Error(), "video_url.url") {
		t.Fatalf("error %q does not name the field that was wrong", err.Error())
	}
}
