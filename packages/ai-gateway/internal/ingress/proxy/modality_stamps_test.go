// modality_stamps_test.go — the multimodal cost-units stamp and artifact
// fingerprint.
//
// Named failure modes tested:
//   - image/TTS units reach the cost formula (without stampModalityUnits the
//     per-kind formulas price every multimodal request at $0)
//   - b64_json image artifacts are fingerprinted over the DECODED bytes;
//     URL-return images carry the URL reference only (no hash — the gateway
//     never fetches it); TTS fingerprints the binary audio body
//   - malformed b64 artifacts are skipped, never fail the request
//   - non-multimodal kinds stamp nothing
package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/execution/estimator"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/metrics"
)

func TestStampModalityUnits(t *testing.T) {
	t.Run("image count from response data length", func(t *testing.T) {
		var u estimator.BillableUnits
		stampModalityUnits(&u, "image_generation", []byte(`{"model":"dall-e-3","n":5}`),
			[]byte(`{"data":[{"url":"https://x/1"},{"url":"https://x/2"}]}`))
		if u.Images != 2 {
			t.Errorf("Images = %d, want 2 (actual generated count from response, not requested n)", u.Images)
		}
	})

	t.Run("tts chars from forwarded input (runes, not bytes)", func(t *testing.T) {
		var u estimator.BillableUnits
		stampModalityUnits(&u, "tts", []byte(`{"model":"tts-1","input":"héllo 世界"}`), nil)
		if u.InputChars != 8 {
			t.Errorf("InputChars = %d, want 8 (rune count of 'héllo 世界')", u.InputChars)
		}
	})

	t.Run("chat stamps nothing", func(t *testing.T) {
		var u estimator.BillableUnits
		stampModalityUnits(&u, "chat", []byte(`{"input":"x"}`), []byte(`{"data":[{}]}`))
		if u.Images != 0 || u.InputChars != 0 {
			t.Errorf("chat must not stamp modality units: %+v", u)
		}
	})

	t.Run("rerank search units from Cohere response meta", func(t *testing.T) {
		var u estimator.BillableUnits
		stampModalityUnits(&u, "rerank", []byte(`{"query":"q","documents":["a","b"]}`),
			[]byte(`{"results":[{"index":0,"relevance_score":0.9}],"meta":{"billed_units":{"search_units":3}}}`))
		if u.SearchUnits != 3 {
			t.Errorf("SearchUnits = %d, want 3 (from meta.billed_units.search_units)", u.SearchUnits)
		}
	})

	t.Run("rerank without search_units leaves zero (Voyage token path)", func(t *testing.T) {
		var u estimator.BillableUnits
		stampModalityUnits(&u, "rerank", nil,
			[]byte(`{"results":[],"meta":{"billed_units":{"total_tokens":26}}}`))
		if u.SearchUnits != 0 {
			t.Errorf("SearchUnits = %d, want 0 — Voyage reports total_tokens, priced via the token path", u.SearchUnits)
		}
	})

	t.Run("stamped units produce non-zero cost through the registered formula", func(t *testing.T) {
		// The business outcome: an image request prices at the per-image rate.
		var u estimator.BillableUnits
		stampModalityUnits(&u, "image_generation", nil, []byte(`{"data":[{"url":"u"}]}`))
		price := 40000.0 // dall-e-3 standard: $0.04/image
		cost := estimator.Lookup("image_generation")(u, metrics.ModelPrices{InputUsdPerM: &price})
		if want := 0.04; !floatClose(cost.Total, want) {
			t.Errorf("1 image priced at %.6f, want %.6f", cost.Total, want)
		}
	})
}

