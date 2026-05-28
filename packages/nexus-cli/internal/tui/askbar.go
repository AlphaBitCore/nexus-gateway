package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// askPhase is the ask bar's state machine.
type askPhase int

const (
	askIdle askPhase = iota
	askRouting
	askAnswering
	askErrorPhase
)

// askBar is the `>` natural-language overlay. It routes a question to a structured
// intent via one VK-authed ChatStream call, then either navigates, opens an event
// explanation, or streams an answer (a second call over fetched read data). It
// never reaches a Gateway write method — the executor has no write arm.
type askBar struct {
	gw          Gateway
	session     Session
	entries     []viewEntry
	input       textinput.Model
	phase       askPhase
	question    string
	notice      string // error / status text
	answer      string // streamed answer text (answer path)
	buf         string // accumulates the routing reply until parse
	stream      *chatStreamer
	fetchCancel context.CancelFunc // cancels the in-flight answer-source read, if any
}

// ask-bar messages (distinct from chat/event so root routing is unambiguous).
type askRouteDeltaMsg struct{ text string }
type askRouteDoneMsg struct{ err error }
type askDataMsg struct {
	question string
	data     []byte
	err      error
}
type askAnswerDeltaMsg struct{ text string }
type askAnswerDoneMsg struct{ err error }
type askCloseMsg struct{}

// navigateMsg asks the root to jump to a view and (optionally) apply a Radar
// filter. Emitted by the ask bar's navigate intent.
type navigateMsg struct {
	view   string
	filter *askFilter
}

func newAskBar(gw Gateway, s Session, entries []viewEntry) askBar {
	ti := textinput.New()
	ti.Placeholder = "ask in plain English… (e.g. show 5xx from openai last hour)"
	ti.Prompt = "> "
	ti.CharLimit = 500
	ti.Focus()
	return askBar{gw: gw, session: s, entries: entries, input: ti}
}

// ready reports whether the session can talk to a model (same gate as Chat).
func (a askBar) ready() bool {
	return a.session.Model != "" && strings.TrimSpace(a.session.VKSecret) != ""
}

// stop tears down any in-flight work — the LLM stream and the answer-source read
// fetch (idempotent, nil-safe) — so a dismissed or navigated-away ask never leaks
// the goroutine, the upstream connection, or the read in flight.
func (a *askBar) stop() {
	a.stream.stop()
	if a.fetchCancel != nil {
		a.fetchCancel()
		a.fetchCancel = nil
	}
}

func (a askBar) Update(msg tea.Msg) (askBar, tea.Cmd) {
	switch msg := msg.(type) {
	case askRouteDeltaMsg:
		a.buf += msg.text
		return a, a.waitRoute()
	case askRouteDoneMsg:
		return a.finishRoute(msg.err)
	case askDataMsg:
		if a.fetchCancel != nil { // the fetch completed; release its ctx timer
			a.fetchCancel()
			a.fetchCancel = nil
		}
		if msg.err != nil {
			a.phase = askErrorPhase
			a.notice = "fetch failed: " + msg.err.Error()
			return a, nil
		}
		return a.startAnswer(msg.question, msg.data)
	case askAnswerDeltaMsg:
		a.answer += msg.text
		return a, a.waitAnswer()
	case askAnswerDoneMsg:
		a.stop()
		a.phase = askIdle
		if msg.err != nil {
			a.phase = askErrorPhase
			a.notice = msg.err.Error()
		}
		return a, nil
	case tea.KeyMsg:
		return a.key(msg)
	}
	return a, nil
}

// key folds one keystroke: esc closes (tearing down any stream), enter submits a
// question (gated on a model+VK), and anything else edits the input while idle.
func (a askBar) key(msg tea.KeyMsg) (askBar, tea.Cmd) {
	switch msg.String() {
	case "esc":
		a.stop()
		return a, func() tea.Msg { return askCloseMsg{} }
	case "enter":
		if a.phase == askRouting || a.phase == askAnswering {
			return a, nil
		}
		q := strings.TrimSpace(a.input.Value())
		if q == "" {
			return a, nil
		}
		if !a.ready() {
			a.phase = askErrorPhase
			a.notice = "select a model + Virtual Key (entry wizard) to use Ask"
			return a, nil
		}
		return a.startRoute(q)
	}
	if a.phase == askRouting || a.phase == askAnswering {
		return a, nil
	}
	var cmd tea.Cmd
	a.input, cmd = a.input.Update(msg)
	return a, cmd
}

// startRoute fires the router LLM call: a system prompt enumerating the views +
// the user's question, temperature 0 for routing determinism.
func (a askBar) startRoute(q string) (askBar, tea.Cmd) {
	a.question = q
	a.phase = askRouting
	a.notice, a.answer, a.buf = "", "", ""
	a.input.SetValue("")
	entries := a.entries
	model := a.session.Model
	a.stream = startChatStream(a.gw, a.session.VKSecret, func(context.Context) core.ChatRequest {
		zero := 0.0
		return core.ChatRequest{
			Model:       model,
			Temperature: &zero,
			Messages: []core.ChatMessage{
				{Role: "system", Content: buildRouterPrompt(entries)},
				{Role: "user", Content: q},
			},
		}
	})
	return a, a.waitRoute()
}

