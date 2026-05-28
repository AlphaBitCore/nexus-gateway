package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// explainPromptMax bounds the normalized text fed to the model so a huge body
// can't blow the prompt budget.
const explainPromptMax = 4000

// eventView shows one traffic event: key fields, the latency-phase waterfall,
// the hook decisions, the request/response bodies (b cycles), and an on-demand
// LLM explanation (x).
type eventView struct {
	gw      Gateway
	session Session
	id      string
	ev      *core.TrafficEvent
	err     error
	loading bool

	bodyMode int // 0 none · 1 request · 2 response

	explaining  bool
	autoExplain bool // armed by an Ask-Nexus explain intent; fires once the event loads
	explainText string
	explainErr  error
	stream      *chatStreamer

	replayBusy bool
	replayResp string
	replayErr  error
}

type eventMsg struct {
	ev  *core.TrafficEvent
	err error
}
type explainDeltaMsg struct{ text string }
type explainDoneMsg struct{ err error }
type replayMsg struct {
	body json.RawMessage
	err  error
}

func newEvent(gw Gateway, s Session) *eventView { return &eventView{gw: gw, session: s} }

// setID points the view at a new event id (called when opened from the radar).
// Any in-flight explanation is cancelled so navigating away never leaks the
// stream goroutine or holds the upstream connection.
func (e *eventView) setID(id string) {
	e.stream.stop()
	e.id = id
	e.ev = nil
	e.err = nil
	e.loading = true
	e.bodyMode = 0
	e.explaining = false
	e.autoExplain = false
	e.explainText = ""
	e.explainErr = nil
	e.replayBusy = false
	e.replayResp = ""
	e.replayErr = nil
}

// setIDExplain points the view at id and arms a one-shot auto-explain that fires
// when the event finishes loading (the Ask-Nexus "explain" intent). startExplain
// needs the loaded event, so it cannot run until the eventMsg arrives.
func (e *eventView) setIDExplain(id string) {
	e.setID(id)
	e.autoExplain = true
}

func (e *eventView) Init() tea.Cmd {
	if e.id == "" {
		return nil
	}
	return e.fetch()
}

// leave cancels an in-flight explanation stream when the operator navigates
// away, so a mid-explain tab-switch never leaks the goroutine + connection.
func (e *eventView) leave() {
	e.stream.stop()
	e.explaining = false
}

func (e *eventView) help() string {
	if e.id == "" || e.ev == nil {
		return "↑/↓ in radar then enter to inspect · q quit"
	}
	return "b cycle bodies · x explain (LLM) · r replay · tab switch · q quit"
}

func (e *eventView) fetch() tea.Cmd {
	id := e.id
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		ev, err := e.gw.TrafficEvent(ctx, id)
		return eventMsg{ev: ev, err: err}
	}
}

func (e *eventView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case eventMsg:
		e.loading = false
		e.ev = msg.ev
		e.err = msg.err
		if e.autoExplain && e.ev != nil && !e.explaining {
			e.autoExplain = false
			return e, e.startExplain()
		}
	case explainDeltaMsg:
		e.explainText += msg.text
		return e, e.waitExplain()
	case explainDoneMsg:
		e.explaining = false
		if msg.err != nil {
			e.explainErr = msg.err
		}
	case replayMsg:
		e.replayBusy = false
		e.replayErr = msg.err
		e.replayResp = prettyJSON(msg.body)
	case tea.KeyMsg:
		switch msg.String() {
		case "b":
			if e.ev != nil {
				e.bodyMode = (e.bodyMode + 1) % 3
			}
		case "x":
			if e.ev != nil && !e.explaining {
				return e, e.startExplain()
			}
		case "r":
			if e.ev != nil && !e.replayBusy {
				return e, e.startReplay()
			}
		}
	}
	return e, nil
}

