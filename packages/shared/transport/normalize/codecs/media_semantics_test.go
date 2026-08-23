package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// Each case here pins a failure mode that was measured or reviewed, not a
// line of code. The shared helpers decide custody for eight codecs, so a
// wrong answer here is a wrong answer everywhere at once.

func TestDecodedSize_IsTheDecodedLength(t *testing.T) {
	// The measured defect reported the base64 length: a 70-byte PNG showed as
	// 96 bytes. Padded and unpadded must both give the decoded number.
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"aGk=", 2},     // "hi"
		{"aGk", 2},      // decodedSize is arithmetic and pads-agnostic
		{"YWJj", 3},     // "abc"
		{"YWJjZA==", 4}, // "abcd"
		{"YWJjZA", 4},   // same, unpadded
		{"YWJjZGU=", 5}, // "abcde"
	} {
		if got := locator.DecodedSize(tc.in); got != tc.want {
			t.Errorf("locator.DecodedSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestValidBase64_MirrorsTheEndpointDecoder(t *testing.T) {
	// The artifact endpoint decodes with StdEncoding. This predicate decides
	// whether a download control is offered, so accepting something the
	// endpoint refuses would ship a button that 422s.
	for _, ok := range []string{"aGk=", "YWJj", "a+/b", "YWJjZA=="} {
		if !locator.ValidBase64(ok) {
			t.Errorf("locator.ValidBase64(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{
		"",         // nothing to serve
		"a",        // remainder 1 is impossible for any encoding
		"ab-c",     // url-safe alphabet, length 4 — only the alphabet can reject it
		"ab_cdXYZ", // ditto, length 8
		"a b ",     // whitespace, length 4 — only the alphabet branch can reject it
		"ab c",     // ditto
		"a===",     // over-padded
		"!!!not-base64!!!",
	} {
		if locator.ValidBase64(bad) {
			t.Errorf("locator.ValidBase64(%q) = true, want false", bad)
		}
	}
}

func TestCapturedMedia_NeverPromisesUnservableBytes(t *testing.T) {
	// Invariant: a download is offered only when the bytes behind it can be
	// served. Each arm below is a way that could be violated.
	t.Run("no locator degrades to absent", func(t *testing.T) {
		got := capturedMedia("image/png", "aGk=", "")
		if got.Source != core.MediaAbsent || got.Locator != "" {
			t.Fatalf("bytes with no address must be absent: %+v", got)
		}
	})
	t.Run("empty payload is absent, not truncated", func(t *testing.T) {
		got := capturedMedia("audio/wav", "", "json:a.b")
		if got.Source != core.MediaAbsent {
			t.Fatalf("a media object carrying no data was never there: %+v", got)
		}
		if got.Locator != "" {
			t.Fatalf("an absent ref must carry no locator: %+v", got)
		}
	})
	t.Run("undecodable payload is truncated with a cause and no size", func(t *testing.T) {
		got := capturedMedia("image/png", "!!!not-base64!!!", "json:a.b")
		if !got.Truncated || got.Cause != "undecodable-base64" {
			t.Fatalf("undecodable bytes must say so, with the specific cause: %+v", got)
		}
		if got.SizeBytes != 0 {
			t.Fatalf("a size for bytes that cannot decode is a fabrication: %+v", got)
		}
	})
	t.Run("good payload reports decoded size and keeps its path", func(t *testing.T) {
		got := capturedMedia("image/png", "YWJjZA==", "json:a.b")
		if got.Source != core.MediaCaptured || got.SizeBytes != 4 || got.Locator != "json:a.b" {
			t.Fatalf("captured ref malformed: %+v", got)
		}
	})
}

func TestInlineOrExternal_ADataURINeverBecomesAURL(t *testing.T) {
	// Storing a whole data URI in a reference field is the headline measured
	// defect. It must not come back through the non-base64 door.
	t.Run("base64 data uri is captured with its declared mime", func(t *testing.T) {
		got := inlineOrExternal("data:image/webp;base64,YWJjZA==", "messages.0.content.0.image_url.url", core.ModalityImage)
		if got.Source != core.MediaCaptured || got.Mime != "image/webp" {
			t.Fatalf("data URI must be captured with its real format: %+v", got)
		}
		if !strings.HasPrefix(got.Locator, "datauri:") {
			t.Fatalf("locator container wrong: %+v", got)
		}
		if got.URL != "" {
			t.Fatalf("a captured ref must never populate URL: %+v", got)
		}
	})
	t.Run("non-base64 data uri is absent, never copied into URL", func(t *testing.T) {
		got := inlineOrExternal(`data:image/svg+xml,<svg/>`, "messages.0.content.0.image_url.url", core.ModalityImage)
		if got.URL != "" {
			t.Fatalf("payload leaked into URL — the measured defect, narrower: %+v", got)
		}
		if got.Source != core.MediaAbsent || got.Mime != "image/svg+xml" {
			t.Fatalf("want absent with the declared mime: %+v", got)
		}
	})
	t.Run("remote url is external and inert", func(t *testing.T) {
		got := inlineOrExternal("https://example.invalid/x.png", "p", core.ModalityImage)
		if got.Source != core.MediaExternal || got.URL != "https://example.invalid/x.png" {
			t.Fatalf("remote reference malformed: %+v", got)
		}
		if got.Locator != "" {
			t.Fatalf("nothing to address — a locator would be a dead control: %+v", got)
		}
	})
	t.Run("empty field is absent", func(t *testing.T) {
		if got := inlineOrExternal("", "p", core.ModalityImage); got.Source != core.MediaAbsent {
			t.Fatalf("want absent: %+v", got)
		}
	})
}

func TestModalityFromMime_AudioAndDocumentsAreNotImages(t *testing.T) {
	// Measured defect: gemini inlineData carrying audio/wav or application/pdf
	// was emitted as an image reference.
	for mime, want := range map[string]string{
		"image/png":            core.ModalityImage,
		"audio/wav":            core.ModalityAudio,
		"audio/L16;rate=24000": core.ModalityAudio,
		"video/mp4":            core.ModalityVideo,
		"application/pdf":      core.ModalityFile,
		"":                     core.ModalityFile,
		"text/markdown":        core.ModalityFile,
	} {
		if got := modalityFromMime(mime); got != want {
			t.Errorf("modalityFromMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestLocatorHelpers_PropagateMissingContext(t *testing.T) {
	// With no positional context a locator would point somewhere arbitrary.
	// The helpers propagate emptiness so capturedMedia can degrade to absent
	// instead of emitting a confidently wrong address.
	if locator.JoinPath("", 3) != "" || locator.JoinSuffix("", "x") != "" {
		t.Fatal("empty base must propagate")
	}
	if locator.JSON("") != "" || locator.DataURI("") != "" || locator.SSE(0, "") != "" {
		t.Fatal("an empty path must not become a path")
	}
	if locator.SSE(-1, "a.b") != "" {
		t.Fatal("a negative frame index is not addressable")
	}
	if got := locator.SSE(2, "candidates.0.content.parts.1.inlineData.data"); got != "sse:2:candidates.0.content.parts.1.inlineData.data" {
		t.Fatalf("sse locator shape: %q", got)
	}
}

func TestAudioMimeFromFormat_MapsTheBareWireWord(t *testing.T) {
	// The chat wire carries a bare format word, not a mime; without the map
	// the modality would fall through to "file" and render the wrong card.
	for in, want := range map[string]string{
		"wav": "audio/wav", "mp3": "audio/mpeg", "flac": "audio/flac",
		"opus": "audio/opus", "": "", "aac": "audio/aac",
	} {
		if got := audioMimeFromFormat(in); got != want {
			t.Errorf("audioMimeFromFormat(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIChat_InputAudioAndFileParts(t *testing.T) {
	// Both used to hit the default branch, which JSON-marshalled the payload
	// into a text block — binary flowing through redaction as if it were prose.
	body := `{"model":"gpt-audio","messages":[{"role":"user","content":[
		{"type":"input_audio","input_audio":{"data":"YWJjZA==","format":"wav"}},
		{"type":"file","file":{"file_data":"YWJjZA==","filename":"n.pdf"}},
		{"type":"file","file":{"file_id":"file-abc"}}]}]}`
	p, err := NewOpenAIChatNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var refs []*core.MediaRef
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "YWJjZA") {
				t.Fatalf("payload reached a text block: %q", b.Text)
			}
			if b.MediaRef != nil {
				refs = append(refs, b.MediaRef)
			}
		}
	}
	if len(refs) != 3 {
		t.Fatalf("want 3 media elements, got %d", len(refs))
	}
	if refs[0].Modality != core.ModalityAudio || refs[0].Mime != "audio/wav" || refs[0].Source != core.MediaCaptured {
		t.Fatalf("input_audio: %+v", refs[0])
	}
	if refs[1].Source != core.MediaCaptured || !strings.HasSuffix(refs[1].Locator, "file.file_data") {
		t.Fatalf("inline file: %+v", refs[1])
	}
	if refs[2].Source != core.MediaProviderRef || refs[2].ProviderRef != "file-abc" {
		t.Fatalf("file id is provider custody: %+v", refs[2])
	}
}

func TestOpenAIResponses_InputFileCustodyPrecedence(t *testing.T) {
	// Highest custody wins when a part carries several references: inline
	// bytes, then a provider id, then a remote URL.
	body := `{"model":"gpt-5","input":[{"role":"user","content":[
		{"type":"input_file","file_data":"YWJjZA==","file_id":"f-1","file_url":"https://x.invalid/a.pdf"},
		{"type":"input_file","file_id":"f-2","file_url":"https://x.invalid/b.pdf"},
		{"type":"input_file","file_url":"https://x.invalid/c.pdf"},
		{"type":"input_image","file_id":"img-1"}]}]}`
	p, err := NewOpenAIResponsesNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var got []string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.MediaRef != nil {
				got = append(got, b.MediaRef.Source)
			}
		}
	}
	want := []string{core.MediaCaptured, core.MediaProviderRef, core.MediaExternal, core.MediaProviderRef}
	if len(got) != len(want) {
		t.Fatalf("want %d refs, got %d (%v)", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ref %d: source %q, want %q", i, got[i], want[i])
		}
	}
}

func TestGeminiUnknownPart_LeavesATrace(t *testing.T) {
	// A part matching no known case used to yield nothing at all, which is
	// how fileData media disappeared without any record it had been sent.
	body := `{"contents":[{"role":"user","parts":[
		{"executableCode":{"language":"PYTHON","code":"print(1)"}},
		{"text":"hi"}]}]}`
	p, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(body), core.Meta{Direction: core.DirectionRequest})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	var marker string
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == core.ContentText && strings.Contains(b.Text, "unrecognised") {
				marker = b.Text
			}
		}
	}
	if marker == "" {
		t.Fatal("an unrecognised part must leave a marker, not vanish")
	}
	// The marker names what was carried, so a reader can tell what was lost.
	if !strings.Contains(marker, "executableCode") {
		t.Fatalf("marker does not name the keys it saw: %q", marker)
	}
}

func TestCohereRequestPart_UnknownTypeStaysInspectable(t *testing.T) {
	got := cohereRequestPart(map[string]any{"type": "future_thing", "x": 1}, "messages.0.content.0")
	if got.Type != core.ContentText || !strings.Contains(got.Text, "future_thing") {
		t.Fatalf("unknown part must stay readable: %+v", got)
	}
	empty := cohereRequestPart(map[string]any{"type": "image_url"}, "p")
	if empty.MediaRef == nil || empty.MediaRef.Source != core.MediaAbsent {
		t.Fatalf("an image part with no url is absent: %+v", empty.MediaRef)
	}
}

func TestAnthropicSourceMedia_UnknownSourceKeepsImageModality(t *testing.T) {
	// media_type is documented only on the base64 variant, so an image with
	// an unrecognised source has no mime to infer from. Guessing "file" would
	// drop the vision requirement capability routing keys off.
	got := anthropicSourceMedia(map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "future_variant"},
	}, "messages.0.content.0", core.ModalityImage)
	if got.Modality != core.ModalityImage || got.Source != core.MediaAbsent {
		t.Fatalf("want an absent image: %+v", got)
	}
}