// finishRoute parses the accumulated routing reply and dispatches the intent. The
// switch has only navigate / answer / explain / unknown arms — no write path.
func (a askBar) finishRoute(err error) (askBar, tea.Cmd) {
	a.stop()
	if err != nil {
		a.phase = askErrorPhase
		a.notice = "routing failed: " + err.Error()
		return a, nil
	}
	switch in := parseIntent([]byte(a.buf)); in.Action {
	case actionNavigate:
		a.phase = askIdle
		view, filter := in.View, in.Filter
		return a, func() tea.Msg { return navigateMsg{view: view, filter: filter} }
	case actionExplain:
		a.phase = askIdle
		id := in.EventID
		return a, func() tea.Msg { return openEventMsg{id: id, explain: true} }
	case actionAnswer:
		a.phase = askAnswering
		a.answer = ""
		ctx, cancel := fetchCtx()
		a.fetchCancel = cancel
		return a, a.fetchSource(ctx, a.question, in.Source)
	default:
		a.phase = askErrorPhase
		a.notice = "couldn't understand that — try the : palette to navigate"
		return a, nil
	}
}

// fetchSource pulls the read data backing one answer source on a worker cmd. The
// ctx is owned by the bar (a.fetchCancel) so a dismiss/teardown cancels the read
// in flight; the askDataMsg handler releases the ctx timer on completion.
func (a askBar) fetchSource(ctx context.Context, question string, src answerSource) tea.Cmd {
	gw := a.gw
	return func() tea.Msg {
		data, err := fetchAnswerData(ctx, gw, src)
		return askDataMsg{question: question, data: data, err: err}
	}
}

// fetchAnswerData reads the source's backing data and returns it as JSON for the
// summary prompt. It ONLY calls Gateway read methods.
func fetchAnswerData(ctx context.Context, gw Gateway, src answerSource) ([]byte, error) {
	switch src {
	case sourceCost:
		byProv, err := gw.ByProvider(ctx, url.Values{})
		if err != nil {
			return nil, err
		}
		cost, err := gw.Cost(ctx, url.Values{})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"byProvider": byProv, "cost": cost})
	case sourceErrors:
		comp, err := gw.ComplianceOverview(ctx, url.Values{})
		if err != nil {
			return nil, err
		}
		alerts, err := gw.Alerts(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"compliance": comp, "alerts": alerts})
	case sourceSLO:
		lat, err := gw.LatencyPhases(ctx, "provider", url.Values{})
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"latency": lat})
	case sourceFleet:
		nodes, err := gw.Nodes(ctx)
		if err != nil {
			return nil, err
		}
		sync, err := gw.ConfigSyncOutOfSync(ctx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"nodes": nodes, "outOfSync": sync})
	}
	return nil, fmt.Errorf("unknown answer source %q", src)
}

// startAnswer fires the summary LLM call over the fetched data, streamed live.
func (a askBar) startAnswer(question string, data []byte) (askBar, tea.Cmd) {
	a.phase = askAnswering
	model := a.session.Model
	a.stream = startChatStream(a.gw, a.session.VKSecret, func(context.Context) core.ChatRequest {
		return core.ChatRequest{
			Model:    model,
			Messages: []core.ChatMessage{{Role: "user", Content: buildAnswerPrompt(question, data)}},
		}
	})
	return a, a.waitAnswer()
}

func (a *askBar) waitRoute() tea.Cmd {
	return a.stream.wait(
		func(d string) tea.Msg { return askRouteDeltaMsg{text: d} },
		func(sd streamDone) tea.Msg { return askRouteDoneMsg{err: sd.err} },
	)
}

func (a *askBar) waitAnswer() tea.Cmd {
	return a.stream.wait(
		func(d string) tea.Msg { return askAnswerDeltaMsg{text: d} },
		func(sd streamDone) tea.Msg { return askAnswerDoneMsg{err: sd.err} },
	)
}

// View renders the input line plus the current phase's status/answer/error.
func (a askBar) View() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Ask Nexus"))
	b.WriteString("\n")
	b.WriteString(a.input.View())
	b.WriteString("\n")
	switch a.phase {
	case askRouting:
		b.WriteString(styles.TileLabel.Render("routing…"))
	case askAnswering:
		body := a.answer
		if body == "" {
			body = styles.TileLabel.Render("thinking…")
		}
		b.WriteString(body)
	case askErrorPhase:
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + a.notice))
	default:
		switch {
		case a.answer != "":
			b.WriteString(a.answer)
		case a.notice != "":
			b.WriteString(styles.TileLabel.Render(a.notice))
		default:
			b.WriteString(styles.TileLabel.Render("enter to ask · esc to close"))
		}
	}
	return styles.Panel.Render(strings.TrimRight(b.String(), "\n"))
}
