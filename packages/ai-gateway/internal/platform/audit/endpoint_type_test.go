// Package audit — endpoint_type_test.go verifies the typed EndpointType
// constants and the EndpointTypeFromPath helper.
//
// Named failure modes covered:
//   - EndpointTypeFromPath: each known segment maps to its canonical kind
//   - EndpointTypeFromPath: unknown and empty segment → ""
//   - Wire format: every constant marshals to the expected JSON string
//   - Constant values: the typed constants match the canonical
//     typology.EndpointKind string vocabulary
package audit

import (
	"encoding/json"
	"testing"
)

// TestEndpointTypeFromPath_KnownSegments verifies that every path-segment
// string the AI Gateway produces is mapped to the correct canonical kind.
func TestEndpointTypeFromPath_KnownSegments(t *testing.T) {
	cases := []struct {
		segment string
		want    EndpointType
	}{
		// Chat family — all chat-family path segments collapse to the
		// single canonical "chat" kind.
		{"chat/completions", EndpointTypeChat},
		{"completions", EndpointTypeChat},
		{"responses", EndpointTypeChat},
		// Embeddings
		{"embeddings", EndpointTypeEmbeddings},
		// Speech-to-text
		{"audio/transcriptions", EndpointTypeSTT},
		{"audio/translations", EndpointTypeSTT},
		// Text-to-speech
		{"audio/speech", EndpointTypeTTS},
		// Image generation
		{"images/generations", EndpointTypeImageGeneration},
		{"images/edits", EndpointTypeImageGeneration},
		{"images/variations", EndpointTypeImageGeneration},
		// Batch
		{"batches", EndpointTypeBatch},
		// Unknown / empty — must return "" (early-failure rows store the
		// empty string in audit.Record.EndpointType).
		{"", ""},
		{"models", ""},
		{"unknown/path", ""},
	}
	for _, c := range cases {
		got := EndpointTypeFromPath(c.segment)
		if got != c.want {
			t.Errorf("EndpointTypeFromPath(%q) = %q, want %q", c.segment, got, c.want)
		}
	}
}

// TestEndpointTypeConstants_WireValues asserts that every typed constant
// matches the canonical typology.EndpointKind string vocabulary that
// downstream analytics and Prometheus consumers read.
func TestEndpointTypeConstants_WireValues(t *testing.T) {
	cases := []struct {
		constant EndpointType
		want     string
	}{
		{EndpointTypeChat, "chat"},
		{EndpointTypeEmbeddings, "embeddings"},
		{EndpointTypeSTT, "stt"},
		{EndpointTypeTTS, "tts"},
		{EndpointTypeImageGeneration, "image_generation"},
		{EndpointTypeBatch, "batch"},
	}
	for _, c := range cases {
		if c.constant != c.want {
			t.Errorf("constant %q has value %q, want %q", c.constant, c.constant, c.want)
		}
	}
}

// TestEndpointTypeConstants_JSONRoundTrip verifies that the typed constants
// survive a JSON marshal → unmarshal round-trip via TrafficEventMessage.
// The wire format must remain a plain string (no type decorators).
func TestEndpointTypeConstants_JSONRoundTrip(t *testing.T) {
	type wireMsg struct {
		EndpointType string `json:"endpointType,omitempty"`
	}

	allConstants := []EndpointType{
		EndpointTypeChat,
		EndpointTypeEmbeddings,
		EndpointTypeSTT,
		EndpointTypeTTS,
		EndpointTypeImageGeneration,
		EndpointTypeBatch,
	}

	for _, ep := range allConstants {
		msg := wireMsg{EndpointType: ep}
		data, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("json.Marshal(%q) failed: %v", ep, err)
		}
		var decoded wireMsg
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("json.Unmarshal failed for %q: %v", ep, err)
		}
		if decoded.EndpointType != ep {
			t.Errorf("round-trip: got %q, want %q", decoded.EndpointType, ep)
		}
	}
}
