package agent

import (
	"context"
	"testing"
)

func TestFakeModelRecordsAndReplays(t *testing.T) {
	fm := newFakeModel(asstText("hello"))
	var streamed string
	resp, err := fm.Generate(context.Background(), ModelRequest{System: "S"}, func(s string) { streamed += s }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopEndTurn || resp.Message.Text() != "hello" {
		t.Fatalf("unexpected resp %+v", resp)
	}
	if streamed != "hello" {
		t.Fatalf("onText should replay text, got %q", streamed)
	}
	if len(fm.gotReqs) != 1 || fm.gotReqs[0].System != "S" {
		t.Fatalf("fake should record the request, got %+v", fm.gotReqs)
	}
	// Past the script → safe end-turn default, never a runaway tool_use.
	r2, _ := fm.Generate(context.Background(), ModelRequest{}, nil, nil)
	if r2.StopReason != StopEndTurn || len(r2.Message.ToolUses()) != 0 {
		t.Fatalf("exhausted fake must default to end_turn, got %+v", r2)
	}
}
