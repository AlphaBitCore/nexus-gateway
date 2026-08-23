package codecs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/locator"
)

// A minimal but real RIFF/WAVE header, so the fixture is the kind of thing the
// capture path actually hands over rather than an arbitrary blob.
func wavBytes(n int) []byte {
	b := make([]byte, n)
	copy(b, []byte("RIFF"))
	copy(b[8:], []byte("WAVEfmt "))
	return b
}

// The defect this pins, measured on production: a transcription whose audio
// WAS captured normalized to source "fingerprint" with no locator, so the
// drawer said "bytes not retained" over bytes sitting in the stored payload.
// Custody must describe what is actually there.
func TestSTTRequest_CapturedAudioIsReportedAsCapturedAndResolvable(t *testing.T) {
	audio := wavBytes(163662)
	n := NewOpenAIAudioTranscriptionsNormalizer()

	got, err := n.Normalize(context.Background(), audio, core.Meta{
		Direction:   core.DirectionRequest,
		ContentType: "audio/wav",
	})
	if err != nil {
		t.Fatalf("Normalize returned %v", err)
	}
	ref := got.HTTP.BodyView.MediaRef
	if ref.Source != core.MediaCaptured {
		t.Errorf("source = %q, want %q — the bytes were handed over", ref.Source, core.MediaCaptured)
	}
	if ref.Locator != locator.Body {
		t.Errorf("locator = %q, want %q — captured bytes must be reachable", ref.Locator, locator.Body)
	}
	if ref.Modality != core.ModalityAudio {
		t.Errorf("modality = %q, want audio", ref.Modality)
	}
	if ref.Mime != "audio/wav" {
		t.Errorf("mime = %q, want the type the capture path supplied", ref.Mime)
	}
	if ref.SizeBytes != int64(len(audio)) {
		t.Errorf("sizeBytes = %d, want %d — the size must be the bytes a fetch returns", ref.SizeBytes, len(audio))
	}
	sum := sha256.Sum256(audio)
	if ref.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 does not digest the captured bytes")
	}
	if got.Kind != core.KindHTTPBinary {
		t.Errorf("kind = %q; captured audio is a binary body, the same shape the speech response uses", got.Kind)
	}
}

// The other half must not regress: when only the stripped envelope survives,
// claiming a locator would offer a download nothing can serve.
func TestSTTRequest_StrippedEnvelopeStaysFingerprintWithNoLocator(t *testing.T) {
	n := NewOpenAIAudioTranscriptionsNormalizer()
	for _, ct := range []string{
		"multipart/form-data",
		"multipart/form-data; boundary=----abc",
		"MULTIPART/FORM-DATA",
		"", // no content type recorded at all
	} {
		got, err := n.Normalize(context.Background(), []byte("--boundary--\r\n"), core.Meta{
			Direction:   core.DirectionRequest,
			ContentType: ct,
		})
		if err != nil {
			t.Fatalf("ContentType %q: Normalize returned %v", ct, err)
		}
		ref := got.HTTP.BodyView.MediaRef
		if ref.Source != core.MediaFingerprint {
			t.Errorf("ContentType %q: source = %q, want fingerprint", ct, ref.Source)
		}
		if ref.Locator != "" {
			t.Errorf("ContentType %q: locator = %q, want empty — nothing could serve it", ct, ref.Locator)
		}
		if got.Kind != core.KindHTTPMultipart {
			t.Errorf("ContentType %q: kind = %q, want http-multipart", ct, got.Kind)
		}
		if ref.SHA256 == "" {
			t.Errorf("ContentType %q: a fingerprint with no digest proves nothing", ct)
		}
	}
}

// The contract the whole media layer rests on, asserted directly on this
// codec: a ref offers bytes exactly when it carries a locator. Neither a
// captured ref without a path nor a fingerprint ref with one may exist.
func TestSTTRequest_LocatorPresenceMatchesCustody(t *testing.T) {
	n := NewOpenAIAudioTranscriptionsNormalizer()
	for _, tc := range []struct{ name, ct string }{
		{"captured", "audio/mpeg"},
		{"fingerprint", "multipart/form-data; boundary=x"},
	} {
		got, err := n.Normalize(context.Background(), wavBytes(2048), core.Meta{
			Direction:   core.DirectionRequest,
			ContentType: tc.ct,
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		ref := got.HTTP.BodyView.MediaRef
		offersBytes := ref.Source == core.MediaCaptured
		if offersBytes != (ref.Locator != "") {
			t.Errorf("%s: source=%q locator=%q — offersBytes and a resolvable locator must agree",
				tc.name, ref.Source, ref.Locator)
		}
	}
}

func TestIsMultipartType(t *testing.T) {
	for _, ct := range []string{"multipart/form-data", "multipart/mixed; boundary=x", " Multipart/Form-Data "} {
		if !isMultipartType(ct) {
			t.Errorf("isMultipartType(%q) = false, want true", ct)
		}
	}
	for _, ct := range []string{"audio/wav", "audio/mpeg", "application/json", "", "multipartish/x"} {
		if isMultipartType(ct) {
			t.Errorf("isMultipartType(%q) = true, want false", ct)
		}
	}
}
