package typology

import "testing"

// TestKindFromPathSegment_KnownSegments pins the path-segment → EndpointKind
// mapping. This is the single source of truth used by
// shared/policy/hooks/core.EndpointTypeFromPath and
// ai-gateway/internal/platform/audit.EndpointTypeFromPath; failure here
// means a delegated caller would now return a different value.
func TestKindFromPathSegment_KnownSegments(t *testing.T) {
	cases := []struct {
		segment string
		want    EndpointKind
	}{
		// Chat family — three segment forms collapse to one kind.
		{"chat/completions", EndpointKindChat},
		{"completions", EndpointKindChat},
		{"responses", EndpointKindChat},
		// Embeddings (plural, matches deployed wire string).
		{"embeddings", EndpointKindEmbeddings},
		// Speech-to-text
		{"audio/transcriptions", EndpointKindSTT},
		{"audio/translations", EndpointKindSTT},
		// Text-to-speech
		{"audio/speech", EndpointKindTTS},
		// Image generation (3 paths → 1 kind)
		{"images/generations", EndpointKindImageGeneration},
		{"images/edits", EndpointKindImageGeneration},
		{"images/variations", EndpointKindImageGeneration},
		// Batch
		{"batches", EndpointKindBatch},
	}
	for _, c := range cases {
		if got := KindFromPathSegment(c.segment); got != c.want {
			t.Errorf("KindFromPathSegment(%q) = %v, want %v", c.segment, got, c.want)
		}
	}
}

func TestKindFromPathSegment_UnknownReturnsEmpty(t *testing.T) {
	cases := []string{"", "unknown", "models", "audio/unknown", "videos/generations"}
	for _, s := range cases {
		got := KindFromPathSegment(s)
		if got != "" {
			t.Errorf("KindFromPathSegment(%q) = %q, want \"\" (unknown → empty)", s, got)
		}
	}
}
