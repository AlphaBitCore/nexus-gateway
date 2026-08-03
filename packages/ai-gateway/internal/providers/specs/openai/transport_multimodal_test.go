// Tests for the multimodal entries in the OpenAI-compat URL path table:
// image generation, TTS speech, and STT transcriptions resolve to their
// upstream paths via BuildURL, and the images wire shape maps to
// /v1/images/generations specifically (edits/variations need ingress-path
// preservation and are deliberately NOT derivable from the wire shape).
package openai_test

import (
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestPathForEndpoint_multimodal(t *testing.T) {
	cases := []struct {
		shape    typology.WireShape
		wantPath string
	}{
		{typology.WireShapeOpenAIImages, "/v1/images/generations"},
		{typology.WireShapeOpenAIAudioSpeech, "/v1/audio/speech"},
		{typology.WireShapeOpenAIAudioTranscriptions, "/v1/audio/transcriptions"},
	}
	for _, tc := range cases {
		t.Run(string(tc.shape), func(t *testing.T) {
			got, ok := openai.PathForEndpoint(tc.shape)
			if !ok {
				t.Fatalf("PathForEndpoint(%s) = not supported; want %s", tc.shape, tc.wantPath)
			}
			if got != tc.wantPath {
				t.Errorf("PathForEndpoint(%s) = %s, want %s", tc.shape, got, tc.wantPath)
			}
		})
	}
}

func TestBuildURL_multimodal(t *testing.T) {
	tr := openai.NewTransport(nil)
	target := provcore.CallTarget{BaseURL: "https://api.openai.com/"}

	url, err := tr.BuildURL(target, typology.WireShapeOpenAIImages, false)
	if err != nil {
		t.Fatalf("BuildURL(images): %v", err)
	}
	if want := "https://api.openai.com/v1/images/generations"; url != want {
		t.Errorf("BuildURL(images) = %s, want %s", url, want)
	}

	url, err = tr.BuildURL(target, typology.WireShapeOpenAIAudioSpeech, false)
	if err != nil {
		t.Fatalf("BuildURL(audio-speech): %v", err)
	}
	if want := "https://api.openai.com/v1/audio/speech"; url != want {
		t.Errorf("BuildURL(audio-speech) = %s, want %s", url, want)
	}
}
