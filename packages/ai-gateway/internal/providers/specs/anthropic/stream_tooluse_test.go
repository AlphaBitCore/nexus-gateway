package anthropic

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/typology"
)

func TestStreamDecoder_ToolUseStreamingConcatenatesArguments(t *testing.T) {
	raw, err := os.ReadFile("testdata/tooluse_stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	dec := NewStreamDecoder(nil)
	sess, err := dec.Open(io.NopCloser(strings.NewReader(string(raw))), typology.WireShapeAnthropicMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close() //nolint:errcheck

	ctx := context.Background()
	var argParts []string
	var sawStart bool
	for {
		ch, err := sess.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		for _, d := range ch.ToolCallDeltas {
			if d.Name == "get_weather" && d.ID == "toolu_test_01" {
				if d.Arguments == "" {
					sawStart = true
				} else {
					argParts = append(argParts, d.Arguments)
				}
			}
		}
	}
	if !sawStart {
		t.Fatal("expected initial tool_use delta with empty arguments")
	}
	got := strings.Join(argParts, "")
	want := `{"location": "San Francisco"}`
	if got != want {
		t.Fatalf("concatenated arguments=%q want %q", got, want)
	}
}
