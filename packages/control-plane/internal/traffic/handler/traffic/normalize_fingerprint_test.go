package traffic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/traffic/store/trafficstore"
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// trafficNormalizeInput builds the store row shape computeNormalized reads.
func trafficNormalizeInput(req, resp []byte, refs string) *trafficstore.NormalizeInput {
	return &trafficstore.NormalizeInput{
		AdapterType: "openai", Model: "m", Path: "/v1/x",
		RequestBody: req, ResponseBody: resp,
		RequestContentType: "application/json", ResponseContentType: "application/json",
		ArtifactRefs: refs, Found: true,
	}
}

// STT audio and video input references are hashed, forwarded, and released —
// the audit pool is byte-bounded and these uploads reach tens of megabytes,
// so keeping a copy is not a change that can be made on the request path
// without measuring it. The fingerprint went to the database and was read by
// nothing, so a transcription rendered as a request with no audio in it.
//
// These pin the two halves of the fix: the fingerprint becomes a media
// element like any other, and it never claims bytes it cannot produce.

func decodePayload(t *testing.T, raw json.RawMessage) normcore.NormalizedPayload {
	t.Helper()
	var p normcore.NormalizedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return p
}

func TestFingerprintRequestPayload_AudioBecomesAMediaElement(t *testing.T) {
	raw := fingerprintRequestPayload(`[{"sha256":"` + strings.Repeat("a", 64) + `","sizeBytes":2400000,"mime":"audio/mpeg"}]`)
	if raw == nil {
		t.Fatal("a recorded fingerprint must produce a payload")
	}
	p := decodePayload(t, raw)
	if len(p.Messages) != 1 || len(p.Messages[0].Content) != 1 {
		t.Fatalf("want one media element, got %+v", p.Messages)
	}
	b := p.Messages[0].Content[0]
	if b.Type != normcore.ContentMedia || b.MediaRef == nil {
		t.Fatalf("block is not media: %+v", b)
	}
	ref := b.MediaRef
	if ref.Modality != normcore.ModalityAudio || ref.Mime != "audio/mpeg" || ref.SizeBytes != 2400000 {
		t.Fatalf("the ref must describe what was sent: %+v", ref)
	}
	if ref.SHA256 != strings.Repeat("a", 64) {
		t.Fatalf("the digest is the proof the file was not altered: %+v", ref)
	}
	if ref.Source != normcore.MediaFingerprint {
		t.Fatalf("source = %q, want fingerprint", ref.Source)
	}
	// The half that matters most: no locator, so no control is offered for
	// bytes that were never stored.
	if ref.Locator != "" {
		t.Fatalf("a fingerprint must not carry a locator: %+v", ref)
	}
	if ref.Cause == "" {
		t.Fatalf("a ref with no bytes must say why: %+v", ref)
	}
}

func TestFingerprintRequestPayload_URLEntriesAreExternalNotFingerprint(t *testing.T) {
	raw := fingerprintRequestPayload(`[{"url":"https://r.example/a.mp4","mime":"video/mp4","sizeBytes":10}]`)
	p := decodePayload(t, raw)
	ref := p.Messages[0].Content[0].MediaRef
	// Two different facts: never fetched, versus read and not retained.
	if ref.Source != normcore.MediaExternal || ref.URL != "https://r.example/a.mp4" {
		t.Fatalf("a URL entry names something the gateway never fetched: %+v", ref)
	}
	if ref.Modality != normcore.ModalityVideo {
		t.Fatalf("modality = %q", ref.Modality)
	}
	if ref.Locator != "" {
		t.Fatalf("still no bytes to reach: %+v", ref)
	}
}

func TestFingerprintRequestPayload_ModalityComesFromTheMime(t *testing.T) {
	raw := fingerprintRequestPayload(`[
		{"sha256":"x","mime":"image/png"},
		{"sha256":"x","mime":"audio/wav"},
		{"sha256":"x","mime":"video/webm"},
		{"sha256":"x","mime":"application/pdf"},
		{"sha256":"x","mime":""}
	]`)
	p := decodePayload(t, raw)
	var got []string
	for _, b := range p.Messages[0].Content {
		got = append(got, b.MediaRef.Modality)
	}
	want := []string{"image", "audio", "video", "file", "file"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("modality %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A column that cannot be read must yield no payload rather than an error
// page: one odd row should not make the whole event unopenable.
func TestFingerprintRequestPayload_UnreadableColumnYieldsNothing(t *testing.T) {
	for _, in := range []string{
		"", "not json", `{"not":"an array"}`, `[]`,
		`[{}]`,                    // neither digest nor URL: describes nothing
		`[{"mime":"audio/mpeg"}]`, // same, with a type
		`[null]`,
	} {
		if got := fingerprintRequestPayload(in); got != nil {
			t.Fatalf("input %q produced a payload: %s", in, got)
		}
	}
}

// The synthesis arm reached end to end: an event whose request body was never
// stored, but whose fingerprint was, must normalize into a request payload
// carrying the media element — not into an empty one.
func TestComputeNormalized_SynthesisesTheRequestFromAFingerprint(t *testing.T) {
	h, _ := newHandlerWithMock(t)
	out := h.computeNormalized("evt-stt", trafficNormalizeInput(
		nil, // no request body was stored: the audio was hashed and released
		[]byte(`{"text":"hello"}`),
		`[{"sha256":"`+strings.Repeat("b", 64)+`","sizeBytes":900,"mime":"audio/wav"}]`,
	))
	if out.RequestNormalized == nil {
		t.Fatal("the request carried audio and normalizes to nothing")
	}
	p := decodePayload(t, out.RequestNormalized)
	ref := p.Messages[0].Content[0].MediaRef
	if ref.Modality != normcore.ModalityAudio || ref.Source != normcore.MediaFingerprint {
		t.Fatalf("ref = %+v", ref)
	}
	if out.RequestStatus == nil || *out.RequestStatus != "ok" {
		t.Fatalf("status = %v", out.RequestStatus)
	}
}

// The discriminator, asserted directly: a stored request body means the
// fingerprints describe the RESPONSE, and synthesising from them would put
// the same artifact on screen twice with two different custody states.
func TestComputeNormalized_DoesNotSynthesiseWhenTheRequestWasStored(t *testing.T) {
	h, _ := newHandlerWithMock(t)
	out := h.computeNormalized("evt-img", trafficNormalizeInput(
		[]byte(`{"prompt":"a cat","model":"gpt-image-1"}`),
		[]byte(`{"data":[{"b64_json":"QUJD"}]}`),
		`[{"sha256":"`+strings.Repeat("c", 64)+`","sizeBytes":3,"mime":"image/png"}]`,
	))
	p := decodePayload(t, out.RequestNormalized)
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == normcore.ContentMedia {
				t.Fatalf("the response's artifact was duplicated into the request: %+v", b.MediaRef)
			}
		}
	}
}