// startReplay re-fires the captured request through the real gateway pipeline
// (via the admin simulator) under the operator's own VK, to reproduce a result.
func (e *eventView) startReplay() tea.Cmd {
	if strings.TrimSpace(e.session.VKSecret) == "" {
		e.replayErr = fmt.Errorf("select a Virtual Key (entry wizard) to replay")
		return nil
	}
	if len(e.ev.RequestBody) == 0 {
		e.replayErr = fmt.Errorf("this event has no captured request body to replay")
		return nil
	}
	e.replayBusy = true
	e.replayResp = ""
	e.replayErr = nil
	path, body, vk, gw := e.ev.Path, e.ev.RequestBody, e.session.VKSecret, e.gw
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		raw, err := gw.SimulatorForward(ctx, core.SimulatorForwardRequest{
			Path: path, Method: "POST", VK: vk, Body: body,
		})
		return replayMsg{body: raw, err: err}
	}
}

// startExplain streams an LLM diagnosis of the event over the normalized view.
func (e *eventView) startExplain() tea.Cmd {
	if strings.TrimSpace(e.session.VKSecret) == "" || e.session.Model == "" {
		e.explainErr = fmt.Errorf("select a model + Virtual Key (entry wizard) to enable explain")
		return nil
	}
	e.explaining = true
	e.explainText = ""
	e.explainErr = nil
	id, ev, gw, model := e.id, e.ev, e.gw, e.session.Model
	// build runs on the stream goroutine: fetch the normalized event, then frame
	// the prompt — no blocking work happens in Update.
	e.stream = startChatStream(gw, e.session.VKSecret, func(ctx context.Context) core.ChatRequest {
		norm, _ := gw.TrafficEventNormalized(ctx, id)
		return core.ChatRequest{
			Model:    model,
			Messages: []core.ChatMessage{{Role: "user", Content: buildExplainPrompt(ev, norm)}},
		}
	})
	return e.waitExplain()
}

// waitExplain drains the next delta (or the terminal done) into explain messages.
func (e *eventView) waitExplain() tea.Cmd {
	return e.stream.wait(
		func(d string) tea.Msg { return explainDeltaMsg{text: d} },
		func(sd streamDone) tea.Msg { return explainDoneMsg{err: sd.err} },
	)
}

// buildExplainPrompt frames the event + normalized text for the model.
func buildExplainPrompt(ev *core.TrafficEvent, norm []byte) string {
	n := string(norm)
	if len(n) > explainPromptMax {
		n = n[:explainPromptMax]
	}
	return fmt.Sprintf(
		"You are an SRE assistant. In 3-4 sentences explain why this AI gateway traffic event "+
			"ended with HTTP %d (request-hook=%s/%s, response-hook=%s/%s). Be concrete.\n\nNormalized event:\n%s",
		ev.StatusCode, dash(ev.RequestHookDecision), dash(ev.RequestHookReason),
		dash(ev.ResponseHookDecision), dash(ev.ResponseHookReason), n)
}

func (e *eventView) View(width, height int) string {
	if e.id == "" {
		return styles.TileLabel.Render("Select an event in the Radar (↑/↓ then enter) to inspect it here.")
	}
	if e.loading {
		return styles.TileLabel.Render("loading event " + e.id + "…")
	}
	if e.err != nil {
		return lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + e.err.Error())
	}
	if e.ev == nil {
		return styles.TileLabel.Render("no event")
	}
	ev := e.ev
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", styles.TileValue.Render("event"), ev.ID)
	status := lipgloss.NewStyle().Foreground(styles.StatusColor(ev.StatusCode)).Render(fmt.Sprintf("%d", ev.StatusCode))
	fmt.Fprintf(&b, "status %s   model %s (%s)\n", status, ev.ModelName, ev.ProviderName)
	fmt.Fprintf(&b, "tokens %d (prompt %d / completion %d)   cost $%.6f   cache %s\n",
		ev.TotalTokens, ev.PromptTokens, ev.CompletionTok, ev.EstCostUSD, dash(ev.CacheStatus))
	fmt.Fprintf(&b, "trace  %s\n", dash(ev.TraceID))
	b.WriteString(e.hooksPanel())
	b.WriteString("\n")
	b.WriteString(styles.TileValue.Render("Latency waterfall"))
	b.WriteString("\n")
	b.WriteString(latencyWaterfall(ev))
	if body := e.bodyPanel(); body != "" {
		b.WriteString("\n\n")
		b.WriteString(body)
	}
	if e.explaining || e.explainText != "" || e.explainErr != nil {
		b.WriteString("\n\n")
		b.WriteString(e.explainPanel())
	}
	if e.replayBusy || e.replayResp != "" || e.replayErr != nil {
		b.WriteString("\n\n")
		b.WriteString(e.replayPanel())
	}
	return b.String()
}

