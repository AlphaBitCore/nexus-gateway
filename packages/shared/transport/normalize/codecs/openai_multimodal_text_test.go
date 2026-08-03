package codecs

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// tinyPNG is a 1x1 PNG; its base64 is used to prove the image response codec
// summarizes artifacts by size/mime and NEVER inlines the base64 payload.
var tinyPNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func reqMeta(path string) core.Meta {
	return core.Meta{AdapterType: "openai", Direction: core.DirectionRequest, ContentType: "application/json", EndpointPath: path}
}
func respMeta(path, ct string) core.Meta {
	return core.Meta{AdapterType: "openai", Direction: core.DirectionResponse, ContentType: ct, EndpointPath: path}
}

func firstText(p core.NormalizedPayload) string {
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText {
				return b.Text
			}
		}
	}
	return ""
}

// ── images ────────────────────────────────────────────────────────────────

func TestOpenAIImages_RequestPrompt(t *testing.T) {
	n := NewOpenAIImagesNormalizer()
	t.Run("string prompt → user text", func(t *testing.T) {
		p, err := n.Normalize(context.Background(),
			[]byte(`{"model":"dall-e-3","prompt":"a red fox","n":1,"size":"1024x1024"}`), reqMeta("/v1/images/generations"))
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if p.Kind != core.KindAIImage {
			t.Errorf("kind=%s want ai-image", p.Kind)
		}
		if got := firstText(p); got != "a red fox" {
			t.Errorf("text=%q want the prompt", got)
		}
		if p.Messages[0].Role != core.RoleUser {
			t.Errorf("role=%s want user", p.Messages[0].Role)
		}
	})
	t.Run("array prompt → every element", func(t *testing.T) {
		p, err := n.Normalize(context.Background(),
			[]byte(`{"prompt":["alice@example.com","second"]}`), reqMeta("/v1/images/generations"))
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if n := len(p.Messages[0].Content); n != 2 {
			t.Fatalf("blocks=%d want 2", n)
		}
		if p.Messages[0].Content[0].Text != "alice@example.com" {
			t.Errorf("first=%q", p.Messages[0].Content[0].Text)
		}
	})
	t.Run("missing prompt → ErrUnsupported", func(t *testing.T) {
		_, err := n.Normalize(context.Background(), []byte(`{"model":"dall-e-3","n":1}`), reqMeta("/v1/images/generations"))
		if err == nil {
			t.Fatal("want ErrUnsupported")
		}
	})
}