func TestBuildArtifactRefs(t *testing.T) {
	t.Run("tts fingerprints the audio body", func(t *testing.T) {
		audio := []byte{0xFF, 0xF3, 0x01, 0x02, 0x03} // arbitrary binary
		got := buildArtifactRefs("tts", audio, "audio/mpeg")
		var refs []ArtifactRef
		if err := json.Unmarshal([]byte(got), &refs); err != nil {
			t.Fatalf("unmarshal: %v (raw=%q)", err, got)
		}
		sum := sha256.Sum256(audio)
		if len(refs) != 1 || refs[0].Sha256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("refs = %+v, want one ref with the audio sha256", refs)
		}
		if refs[0].SizeBytes != int64(len(audio)) || refs[0].Mime != "audio/mpeg" {
			t.Errorf("size/mime = %d/%s, want %d/audio/mpeg", refs[0].SizeBytes, refs[0].Mime, len(audio))
		}
	})

	t.Run("b64_json image fingerprints the DECODED artifact", func(t *testing.T) {
		artifact := []byte("fake-png-bytes")
		b64 := base64.StdEncoding.EncodeToString(artifact)
		body := []byte(`{"created":1,"data":[{"b64_json":"` + b64 + `"}]}`)
		got := buildArtifactRefs("image_generation", body, "application/json")
		var refs []ArtifactRef
		if err := json.Unmarshal([]byte(got), &refs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		sum := sha256.Sum256(artifact)
		if len(refs) != 1 || refs[0].Sha256 != hex.EncodeToString(sum[:]) {
			t.Fatalf("fingerprint must cover the decoded artifact, not the JSON envelope; refs=%+v", refs)
		}
		if refs[0].SizeBytes != int64(len(artifact)) {
			t.Errorf("SizeBytes = %d, want decoded length %d", refs[0].SizeBytes, len(artifact))
		}
	})

	t.Run("url-return image stores the reference only (never fetched, no hash)", func(t *testing.T) {
		body := []byte(`{"data":[{"url":"https://oai.example/img1.png"},{"url":"https://oai.example/img2.png"}]}`)
		got := buildArtifactRefs("image_generation", body, "application/json")
		var refs []ArtifactRef
		if err := json.Unmarshal([]byte(got), &refs); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(refs) != 2 {
			t.Fatalf("want 2 URL refs, got %+v", refs)
		}
		for _, r := range refs {
			if r.Sha256 != "" || r.SizeBytes != 0 {
				t.Errorf("URL mode must carry NO content hash (nothing was fetched): %+v", r)
			}
			if r.URL == "" {
				t.Errorf("URL ref missing url: %+v", r)
			}
		}
	})

	t.Run("malformed b64 artifact skipped, request never fails", func(t *testing.T) {
		body := []byte(`{"data":[{"b64_json":"!!!not-base64!!!"}]}`)
		if got := buildArtifactRefs("image_generation", body, "application/json"); got != "" {
			t.Errorf("malformed artifact must yield no ref, got %q", got)
		}
	})

	t.Run("chat produces no refs", func(t *testing.T) {
		if got := buildArtifactRefs("chat", []byte(`{"choices":[]}`), "application/json"); got != "" {
			t.Errorf("chat must not stamp artifact refs, got %q", got)
		}
	})
}

func TestSniffImageMime(t *testing.T) {
	cases := []struct {
		name string
		b    []byte
		want string
	}{
		{"png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, "image/png"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0}, "image/jpeg"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp"},
		{"gif", []byte("GIF89a\x00\x00"), "image/gif"},
		{"unknown short", []byte{0x00, 0x01}, "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sniffImageMime(tc.b); got != tc.want {
				t.Errorf("sniffImageMime = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildArtifactRefs_b64MimeSniffed(t *testing.T) {
	// A gpt-image-1-style webp artifact must NOT be recorded as image/png.
	webp := []byte("RIFF\x00\x00\x00\x00WEBPVP8 payloadbytes")
	b64 := base64.StdEncoding.EncodeToString(webp)
	body := []byte(`{"data":[{"b64_json":"` + b64 + `"}]}`)
	got := buildArtifactRefs("image_generation", body, "application/json")
	var refs []ArtifactRef
	if err := json.Unmarshal([]byte(got), &refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Mime != "image/webp" {
		t.Errorf("mime = %v, want image/webp (sniffed, not hardcoded png)", refs)
	}
}

func TestWarnUnderivableModalityUnits(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		status int
		u      estimator.BillableUnits
		want   bool
	}{
		{"image no units 2xx → warn", "image_generation", 200, estimator.BillableUnits{}, true},
		{"image with count → no warn", "image_generation", 200, estimator.BillableUnits{Images: 1}, false},
		{"image token-priced → no warn", "image_generation", 200, estimator.BillableUnits{PromptTokens: 5}, false},
		{"tts no chars 2xx → warn", "tts", 200, estimator.BillableUnits{}, true},
		{"tts with chars → no warn", "tts", 200, estimator.BillableUnits{InputChars: 10}, false},
		{"non-2xx → no warn", "image_generation", 500, estimator.BillableUnits{}, false},
		{"chat → no warn", "chat", 200, estimator.BillableUnits{}, false},
		{"rerank no units 2xx → warn (R-5 quota-bypass guard)", "rerank", 200, estimator.BillableUnits{}, true},
		{"rerank with search units → no warn", "rerank", 200, estimator.BillableUnits{SearchUnits: 1}, false},
		{"rerank token-priced (Voyage) → no warn", "rerank", 200, estimator.BillableUnits{PromptTokens: 5}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := warnUnderivableModalityUnits(tc.kind, tc.status, tc.u); got != tc.want {
				t.Errorf("warnUnderivableModalityUnits = %v, want %v", got, tc.want)
			}
		})
	}
}

func floatClose(a, b float64) bool {
	d := a - b
	return d < 1e-10 && d > -1e-10
}
