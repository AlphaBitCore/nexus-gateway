// Coverage backfill: tests for residual low-coverage branches in the core
// sub-package (regex_cache, SpansFromModifiedContent, endpoint/modality helpers).
package core

import (
	"testing"

	normalize "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// --- ChatOnly helper --------------------------------------------------------

func TestChatOnly_SupportsEndpoint_Chat(t *testing.T) {
	var h ChatOnly
	if !h.SupportsEndpoint(EndpointTypeChat) {
		t.Error("ChatOnly must support EndpointTypeChat")
	}
}

func TestChatOnly_SupportsEndpoint_Empty(t *testing.T) {
	var h ChatOnly
	if !h.SupportsEndpoint("") {
		t.Error("ChatOnly must support empty endpoint (backward-compat)")
	}
}

func TestChatOnly_SupportsEndpoint_Embeddings(t *testing.T) {
	var h ChatOnly
	if h.SupportsEndpoint(EndpointTypeEmbeddings) {
		t.Error("ChatOnly must NOT support EndpointTypeEmbeddings")
	}
}

func TestChatOnly_SupportsModality_Text(t *testing.T) {
	var h ChatOnly
	if !h.SupportsModality(ModalityText) {
		t.Error("ChatOnly must support ModalityText")
	}
}

func TestChatOnly_SupportsModality_Empty(t *testing.T) {
	var h ChatOnly
	if !h.SupportsModality("") {
		t.Error("ChatOnly must support empty modality (backward-compat)")
	}
}

func TestChatOnly_SupportsModality_Image(t *testing.T) {
	var h ChatOnly
	if h.SupportsModality(ModalityImage) {
		t.Error("ChatOnly must NOT support ModalityImage")
	}
}

// --- AnyEndpointAnyModality helper ------------------------------------------

func TestAnyEndpointAnyModality_SupportsEndpoint_AlwaysTrue(t *testing.T) {
	var h AnyEndpointAnyModality
	for _, ep := range []EndpointType{
		EndpointTypeChat, EndpointTypeEmbeddings, EndpointTypeBatch,
		EndpointTypeJob, EndpointTypeSTT, EndpointTypeTTS,
		EndpointTypeImageGeneration, EndpointTypeVideoGeneration, "",
	} {
		if !h.SupportsEndpoint(ep) {
			t.Errorf("AnyEndpointAnyModality.SupportsEndpoint(%q) must return true", ep)
		}
	}
}

func TestAnyEndpointAnyModality_SupportsModality_AlwaysTrue(t *testing.T) {
	var h AnyEndpointAnyModality
	for _, m := range []Modality{ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ""} {
		if !h.SupportsModality(m) {
			t.Errorf("AnyEndpointAnyModality.SupportsModality(%q) must return true", m)
		}
	}
}

// --- TextOnlyContentScanning helper -----------------------------------------

func TestTextOnlyContentScanning_SupportsEndpoint_Included(t *testing.T) {
	var h TextOnlyContentScanning
	for _, ep := range []EndpointType{
		EndpointTypeChat, EndpointTypeSTT, EndpointTypeImageGeneration,
		EndpointTypeTTS, EndpointTypeVideoGeneration,
		EndpointTypeEmbeddings, // embedding inputs are text
		"",
	} {
		if !h.SupportsEndpoint(ep) {
			t.Errorf("TextOnlyContentScanning.SupportsEndpoint(%q) must return true", ep)
		}
	}
}

// TestTextOnlyContentScanning_SupportsEndpoint_Excluded verifies that
// EndpointTypeBatch and EndpointTypeJob are excluded.
// EndpointTypeEmbeddings is not excluded: embedding inputs are plain
// text and must be scannable by text hooks on the request side. The
// response-side gating is handled separately via TextOnlyContentScanningMarker
// in BuildPipeline.
func TestTextOnlyContentScanning_SupportsEndpoint_Excluded(t *testing.T) {
	var h TextOnlyContentScanning
	for _, ep := range []EndpointType{EndpointTypeBatch, EndpointTypeJob} {
		if h.SupportsEndpoint(ep) {
			t.Errorf("TextOnlyContentScanning.SupportsEndpoint(%q) must return false", ep)
		}
	}
	// Embeddings are included on the request side.
	if !h.SupportsEndpoint(EndpointTypeEmbeddings) {
		t.Error("TextOnlyContentScanning.SupportsEndpoint(EndpointTypeEmbeddings) must return true")
	}
}

func TestTextOnlyContentScanning_SupportsModality_TextAndEmpty(t *testing.T) {
	var h TextOnlyContentScanning
	if !h.SupportsModality(ModalityText) {
		t.Error("TextOnlyContentScanning must support ModalityText")
	}
	if !h.SupportsModality("") {
		t.Error("TextOnlyContentScanning must support empty modality (backward-compat)")
	}
}

func TestTextOnlyContentScanning_SupportsModality_NonText(t *testing.T) {
	var h TextOnlyContentScanning
	for _, m := range []Modality{ModalityImage, ModalityAudio, ModalityVideo} {
		if h.SupportsModality(m) {
			t.Errorf("TextOnlyContentScanning.SupportsModality(%q) must return false", m)
		}
	}
}

// --- EndpointTypeFromPath ---------------------------------------------------

func TestEndpointTypeFromPath_KnownPaths(t *testing.T) {
	tests := []struct {
		path string
		want EndpointType
	}{
		{"chat/completions", EndpointTypeChat},
		{"completions", EndpointTypeChat},
		{"responses", EndpointTypeChat},
		{"embeddings", EndpointTypeEmbeddings},
		{"audio/transcriptions", EndpointTypeSTT},
		{"audio/translations", EndpointTypeSTT},
		{"audio/speech", EndpointTypeTTS},
		{"images/generations", EndpointTypeImageGeneration},
		{"images/edits", EndpointTypeImageGeneration},
		{"images/variations", EndpointTypeImageGeneration},
		{"batches", EndpointTypeBatch},
		{"unknown/path", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got := EndpointTypeFromPath(tc.path)
		if got != tc.want {
			t.Errorf("EndpointTypeFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// --- SetRegexCacheCap: panic on non-positive cap ---------------------------

func TestSetRegexCacheCap_NonPositivePanics(t *testing.T) {
	// Restore the production cap after the panic — otherwise downstream
	// tests inherit a tiny cache and tank coverage on the cache-hit path.
	defer SetRegexCacheCap(defaultRegexCacheCap)

	defer func() {
		if rec := recover(); rec == nil {
			t.Fatal("expected panic on non-positive cap")
		}
	}()
	SetRegexCacheCap(-1)
}

// --- SpansFromModifiedContent: modified > original triggers clamp ---------

func TestSpansFromModifiedContent_ModifiedLongerThanOriginalClamps(t *testing.T) {
	// Original has 1 text block; modified passes 3 entries — limit should
	// clamp to len(original) = 1 (the `len(original) < limit` branch).
	in := &HookInput{Normalized: &normalize.NormalizedPayload{
		Kind:             normalize.KindAIChat,
		NormalizeVersion: normalize.SchemaVersion,
		Messages: []normalize.Message{{
			Role: normalize.RoleUser,
			Content: []normalize.ContentBlock{
				{Type: normalize.ContentText, Text: "only"},
			},
		}},
	}}
	modified := []ContentBlock{{Text: "X"}, {Text: "Y"}, {Text: "Z"}}
	spans := SpansFromModifiedContent(in, modified,
		normalize.SourceHook, "r", normalize.ActionRedact)
	if len(spans) != 1 {
		t.Errorf("len(spans) = %d, want 1 (clamp to len(original))", len(spans))
	}
}
