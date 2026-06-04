package kit

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

type fakeStreamGW struct {
	deltas []string
	usage  *core.ChatUsage
	err    error
}

func (f fakeStreamGW) ChatStream(_ context.Context, _ string, _ core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error) {
	for _, d := range f.deltas {
		onDelta(d)
	}
	return f.usage, f.err
}

// TestStartChatStream_DrainsDeltasThenDone verifies the streamer surfaces every
// content delta in order and then the terminal result (usage), via repeated Wait.
func TestStartChatStream_DrainsDeltasThenDone(t *testing.T) {
	gw := fakeStreamGW{deltas: []string{"a", "b"}, usage: &core.ChatUsage{PromptTokens: 3}}
	s := StartChatStream(gw, "vk", func(context.Context) core.ChatRequest { return core.ChatRequest{} })

	var got []string
	for i := 0; i < 3; i++ {
		msg := s.Wait(
			func(d string) tea.Msg { return d },
			func(sd StreamDone) tea.Msg { return sd },
		)()
		switch m := msg.(type) {
		case string:
			got = append(got, m)
		case StreamDone:
			if strings.Join(got, "") != "ab" {
				t.Fatalf("deltas should arrive in order before done, got %v", got)
			}
			if m.Usage == nil || m.Usage.PromptTokens != 3 {
				t.Fatalf("terminal usage wrong: %+v", m.Usage)
			}
			return
		}
	}
	t.Fatal("never received the terminal done frame")
}

func TestChatStreamer_StopNilSafeAndCancels(t *testing.T) {
	var nilS *ChatStreamer
	nilS.Stop() // must not panic

	s := StartChatStream(fakeStreamGW{deltas: []string{"x"}}, "vk",
		func(context.Context) core.ChatRequest { return core.ChatRequest{} })
	s.Stop() // cancels the background context; idempotent
	s.Stop()
}
