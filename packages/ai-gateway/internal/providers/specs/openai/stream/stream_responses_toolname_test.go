// Package stream_test — the /v1/responses tool-call name, which the wire states once.
// Named failure modes:
//   - a tool call reaches canonical with arguments and no name (client cannot dispatch it)
//   - the name from one output item leaks onto another item's call
package stream_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	provcore "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/core"
	ostream "github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/providers/specs/openai/stream"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

// responsesToolStream is the shape a real /v1/responses turn uses: the function
// name arrives ONCE on response.output_item.added, and the argument deltas that
// follow carry only item_id and output_index. A decoder that reads only the
// deltas therefore produces a tool call with no name — the client learns a tool
// was requested and cannot tell which one.
//
// Two concurrent items are included so a decoder cannot pass by remembering a
// single "last name seen": output_index 0 is get_weather, output_index 1 is
// get_time, and their argument deltas interleave.
const responsesToolStream = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"get_weather","arguments":""}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"get_time","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_a","delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_b","delta":"{\"tz\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_a","delta":"\"Paris\"}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}

`

func TestResponsesDecoders_ToolCallCarriesItsName(t *testing.T) {
	// Both sessions decode this wire — the egress session serves a /v1/responses
	// ingress, the plain one serves a Responses-native upstream — and both had
	// dropped the name. Running the same stream through each keeps the pair from
	// drifting apart again.
	for _, tc := range []struct {
		name string
		open func(io.ReadCloser) (provcore.StreamSession, error)
	}{
		{
			name: "egress session",
			open: func(r io.ReadCloser) (provcore.StreamSession, error) {
				return ostream.NewStreamDecoder(slog.Default()).Open(r, typology.WireShapeOpenAIResponses)
			},
		},
		{
			name: "responses session",
			open: func(r io.ReadCloser) (provcore.StreamSession, error) {
				return ostream.NewResponsesStreamDecoder(slog.Default()).Open(r, typology.WireShapeOpenAIResponses)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := tc.open(io.NopCloser(strings.NewReader(responsesToolStream)))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer sess.Close()

			namesByIndex := map[int]string{}
			argsByIndex := map[int]string{}
			for range 32 {
				chunk, err := sess.Next(context.Background())
				if err != nil {
					break
				}
				for _, d := range chunk.ToolCallDeltas {
					if d.Name != "" {
						namesByIndex[d.Index] = d.Name
					}
					argsByIndex[d.Index] += d.Arguments
				}
				if chunk.Done {
					break
				}
			}

			if got := namesByIndex[0]; got != "get_weather" {
				t.Errorf("output_index 0 name = %q, want %q — the name is announced on "+
					"output_item.added and never repeated on the deltas", got, "get_weather")
			}
			if got := namesByIndex[1]; got != "get_time" {
				t.Errorf("output_index 1 name = %q, want %q — a second concurrent item must "+
					"keep its own name", got, "get_time")
			}
			if got := argsByIndex[0]; got != `{"city":"Paris"}` {
				t.Errorf("output_index 0 arguments = %q, want %q", got, `{"city":"Paris"}`)
			}
			if got := argsByIndex[1]; got != `{"tz":` {
				t.Errorf("output_index 1 arguments = %q, want %q", got, `{"tz":`)
			}
		})
	}
}