func TestOpenAIImages_ResponseSummary_NoInlineB64(t *testing.T) {
	n := NewOpenAIImagesNormalizer()
	b64 := base64.StdEncoding.EncodeToString(tinyPNG)
	body := `{"created":1,"data":[{"b64_json":"` + b64 + `","revised_prompt":"a vivid red fox"}]}`
	p, err := n.Normalize(context.Background(), []byte(body), respMeta("/v1/images/generations", "application/json"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if p.Kind != core.KindAIImage {
		t.Fatalf("kind=%s", p.Kind)
	}
	var sawRevised, sawRef bool
	for _, b := range p.Messages[0].Content {
		if b.Type == core.ContentText {
			if b.Text != "a vivid red fox" {
				t.Errorf("revised=%q", b.Text)
			}
			sawRevised = true
			if strings.Contains(b.Text, b64) {
				t.Fatal("base64 leaked into a text block")
			}
		}
		if b.Type == core.ContentImageRef {
			sawRef = true
			if b.ImageRef.ContentType != "image/png" {
				t.Errorf("mime=%s want image/png", b.ImageRef.ContentType)
			}
			if b.ImageRef.Size != int64(len(tinyPNG)) {
				t.Errorf("size=%d want %d", b.ImageRef.Size, len(tinyPNG))
			}
		}
	}
	if !sawRevised || !sawRef {
		t.Fatalf("revised=%v ref=%v", sawRevised, sawRef)
	}
	// The whole marshaled payload must not carry the base64 blob.
	if strings.Contains(mustJSON(t, p), b64) {
		t.Fatal("base64 present in the marshaled normalized payload")
	}
}

func TestOpenAIImages_ResponseURL_InertText(t *testing.T) {
	n := NewOpenAIImagesNormalizer()
	p, err := n.Normalize(context.Background(),
		[]byte(`{"data":[{"url":"https://cdn.example/img.png"}]}`), respMeta("/v1/images/generations", "application/json"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := firstText(p)
	if !strings.Contains(got, "https://cdn.example/img.png") {
		t.Errorf("url text=%q", got)
	}
	// url mode must NOT emit an image_ref (BinaryRef has no url field).
	for _, b := range p.Messages[0].Content {
		if b.Type == core.ContentImageRef {
			t.Fatal("url mode wrongly emitted an image_ref")
		}
	}
}

// ── tts ───────────────────────────────────────────────────────────────────

func TestOpenAIAudioSpeech_Request(t *testing.T) {
	n := NewOpenAIAudioSpeechNormalizer()
	p, err := n.Normalize(context.Background(),
		[]byte(`{"model":"tts-1","input":"read this","instructions":"cheerful","voice":"alloy"}`), reqMeta("/v1/audio/speech"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if p.Kind != core.KindAITTS {
		t.Fatalf("kind=%s want ai-tts", p.Kind)
	}
	if n := len(p.Messages[0].Content); n != 2 {
		t.Fatalf("blocks=%d want input+instructions", n)
	}
	if p.Messages[0].Content[0].Text != "read this" || p.Messages[0].Content[1].Text != "cheerful" {
		t.Errorf("texts=%v", p.Messages[0].Content)
	}
}

func TestOpenAIAudioSpeech_ResponseBinary_TerminalClaim(t *testing.T) {
	n := NewOpenAIAudioSpeechNormalizer()
	audio := []byte{0xff, 0xfb, 0x90, 0x00, 0x01, 0x02, 0x03} // mp3-ish bytes
	p, err := n.Normalize(context.Background(), audio, respMeta("/v1/audio/speech", "audio/mpeg"))
	if err != nil {
		t.Fatalf("response must claim terminally, got err=%v", err)
	}
	if p.Kind != core.KindHTTPBinary {
		t.Fatalf("kind=%s want http-binary", p.Kind)
	}
	if p.HTTP == nil || p.HTTP.BodyView == nil || p.HTTP.BodyView.BinaryRef == nil {
		t.Fatal("missing binary ref")
	}
	if p.HTTP.BodyView.BinaryRef.ContentType != "audio/mpeg" {
		t.Errorf("mime=%s", p.HTTP.BodyView.BinaryRef.ContentType)
	}
	if p.HTTP.BodyView.BinaryRef.Size != int64(len(audio)) {
		t.Errorf("size=%d", p.HTTP.BodyView.BinaryRef.Size)
	}
}

// ── stt ───────────────────────────────────────────────────────────────────

func TestOpenAIAudioTranscriptions_Response(t *testing.T) {
	n := NewOpenAIAudioTranscriptionsNormalizer()
	t.Run("json text", func(t *testing.T) {
		p, err := n.Normalize(context.Background(), []byte(`{"text":"hello world"}`), respMeta("/v1/audio/transcriptions", "application/json"))
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if p.Kind != core.KindAISTT {
			t.Fatalf("kind=%s", p.Kind)
		}
		if got := firstText(p); got != "hello world" {
			t.Errorf("transcript=%q", got)
		}
		if p.Messages[0].Role != core.RoleAssistant {
			t.Errorf("role=%s want assistant", p.Messages[0].Role)
		}
	})
	t.Run("verbose_json segments joined", func(t *testing.T) {
		p, _ := n.Normalize(context.Background(),
			[]byte(`{"segments":[{"text":"foo "},{"text":"bar"}],"duration":2.0}`), respMeta("/v1/audio/transcriptions", "application/json"))
		if got := firstText(p); got != "foo bar" {
			t.Errorf("joined=%q", got)
		}
	})
	t.Run("text/plain verbatim", func(t *testing.T) {
		p, _ := n.Normalize(context.Background(), []byte("just a transcript\n"), respMeta("/v1/audio/transcriptions", "text/plain"))
		if got := firstText(p); got != "just a transcript" {
			t.Errorf("plain=%q", got)
		}
	})
}

func TestOpenAIAudioTranscriptions_RequestMultipart_TerminalClaim(t *testing.T) {
	n := NewOpenAIAudioTranscriptionsNormalizer()
	p, err := n.Normalize(context.Background(), []byte("--BOUNDARY\r\nblah\r\n--BOUNDARY--"),
		core.Meta{Direction: core.DirectionRequest, ContentType: "multipart/form-data", EndpointPath: "/v1/audio/transcriptions"})
	if err != nil {
		t.Fatalf("request must claim terminally, got err=%v", err)
	}
	if p.Kind != core.KindHTTPMultipart {
		t.Fatalf("kind=%s want http-multipart", p.Kind)
	}
}

// ── error/edge arms + sniffing ──────────────────────────────────────────────

func TestMultimodalCodecs_ErrorArms(t *testing.T) {
	img := NewOpenAIImagesNormalizer()
	tts := NewOpenAIAudioSpeechNormalizer()
	stt := NewOpenAIAudioTranscriptionsNormalizer()

	// IDs (metric labels).
	if img.ID() != "openai-images" || tts.ID() != "openai-audio-speech" || stt.ID() != "openai-audio-transcriptions" {
		t.Fatalf("ids: %s %s %s", img.ID(), tts.ID(), stt.ID())
	}

	// Empty body → ErrUnsupported on every codec/direction.
	for _, c := range []core.Normalizer{img, tts, stt} {
		if _, err := c.Normalize(context.Background(), nil, reqMeta("/x")); err == nil {
			t.Errorf("%s: empty body must error", c.(interface{ ID() string }).ID())
		}
	}

	// Unsupported direction (neither request nor response).
	badDir := core.Meta{Direction: core.Direction("sideways"), ContentType: "application/json"}
	if _, err := img.Normalize(context.Background(), []byte(`{"prompt":"x"}`), badDir); err == nil {
		t.Error("images: bad direction must error")
	}
	if _, err := tts.Normalize(context.Background(), []byte(`{"input":"x"}`), badDir); err == nil {
		t.Error("tts: bad direction must error")
	}

	// Malformed JSON request.
	if _, err := img.Normalize(context.Background(), []byte(`{not json`), reqMeta("/v1/images/generations")); err == nil {
		t.Error("images: malformed json must error")
	}
	if _, err := tts.Normalize(context.Background(), []byte(`{not json`), reqMeta("/v1/audio/speech")); err == nil {
		t.Error("tts: malformed json must error")
	}
	// TTS request with no user text at all.
	if _, err := tts.Normalize(context.Background(), []byte(`{"model":"tts-1","voice":"alloy"}`), reqMeta("/v1/audio/speech")); err == nil {
		t.Error("tts: no user text must error")
	}
	// Image response missing data.
	if _, err := img.Normalize(context.Background(), []byte(`{"created":1}`), respMeta("/v1/images/generations", "application/json")); err == nil {
		t.Error("images: response without data must error")
	}
	// STT response that is JSON with no recognized transcript field over-scans:
	// the raw body is projected so nothing hides from the scanner (no error).
	if p, err := stt.Normalize(context.Background(), []byte(`{"duration":1.0}`), respMeta("/v1/audio/transcriptions", "application/json")); err != nil || firstText(p) == "" {
		t.Errorf("stt no-transcript over-scan: err=%v text=%q", err, firstText(p))
	}
}

func TestMultimodalCodecs_EdgeText(t *testing.T) {
	img := NewOpenAIImagesNormalizer()

	// Empty-string prompt: decodes to no text → no messages, no error.
	if p, err := img.Normalize(context.Background(), []byte(`{"prompt":""}`), reqMeta("/v1/images/generations")); err != nil || len(p.Messages) != 0 {
		t.Errorf("empty prompt: err=%v msgs=%d", err, len(p.Messages))
	}
	// Array prompt with empty elements: empties dropped.
	p, err := img.Normalize(context.Background(), []byte(`{"prompt":["keep","",""]}`), reqMeta("/v1/images/generations"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(p.Messages[0].Content) != 1 || p.Messages[0].Content[0].Text != "keep" {
		t.Errorf("empties not dropped: %v", p.Messages[0].Content)
	}
	// Object-valued prompt: unrepresentable → ErrUnsupported.
	if _, err := img.Normalize(context.Background(), []byte(`{"prompt":{"nested":1}}`), reqMeta("/v1/images/generations")); err == nil {
		t.Error("object prompt must error")
	}
	// Invalid base64 artifact: size falls back, mime stays octet-stream, no panic.
	resp := `{"data":[{"b64_json":"!!!not-base64!!!"}]}`
	pr, err := img.Normalize(context.Background(), []byte(resp), respMeta("/v1/images/generations", "application/json"))
	if err != nil {
		t.Fatalf("invalid-b64 err=%v", err)
	}
	var ref *core.BinaryRef
	for _, b := range pr.Messages[0].Content {
		if b.Type == core.ContentImageRef {
			ref = b.ImageRef
		}
	}
	if ref == nil || ref.ContentType != "application/octet-stream" {
		t.Errorf("invalid-b64 ref=%v", ref)
	}
	// Invalid base64 → the arithmetic size is a fiction; report 0, not a fake count.
	if ref != nil && ref.Size != 0 {
		t.Errorf("invalid-b64 size=%d want 0 (no fabricated byte count)", ref.Size)
	}
}

func TestSTT_UnknownResponseShape_StaysScannable(t *testing.T) {
	n := NewOpenAIAudioTranscriptionsNormalizer()
	// A non-conforming provider returns JSON without text/segments. The content
	// must still project (over-scan) rather than vanish — a silent DLP gap on
	// the interception path.
	p, err := n.Normalize(context.Background(), []byte(`{"result":"a secret phrase"}`),
		respMeta("/v1/audio/transcriptions", "application/json"))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	got := firstText(p)
	if !strings.Contains(got, "a secret phrase") {
		t.Errorf("unknown STT shape must stay scannable, got %q", got)
	}
}

func TestSniffImageMagic(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00"), "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBP"), "image/webp"},
		{"gif", []byte("GIF89a\x00"), "image/gif"},
		{"http-sniffable html", []byte("<!DOCTYPE html><html>"), "text/html; charset=utf-8"},
		{"unknown", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07}, "application/octet-stream"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sniffImageMagic(c.b); got != c.want {
				t.Errorf("sniff=%s want %s", got, c.want)
			}
		})
	}
}

func mustJSON(t *testing.T, p core.NormalizedPayload) string {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
