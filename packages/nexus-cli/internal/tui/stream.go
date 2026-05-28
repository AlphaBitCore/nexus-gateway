package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
)

// streamGateway is the streaming subset of Gateway the pump needs.
type streamGateway interface {
	ChatStream(ctx context.Context, vkSecret string, req core.ChatRequest, onDelta func(string)) (*core.ChatUsage, error)
}

// streamDone carries the terminal result of a background ChatStream.
type streamDone struct {
	usage *core.ChatUsage
	err   error
}

// chatStreamer runs a VK-authed ChatStream in the background, exposing each
// content delta and the terminal result as drainable channels plus a cancel for
// teardown. It is shared by the Chat Playground and the Event "explain" panel
// so the goroutine/channel/cancel plumbing lives in one place.
type chatStreamer struct {
	deltaCh chan string
	doneCh  chan streamDone
	cancel  context.CancelFunc
}

// startChatStream launches the stream and returns the running streamer. build
// runs on the stream goroutine to assemble the request (so callers can do
// pre-work like fetching a normalized event without blocking Update).
func startChatStream(gw streamGateway, vkSecret string, build func(ctx context.Context) core.ChatRequest) *chatStreamer {
	deltaCh := make(chan string, 64)
	doneCh := make(chan streamDone, 1)
	ctx, cancel := context.WithTimeout(context.Background(), chatStreamTimeout)
	s := &chatStreamer{deltaCh: deltaCh, doneCh: doneCh, cancel: cancel}
	go func() {
		req := build(ctx)
		usage, err := gw.ChatStream(ctx, vkSecret, req, func(d string) { deltaCh <- d })
		close(deltaCh)
		doneCh <- streamDone{usage: usage, err: err}
	}()
	return s
}

// wait drains one delta (or, once the stream closed, the terminal done) and
// re-issues itself, adapting each into the caller's message type.
func (s *chatStreamer) wait(onDelta func(string) tea.Msg, onDone func(streamDone) tea.Msg) tea.Cmd {
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
func (s *chatStreamer) stop() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}
