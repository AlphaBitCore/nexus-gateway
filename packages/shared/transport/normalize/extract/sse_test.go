package extract

import (
	"errors"
	"strings"
	"testing"
)

func TestWalkSSE_BasicEventDataFrames(t *testing.T) {
	raw := []byte("event: delta\ndata: {\"v\": \"hi\"}\n\nevent: delta\ndata: {\"v\": \"bye\"}\n\n")
	var frames []struct {
		Event string
		Data  string
	}
	err := WalkSSE(raw, func(event, data string) error {
		frames = append(frames, struct {
			Event string
			Data  string
		}{event, data})
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames: %d want 2", len(frames))
	}
	if frames[0].Event != "delta" || frames[0].Data != `{"v": "hi"}` {
		t.Errorf("frame 0: %+v", frames[0])
	}
	if frames[1].Event != "delta" || frames[1].Data != `{"v": "bye"}` {
		t.Errorf("frame 1: %+v", frames[1])
	}
}

func TestWalkSSE_DataOnlyNoEvent(t *testing.T) {
	raw := []byte("data: hello\n\ndata: world\n\n")
	var datas []string
	err := WalkSSE(raw, func(event, data string) error {
		datas = append(datas, data)
		if event != "" {
			t.Errorf("expected empty event, got %q", event)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if len(datas) != 2 || datas[0] != "hello" || datas[1] != "world" {
		t.Fatalf("datas: %v", datas)
	}
}

func TestWalkSSE_MultiLineDataConcatenated(t *testing.T) {
	raw := []byte("data: line1\ndata: line2\n\n")
	var got string
	err := WalkSSE(raw, func(_, data string) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if got != "line1\nline2" {
		t.Fatalf("data: %q want %q", got, "line1\nline2")
	}
}

func TestWalkSSE_SkipsComments(t *testing.T) {
	raw := []byte(": keepalive\ndata: payload\n\n")
	var got string
	err := WalkSSE(raw, func(_, data string) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if got != "payload" {
		t.Fatalf("data: %q", got)
	}
}

func TestWalkSSE_StopWalk(t *testing.T) {
	raw := []byte("data: 1\n\ndata: 2\n\ndata: 3\n\n")
	count := 0
	err := WalkSSE(raw, func(_, _ string) error {
		count++
		if count >= 2 {
			return ErrSSEStopWalk
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if count != 2 {
		t.Fatalf("count: %d want 2", count)
	}
}

func TestWalkSSE_PropagatesErrors(t *testing.T) {
	raw := []byte("data: 1\n\n")
	want := errors.New("boom")
	err := WalkSSE(raw, func(_, _ string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("err: %v want %v", err, want)
	}
}

func TestWalkSSE_TerminalFrameWithoutBlankLine(t *testing.T) {
	// Real SSE streams may end without a trailing blank line.
	raw := []byte("data: last")
	var got string
	err := WalkSSE(raw, func(_, data string) error {
		got = data
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if got != "last" {
		t.Fatalf("data: %q", got)
	}
}

func TestLooksLikeSSE(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{"event-prefix", "event: delta\ndata: ...", true},
		{"data-prefix", "data: hello\n\n", true},
		{"leading-whitespace", "  \n\nevent: delta\n", true},
		{"json", `{"foo": "bar"}`, false},
		{"plain-text", "hello world", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := LooksLikeSSE([]byte(c.raw))
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

// Real-world fixture sanity: a slice of the baa07c15 SSE stream walks
// cleanly with the right frame count and event labels.
func TestWalkSSE_ChatGPTFixture(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"event: delta_encoding",
		`data: "v1"`,
		"",
		`data: {"type":"resume_conversation_token","token":"abc"}`,
		"",
		"event: delta",
		`data: {"p":"","o":"add","v":{"message":{"author":{"role":"user"},"content":{"parts":["hello"]}}}}`,
		"",
		"event: delta",
		`data: {"p":"/message/content/parts/0","o":"append","v":"A few"}`,
		"",
		"event: delta",
		`data: {"v":" books that stand out"}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"))

	frames := 0
	deltaFrames := 0
	err := WalkSSE(raw, func(event, _ string) error {
		frames++
		if event == "delta" {
			deltaFrames++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk err: %v", err)
	}
	if frames < 5 {
		t.Fatalf("frames: %d want >= 5", frames)
	}
	if deltaFrames != 3 {
		t.Fatalf("delta frames: %d want 3", deltaFrames)
	}
}
