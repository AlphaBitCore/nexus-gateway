package codecs

import (
	"context"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

func TestGeminiStream_FunctionCallArgsUseLatestSnapshot(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{}},"thoughtSignature":"sig-1"}]},"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"location":"San","stale":"only-intermediate"}}}]},"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"location":"San Francisco"}}}]},"finishReason":"STOP","index":0}]}`,
	}, "\n")

	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 1 {
		t.Fatalf("messages/content = %+v, want one assistant tool call", got.Messages)
	}
	call := got.Messages[0].Content[0].ToolUse
	if call == nil || call.CallID != "call-1" || call.Name != "lookup" {
		t.Fatalf("call = %+v, want call-1/lookup", call)
	}
	if gotLocation, _ := call.Input["location"].(string); gotLocation != "San Francisco" {
		t.Errorf("final args location = %q, want San Francisco; input = %#v", gotLocation, call.Input)
	}
	if _, present := call.Input["stale"]; present {
		t.Errorf("final args retained an intermediate snapshot key: %#v", call.Input)
	}
	if call.ThoughtSignature != "sig-1" {
		t.Errorf("thought signature = %q, want sig-1", call.ThoughtSignature)
	}
}

func TestGeminiStream_SameNameCallsWithoutIDsRemainDistinct(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"location":"Paris"}}},{"functionCall":{"name":"lookup","args":{"location":"Tokyo"}}}]} ,"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"location":"Paris, France"}}},{"functionCall":{"name":"lookup","args":{"location":"Tokyo, Japan"}}}]},"finishReason":"STOP","index":0}]}`,
	}, "\n")

	got, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(raw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
		t.Fatalf("messages/content = %+v, want two tool calls", got.Messages)
	}
	seen := map[string]bool{}
	for i, wantLocation := range []string{"Paris, France", "Tokyo, Japan"} {
		call := got.Messages[0].Content[i].ToolUse
		if call == nil {
			t.Fatalf("content[%d] = %+v, want tool use", i, got.Messages[0].Content[i])
		}
		if seen[call.CallID] {
			t.Fatalf("duplicate fallback ID %q for calls: %+v", call.CallID, got.Messages[0].Content)
		}
		seen[call.CallID] = true
		if gotLocation, _ := call.Input["location"].(string); gotLocation != wantLocation {
			t.Errorf("content[%d] location = %q, want %q", i, gotLocation, wantLocation)
		}
	}
}

func TestGeminiFallbackIDStableBetweenStreamAndNonStream(t *testing.T) {
	streamRaw := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"partial"}}}]},"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","args":{"q":"final"}}}]},"finishReason":"STOP","index":0}]}`,
	}, "\n")
	nonStreamRaw := `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"final"}}}]},"finishReason":"STOP","index":0}]}`
	streamed, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(streamRaw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	nonStream, err := NewGeminiGenerateNormalizer().Normalize(context.Background(), []byte(nonStreamRaw), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatal(err)
	}
	streamID := streamed.Messages[0].Content[0].ToolUse.CallID
	nonStreamID := nonStream.Messages[0].Content[0].ToolUse.CallID
	if streamID == "" || streamID != nonStreamID {
		t.Fatalf("fallback IDs differ for matching coordinates: stream=%q nonstream=%q", streamID, nonStreamID)
	}
}

func TestGeminiStream_DuplicateNativeIDsPreserveDistinctPartsLikeNonStream(t *testing.T) {
	streamRaw := `data: {"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"id":"duplicate-id","name":"lookup","args":{"q":"first"}},"thoughtSignature":"sig-a"},` +
		`{"functionCall":{"id":"duplicate-id","name":"lookup","args":{"q":"second"}},"thoughtSignature":"sig-b"}` +
		`]},"finishReason":"STOP","index":0}]}`
	nonStreamRaw := `{"candidates":[{"content":{"role":"model","parts":[` +
		`{"functionCall":{"id":"duplicate-id","name":"lookup","args":{"q":"first"}},"thoughtSignature":"sig-a"},` +
		`{"functionCall":{"id":"duplicate-id","name":"lookup","args":{"q":"second"}},"thoughtSignature":"sig-b"}` +
		`]},"finishReason":"STOP","index":0}]}`

	streamed, err := NewGeminiGenerateNormalizer().Normalize(
		context.Background(), []byte(streamRaw), core.Meta{Direction: core.DirectionResponse, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	nonStream, err := NewGeminiGenerateNormalizer().Normalize(
		context.Background(), []byte(nonStreamRaw), core.Meta{Direction: core.DirectionResponse})
	if err != nil {
		t.Fatal(err)
	}
	for label, got := range map[string]core.NormalizedPayload{"stream": streamed, "nonstream": nonStream} {
		if len(got.Messages) != 1 || len(got.Messages[0].Content) != 2 {
			t.Fatalf("%s merged duplicate native IDs: %+v", label, got.Messages)
		}
		first := got.Messages[0].Content[0].ToolUse
		second := got.Messages[0].Content[1].ToolUse
		if first.CallID != "duplicate-id" || second.CallID != "duplicate-id" ||
			first.Input["q"] != "first" || second.Input["q"] != "second" ||
			first.ThoughtSignature != "sig-a" || second.ThoughtSignature != "sig-b" {
			t.Fatalf("%s changed duplicate-id calls: first=%+v second=%+v", label, first, second)
		}
	}
}
