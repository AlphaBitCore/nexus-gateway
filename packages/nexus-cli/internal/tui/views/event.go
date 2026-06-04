package views

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"image/color"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// explainPromptMax bounds the normalized text fed to the model so a huge body
// can't blow the prompt budget.
const explainPromptMax = 4000

// EventView shows one traffic event: key fields, the latency-phase waterfall,
// the hook decisions, the request/response bodies (b cycles), and an on-demand
// LLM explanation (x).
type EventView struct {
	gw      kit.Gateway
	session kit.Session
	id      string
	ev      *core.TrafficEvent
	err     error
	loading bool

	bodyMode int // 0 none · 1 request · 2 response

	explaining  bool
	autoExplain bool // armed by an Ask-Nexus explain intent; fires once the event loads
	explainText string
	explainErr  error
	stream      *kit.ChatStreamer

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

func newEvent(gw kit.Gateway, s kit.Session) *EventView { return &EventView{gw: gw, session: s} }

// setSession follows a runtime chat-model switch (sessionSetter).
func (v *EventView) SetSession(s kit.Session) { v.session = s }

// SetID points the view at a new event id (called when opened from the Radar).
// Any in-flight explanation is cancelled so navigating away never leaks the
// stream goroutine or holds the upstream connection.
func (e *EventView) SetID(id string) {
	e.stream.Stop()
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

// SetIDExplain points the view at id and arms a one-shot auto-explain that fires
// when the event finishes loading (the Ask-Nexus "explain" intent). startExplain
// needs the loaded event, so it cannot run until the eventMsg arrives.
func (e *EventView) SetIDExplain(id string) {
	e.SetID(id)
	e.autoExplain = true
}

func (e *EventView) Init() tea.Cmd {
	if e.id == "" {
		return nil
	}
	return e.fetch()
}

// leave cancels an in-flight explanation stream when the operator navigates
// away, so a mid-explain tab-switch never leaks the goroutine + connection.
func (e *EventView) Leave() {
	e.stream.Stop()
	e.explaining = false
}

func (e *EventView) Help() string {
	if e.id == "" || e.ev == nil {
		return "↑/↓ in Radar then enter to inspect · q quit"
	}
	return "b cycle bodies · x explain (LLM) · r replay · ←/esc back · q quit"
}

// crumb contributes the open event id to the breadcrumb trail so a drilled event
// reads "nexus › Radar › ev-9a3f" rather than the static "Event" label.
func (e *EventView) Crumb() string {
	if e.id == "" {
		return ""
	}
	return e.id
}

func (e *EventView) fetch() tea.Cmd {
	id := e.id
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		ev, err := e.gw.TrafficEvent(ctx, id)
		return eventMsg{ev: ev, err: err}
	}
}

func (e *EventView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
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
		e.replayResp = kit.PrettyJSON(msg.body)
	case tea.KeyPressMsg:
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
func (e *EventView) startReplay() tea.Cmd {
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
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		raw, err := gw.SimulatorForward(ctx, core.SimulatorForwardRequest{
			Path: path, Method: "POST", VK: vk, Body: body,
		})
		return replayMsg{body: raw, err: err}
	}
}

// startExplain streams an LLM diagnosis of the event over the normalized view.
func (e *EventView) startExplain() tea.Cmd {
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
	e.stream = kit.StartChatStream(gw, e.session.VKSecret, func(ctx context.Context) core.ChatRequest {
		norm, _ := gw.TrafficEventNormalized(ctx, id)
		return core.ChatRequest{
			Model:    model,
			Messages: []core.ChatMessage{{Role: "user", Content: buildExplainPrompt(ev, norm)}},
		}
	})
	return e.waitExplain()
}

// waitExplain drains the next delta (or the terminal done) into explain messages.
func (e *EventView) waitExplain() tea.Cmd {
	return e.stream.Wait(
		func(d string) tea.Msg { return explainDeltaMsg{text: d} },
		func(sd kit.StreamDone) tea.Msg { return explainDoneMsg{err: sd.Err} },
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
		ev.StatusCode, kit.Dash(ev.RequestHookDecision), kit.Dash(ev.RequestHookReason),
		kit.Dash(ev.ResponseHookDecision), kit.Dash(ev.ResponseHookReason), n)
}

func (e *EventView) View(width, height int) string {
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
		ev.TotalTokens, ev.PromptTokens, ev.CompletionTok, ev.EstCostUSD, kit.Dash(ev.CacheStatus))
	fmt.Fprintf(&b, "trace  %s\n", kit.Dash(ev.TraceID))
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
		b.WriteString(e.explainPanel(width))
	}
	if e.replayBusy || e.replayResp != "" || e.replayErr != nil {
		b.WriteString("\n\n")
		b.WriteString(e.replayPanel())
	}
	return b.String()
}

// replayPanel renders the result of re-firing the captured request.
func (e *EventView) replayPanel() string {
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
func (e *EventView) hooksPanel() string {
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
func (e *EventView) bodyPanel() string {
	switch e.bodyMode {
	case 1:
		return styles.TileValue.Render("Request body") + "\n" + kit.PrettyJSON(e.ev.RequestBody)
	case 2:
		return styles.TileValue.Render("Response body") + "\n" + kit.PrettyJSON(e.ev.ResponseBody)
	default:
		return ""
	}
}

// explainPanel renders the streamed LLM explanation, word-wrapped to the view
// width so a long diagnosis reads as a paragraph instead of one clipped line.
func (e *EventView) explainPanel(width int) string {
	head := styles.TileValue.Render("Explanation")
	if e.explainErr != nil {
		return head + "\n" + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+e.explainErr.Error())
	}
	if e.explaining && e.explainText == "" {
		return head + "\n" + styles.TileLabel.Render("thinking…")
	}
	return head + "\n" + kit.WrapText(e.explainText, width)
}

// hookOutcome canonicalizes a hook decision into one of block | redact | allow.
// It is the single source of truth for hook-decision classification, shared by
// the event view (color) and the Radar badges.
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
func hookDecisionColor(decision string) color.Color {
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

// ID returns the open traffic-event id (set by the shell's drill / show drives).
func (e *EventView) ID() string { return e.id }

// AutoExplain reports whether the view will auto-run the explain stream on open
// (armed by the explain drill drive).
func (e *EventView) AutoExplain() bool { return e.autoExplain }