// replayPanel renders the result of re-firing the captured request.
func (e *eventView) replayPanel() string {
	head := styles.TileValue.Render("Replay")
	switch {
	case e.replayBusy:
		return head + "\n" + styles.TileLabel.Render("re-firing…")
	case e.replayErr != nil:
		return head + "\n" + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+e.replayErr.Error())
	default:
		return head + "\n" + e.replayResp
	}
}

// hooksPanel renders the request/response hook decisions, colored by outcome.
func (e *eventView) hooksPanel() string {
	ev := e.ev
	if ev.RequestHookDecision == "" && ev.ResponseHookDecision == "" {
		return styles.TileLabel.Render("hooks  (none recorded)") + "\n"
	}
	line := func(label, decision, reason string) string {
		if decision == "" {
			return ""
		}
		c := hookDecisionColor(decision)
		tag := lipgloss.NewStyle().Bold(true).Foreground(c).Render(strings.ToUpper(decision))
		out := fmt.Sprintf("  %-12s %s", label, tag)
		if reason != "" {
			out += styles.TileLabel.Render("  " + reason)
		}
		return out + "\n"
	}
	return styles.TileValue.Render("Hooks") + "\n" +
		line("request", ev.RequestHookDecision, ev.RequestHookReason) +
		line("response", ev.ResponseHookDecision, ev.ResponseHookReason)
}

// bodyPanel renders the request or response body per the current bodyMode.
func (e *eventView) bodyPanel() string {
	switch e.bodyMode {
	case 1:
		return styles.TileValue.Render("Request body") + "\n" + prettyJSON(e.ev.RequestBody)
	case 2:
		return styles.TileValue.Render("Response body") + "\n" + prettyJSON(e.ev.ResponseBody)
	default:
		return ""
	}
}

// explainPanel renders the streamed LLM explanation.
func (e *eventView) explainPanel() string {
	head := styles.TileValue.Render("Explanation")
	if e.explainErr != nil {
		return head + "\n" + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+e.explainErr.Error())
	}
	body := e.explainText
	if e.explaining && body == "" {
		body = styles.TileLabel.Render("thinking…")
	}
	return head + "\n" + body
}

// hookOutcome canonicalizes a hook decision into one of block | redact | allow.
// It is the single source of truth for hook-decision classification, shared by
// the event view (color) and the radar badges.
func hookOutcome(decision string) string {
	switch strings.ToLower(decision) {
	case "block", "deny", "reject":
		return "block"
	case "redact", "warn", "flag":
		return "redact"
	default:
		return "allow"
	}
}

// hookDecisionColor maps a hook decision to a RAG color.
func hookDecisionColor(decision string) lipgloss.Color {
	switch hookOutcome(decision) {
	case "block":
		return styles.Red
	case "redact":
		return styles.Amber
	default:
		return styles.Green
	}
}

// latencyWaterfall renders proportional bars for each latency phase.
func latencyWaterfall(ev *core.TrafficEvent) string {
	phases := []struct {
		name string
		ms   int
	}{
		{"req hooks", ev.RequestHooksMs},
		{"upstream ttfb", ev.UpstreamTTFBMs},
		{"upstream total", ev.UpstreamTotMs},
		{"resp hooks", ev.RespHooksMs},
	}
	max := ev.LatencyMs
	for _, p := range phases {
		if p.ms > max {
			max = p.ms
		}
	}
	if max <= 0 {
		return styles.TileLabel.Render("  (no latency data)")
	}
	const barWidth = 40
	var lines []string
	for _, p := range phases {
		filled := p.ms * barWidth / max
		bar := lipgloss.NewStyle().Foreground(styles.Brand).Render(strings.Repeat("█", filled))
		lines = append(lines, fmt.Sprintf("  %-15s %s %dms", p.name, bar, p.ms))
	}
	lines = append(lines, fmt.Sprintf("  %-15s %s %dms",
		styles.TileValue.Render("total"), "", ev.LatencyMs))
	return strings.Join(lines, "\n")
}

// dash renders "—" for empty strings.
func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
