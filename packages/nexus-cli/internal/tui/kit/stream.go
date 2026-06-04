package kit

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// ChatStreamTimeout bounds a background ChatStream so a stalled upstream can't keep
// the goroutine + connection alive forever.
const ChatStreamTimeout = 2 * time.Minute

// StreamGateway is the streaming subset of Gateway the pump needs.
type StreamGateway interface {
	ChatStream(ctx context.Context, vkSecret string, req core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error)
}

// StreamDone carries the terminal result of a background ChatStream.
type StreamDone struct {
	Usage *core.ChatUsage
	Err   error
}

// ChatStreamer runs a VK-authed ChatStream in the background, exposing each
// content delta and the terminal result as drainable channels plus a cancel for
// teardown. It is shared by the Chat Playground and the Event "explain" panel
// so the goroutine/channel/cancel plumbing lives in one place.
type ChatStreamer struct {
	deltaCh chan string
	doneCh  chan StreamDone
	cancel  context.CancelFunc
}

// StartChatStream launches the stream and returns the running streamer. build
// runs on the stream goroutine to assemble the request (so callers can do
// pre-work like fetching a normalized event without blocking Update).
func StartChatStream(gw StreamGateway, vkSecret string, build func(ctx context.Context) core.ChatRequest) *ChatStreamer {
	deltaCh := make(chan string, 64)
	doneCh := make(chan StreamDone, 1)
	ctx, cancel := context.WithTimeout(context.Background(), ChatStreamTimeout)
	s := &ChatStreamer{deltaCh: deltaCh, doneCh: doneCh, cancel: cancel}
	go func() {
		req := build(ctx)
		usage, err := gw.ChatStream(ctx, vkSecret, req, func(d string) { deltaCh <- d })
		close(deltaCh)
		doneCh <- StreamDone{Usage: usage, Err: err}
	}()
	return s
}

// wait drains one delta (or, once the stream closed, the terminal done) and
// re-issues itself, adapting each into the caller's message type.
func (s *ChatStreamer) Wait(onDelta func(string) tea.Msg, onDone func(StreamDone) tea.Msg) tea.Cmd {
	deltaCh, doneCh := s.deltaCh, s.doneCh
	return func() tea.Msg {
		if d, ok := <-deltaCh; ok {
			return onDelta(d)
		}
		return onDone(<-doneCh)
	}
}

// stop cancels the background stream. Idempotent and nil-safe, so views call it
// freely on teardown (navigate-away) to avoid leaking a goroutine + connection.
func (s *ChatStreamer) Stop() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}
