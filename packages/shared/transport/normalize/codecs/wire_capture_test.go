package codecs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// These tests run against bodies captured from the real provider APIs, not
// hand-written fixtures. Several codec decisions in this area were first made
// from documentation and turned out to disagree with the wire — the Gemini
// Files-API URI scheme and the Responses audio content type both did — so the
// shapes are pinned here against what the providers actually sent.
//
// Captures live in ../testdata/wire. Payload bytes are trimmed; every
// structural key is intact, which is what these assertions are about.

func wireFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "wire", name))
	if err != nil {
		t.Fatalf("read wire capture %s: %v", name, err)
	}
	return b
}

func firstMedia(t *testing.T, p core.NormalizedPayload) *core.MediaRef {
	t.Helper()
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentMedia && b.MediaRef != nil {
				return b.MediaRef
			}
		}
	}
	t.Fatalf("no media block in payload: %+v", p.Messages)
	return nil
}

// A Gemini Files-API URI is https, exactly like a public video link, so a
// scheme-based rule labels provider-held media "never fetched or stored by
// Nexus" — inverted custody on Google's own mechanism for large media. The
// URI below is the one the Files API actually returned.
func TestGeminiFileDataCustody_RealFilesAPIURI(t *testing.T) {
	const filesAPIURI = "https://generativelanguage.googleapis.com/v1beta/files/rf9ysxbpu442"

	got := geminiFileDataMedia("image/png", filesAPIURI)
	if got.Source != core.MediaProviderRef {
		t.Fatalf("Files-API URI must be provider-held, got source=%q", got.Source)
	}
	if got.ProviderRef != filesAPIURI {
		t.Fatalf("provider ref not carried: %+v", got)
	}
	if got.URL != "" {
		t.Fatalf("a provider-held ref must not populate URL: %+v", got)
	}

	// A third-party URL is the genuinely external case.
	ext := geminiFileDataMedia("video/mp4", "https://www.youtube.com/watch?v=aaaaaaaaaaa")
	if ext.Source != core.MediaExternal || ext.URL == "" {
		t.Fatalf("third-party URL must be external with the URL carried: %+v", ext)
	}
	if ext.ProviderRef != "" {
		t.Fatalf("an external ref must not populate ProviderRef: %+v", ext)
	}
}

// The audio object on the chat wire carries {data, expires_at, id,
// transcript}. Before this decode existed the whole modality vanished and an
// audio turn read as an empty assistant message.
func TestOpenAIChat_RealAudioOutResponse(t *testing.T) {
	raw := wireFixture(t, "audio_gpt-audio.json")
	n := NewOpenAIChatNormalizer()
	p, err := n.Normalize(context.Background(), raw, core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}

	ref := firstMedia(t, p)
	if ref.Modality != core.ModalityAudio {
		t.Fatalf("audio output must be modality audio, got %q", ref.Modality)
	}
	if ref.Source != core.MediaCaptured || ref.Locator == "" {
		t.Fatalf("inline audio bytes are captured and addressable: %+v", ref)
	}
	if !strings.HasSuffix(ref.Locator, ".message.audio.data") {
		t.Fatalf("locator does not address the audio field: %q", ref.Locator)
	}
	if ref.SizeBytes <= 0 {
		t.Fatalf("decoded size must be reported: %+v", ref)
	}

	// The spoken words are also text, and must stay scannable as text.
	var sawTranscript bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && b.Text != "" {
				sawTranscript = true
			}
		}
	}
	if !sawTranscript {
		t.Fatal("the audio transcript must survive as a text block for compliance scanning")
	}

	// audio_tokens rides in completion_tokens_details on the real wire and is
	// what makes an audio turn's cost explicable.
	if p.Usage == nil || p.Usage.AudioTokens == nil || *p.Usage.AudioTokens <= 0 {
		t.Fatalf("audio tokens must be carried: %+v", p.Usage)
	}
}

// Cohere v2 accepts array content with image parts. The codec previously
// decoded content as a string only, so an image-bearing request degraded to
// one raw-JSON text block and the image was lost.
func TestCohereChat_RealArrayContentRequest(t *testing.T) {
	body := `{"model":"command-a-vision-07-2025","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what colour"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}}]}]}`

	n := NewCohereChatNormalizer()
	p, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Mime != "image/png" {
		t.Fatalf("the data-URI prefix declares the real format: %+v", ref)
	}
	if ref.Modality != core.ModalityImage || ref.Source != core.MediaCaptured {
		t.Fatalf("inline image is a captured image: %+v", ref)
	}
	if ref.Locator != "datauri:messages.0.content.1.image_url.url" {
		t.Fatalf("locator does not address the image field exactly: %q", ref.Locator)
	}
	// The sibling text part must survive alongside it.
	var sawText bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "what colour") {
				sawText = true
			}
		}
	}
	if !sawText {
		t.Fatal("array-content text parts must survive as text blocks")
	}
}

// An Anthropic PDF arrives as a document block with a base64 source. It used
// to fall through to the JSON-marshal branch, which put the whole document
// into a text block — binary flowing through the compliance pipeline as prose.
func TestAnthropicMessages_RealPDFDocumentRequest(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":32,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}},` +
		`{"type":"text","text":"one word: topic?"}]}]}`

	n := NewAnthropicMessagesNormalizer()
	p, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	ref := firstMedia(t, p)
	if ref.Mime != "application/pdf" || ref.Modality != core.ModalityFile {
		t.Fatalf("a PDF is a file with its declared mime: %+v", ref)
	}
	if ref.Source != core.MediaCaptured || !strings.HasSuffix(ref.Locator, ".source.data") {
		t.Fatalf("document bytes must be addressable: %+v", ref)
	}
	// No base64 run may reach a text block — that was the defect's signature.
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "JVBERi0x") {
				t.Fatalf("document payload leaked into a text block: %q", b.Text)
			}
		}
	}
}

// Anthropic's text-bearing document variants are not media. Routing them into
// a media ref drops the text, and because compliance hooks scan text-shaped
// blocks only, dropping it hides the content from scanning entirely.
func TestAnthropicMessages_TextDocumentStaysScannable(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"card 4111 1111 1111 1111"}}]}]}`

	n := NewAnthropicMessagesNormalizer()
	p, err := n.Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var found bool
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "4111") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("a text document source must remain a text block, or no compliance hook ever sees it")
	}
}
