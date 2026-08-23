package core

import (
	"strings"
	"testing"
)

// The compliance boundary, stated once and pinned.
//
// Hooks scan what TextProjection returns. A media element must contribute
// NOTHING to it: the bytes are binary, a regex over base64 matches nothing
// useful, and putting them there costs every scan the size of every payload.
// This program has broken that boundary repeatedly — a document body
// serialised into a text block, a wire structure rendered as prose, a
// payload smuggled through a codec's fallback — so the property is asserted
// here rather than re-established by inspection each time a codec changes.
//
// The sibling text must still project. "Scan nothing" would be trivially
// satisfiable by projecting nothing at all, and it is the caption next to
// the image that a compliance rule most needs to see.

func TestMediaContributesNothingToTheComplianceProjection(t *testing.T) {
	// A payload marker long enough that a partial leak is unmistakable.
	marker := strings.Repeat("QUJD", 5000)

	for _, tc := range []struct {
		name    string
		payload *NormalizedPayload
	}{
		{
			"captured image beside its caption",
			&NormalizedPayload{Kind: KindAIChat, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
				{Type: ContentText, Text: "describe this"},
				{Type: ContentMedia, MediaRef: &MediaRef{
					Modality: ModalityImage, Mime: "image/png", Source: MediaCaptured,
					Locator:   "datauri:messages.0.content.1.image_url.url",
					SizeBytes: 15000, SHA256: marker,
				}},
			}}}},
		},
		{
			"every custody state at once",
			&NormalizedPayload{Kind: KindAIChat, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
				{Type: ContentText, Text: "describe this"},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaCaptured, Locator: marker}},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaExternal, URL: marker}},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaProviderRef, ProviderRef: marker}},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaFingerprint, SHA256: marker}},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaAgedOut, Cause: marker}},
				{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaAbsent, Mime: marker}},
			}}}},
		},
		{
			"whole-body binary",
			&NormalizedPayload{Kind: KindHTTPBinary, HTTP: &HTTPPayload{BodyView: &HTTPBodyView{
				MediaRef: &MediaRef{Modality: ModalityAudio, Source: MediaCaptured, Locator: "body", SHA256: marker},
			}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frags := tc.payload.TextProjection()
			for i, s := range frags {
				if strings.Contains(s, marker[:64]) {
					t.Fatalf("fragment %d carries media content into the scanner: %d bytes", i, len(s))
				}
			}
			// Reasoning is opt-in, and opting in must not change this.
			for i, s := range tc.payload.TextProjectionWith(TextProjectionOptions{IncludeReasoning: true}) {
				if strings.Contains(s, marker[:64]) {
					t.Fatalf("fragment %d leaks once reasoning is included", i)
				}
			}
		})
	}
}

// The other half: a caption beside an image is exactly what a rule needs to
// match, so "media projects nothing" must not become "nothing projects".
func TestTextBesideMediaStillReachesTheScanner(t *testing.T) {
	p := &NormalizedPayload{Kind: KindAIChat, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
		{Type: ContentText, Text: "card 4111 1111 1111 1111"},
		{Type: ContentMedia, MediaRef: &MediaRef{Source: MediaCaptured, Locator: "body"}},
	}}}}
	var seen bool
	for _, s := range p.TextProjection() {
		if strings.Contains(s, "4111 1111 1111 1111") {
			seen = true
		}
	}
	if !seen {
		t.Fatal("the text beside an image must still be scannable, or the image hides its own caption")
	}
}
