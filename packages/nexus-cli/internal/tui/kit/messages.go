package kit

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Poll intervals: the radar refreshes fast; aggregate views slower (NFR-4).
const (
	PollFast = 2 * time.Second
	PollSlow = 5 * time.Second
)

// AnimInterval drives cockpit animation (pulsing status lights). It only advances
// a phase counter — it never refetches — so it can tick faster than the polls.
const AnimInterval = 700 * time.Millisecond

// ConvAnimInterval drives the conversation's typewriter reveal + working spinner.
// Faster than AnimInterval so streamed text reads as smooth, even typing.
const ConvAnimInterval = 80 * time.Millisecond

// LoginTimeout bounds the wizard's browser-login wait (the loopback listener
// closes when it fires so a never-completed login does not hang the wizard).
const LoginTimeout = 3 * time.Minute

// AgentTurnTimeout bounds one agent turn so a stuck model/tool never wedges the
// UI; the bridge cancels the turn's context when it fires (the agent loop returns
// the model/context error, which rides back on agentDoneMsg).
const AgentTurnTimeout = 5 * time.Minute

// DefaultViewWidth is the fallback render width before the first WindowSizeMsg.
const DefaultViewWidth = 100

// Tick schedules msg after d.
func Tick(d time.Duration, msg tea.Msg) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return msg })
}

// FetchCtx bounds a single data fetch so a hung gateway never freezes a view. The
// budget must comfortably exceed the transport's TLS-handshake budget (30s, widened in
// core.NewHTTPTransport for slow prod TLS): after the CLI sits idle the keep-alive
// connections go cold, so the next poll pays a full cold handshake. A 10s fetch ctx was
// shorter than that 30s handshake budget, so the first poll after idle died with
// "context deadline exceeded" mid-handshake. 35s covers a cold handshake + the request
// while still surfacing a genuinely hung gateway in bounded time.
func FetchCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 35*time.Second)
}
