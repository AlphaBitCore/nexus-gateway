package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// generatorBurstSize is how many chats a single `g` press fires. Small enough
// to keep the demo cheap, large enough to make the radar/dashboards visibly
// move.
const generatorBurstSize = 10

// labView is the Simulator/Lab tab: a synthetic traffic generator (fires chats
// to make the radar light up) plus a request lab (crafts one request and runs
// it through the real pipeline via the admin simulator-forward endpoint).
type labView struct {
	gw      Gateway
	session Session

	// generator state.
	genTotal, genOK, genFail int
	genRunning               bool
	genCh                    chan genResultMsg

	// request lab state.
	editor    textarea.Model
	editing   bool
	labResp   string
	labErr    error
	labBusy   bool
	labStatus string

	// route dry-run ("why this route") for the selected model.
	routeRes  *core.RoutingSimulateResult
	routeErr  error
	routeBusy bool
}

type genResultMsg struct {
	ok bool
	ms int
}
type labResultMsg struct {
	body json.RawMessage
	err  error
}
type routeResultMsg struct {
	res *core.RoutingSimulateResult
	err error
}

func newLab(gw Gateway, s Session) *labView {
	ta := textarea.New()
	ta.SetHeight(8)
	ta.SetWidth(60)
	ta.Placeholder = "JSON request body…"
	ta.SetValue(defaultLabBody(s.Model))
	return &labView{gw: gw, session: s, editor: ta}
}

// defaultLabBody seeds the lab editor with a sane chat-completions request so
// the operator can press `r` immediately without typing JSON.
func defaultLabBody(model string) string {
	if model == "" {
		model = "gpt-4o-mini"
	}
	return fmt.Sprintf("{\n  \"model\": %q,\n  \"messages\": [{\"role\": \"user\", \"content\": \"hello\"}],\n  \"max_tokens\": 64\n}", model)
}

func (l *labView) Init() tea.Cmd { return nil }

// capturing is true while the lab editor is focused.
func (l *labView) capturing() bool { return l.editing }

func (l *labView) help() string {
	if l.editing {
		return "esc exit edit · ctrl+s send lab request · tab switch tab"
	}
	return "g generator burst · e edit body · r send lab · t route dry-run · q quit"
}

func (l *labView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case genResultMsg:
		if msg.ok {
			l.genOK++
		} else {
			l.genFail++
		}
		if l.genOK+l.genFail >= l.genTotal {
			l.genRunning = false
			return l, nil
		}
		return l, l.waitGen()
	case labResultMsg:
		l.labBusy = false
		l.labErr = msg.err
		l.labResp = prettyJSON(msg.body)
		return l, nil
	case routeResultMsg:
		l.routeBusy = false
		l.routeErr = msg.err
		l.routeRes = msg.res
		return l, nil
	case tea.KeyMsg:
		if l.editing {
			return l.updateEditing(msg)
		}
		switch msg.String() {
		case "g":
			if !l.ready() || l.genRunning {
				return l, nil
			}
			return l, l.fireGenerator()
		case "r":
			if !l.ready() || l.labBusy {
				return l, nil
			}
			return l, l.sendLab()
		case "t":
			if l.session.Model == "" || l.routeBusy {
				return l, nil
			}
			return l, l.sendRoute()
		case "e":
			l.editing = true
			return l, l.editor.Focus()
		}
	}
	return l, nil
}

// sendRoute runs the routing dry-run for the selected model (no real request).
func (l *labView) sendRoute() tea.Cmd {
	l.routeBusy = true
	l.routeErr = nil
	l.routeRes = nil
	model := l.session.Model
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := l.gw.RoutingSimulate(ctx, core.RoutingSimulateRequest{ModelID: model, EndpointType: "chat"})
		return routeResultMsg{res: res, err: err}
	}
}

// updateEditing handles keys while the lab editor is focused.
func (l *labView) updateEditing(msg tea.KeyMsg) (viewModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		l.editing = false
		l.editor.Blur()
		return l, nil
	case "ctrl+s":
		l.editing = false
		l.editor.Blur()
		if !l.ready() || l.labBusy {
			return l, nil
		}
		return l, l.sendLab()
	}
	var cmd tea.Cmd
	l.editor, cmd = l.editor.Update(msg)
	return l, cmd
}

func (l *labView) ready() bool {
	return l.session.Model != "" && strings.TrimSpace(l.session.VKSecret) != ""
}

// fireGenerator launches generatorBurstSize chat requests in parallel and sets
// up the wait loop that aggregates their results into the visible counters.
func (l *labView) fireGenerator() tea.Cmd {
	l.genTotal, l.genOK, l.genFail = generatorBurstSize, 0, 0
	l.genRunning = true
	l.genCh = make(chan genResultMsg, generatorBurstSize)
	for i := 0; i < generatorBurstSize; i++ {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), chatStreamTimeout)
			defer cancel()
			start := time.Now()
			_, err := l.gw.ChatStream(ctx, l.session.VKSecret,
				core.ChatRequest{
					Model:    l.session.Model,
					Messages: []core.ChatMessage{{Role: "user", Content: "ping"}},
				}, nil)
			l.genCh <- genResultMsg{ok: err == nil, ms: int(time.Since(start).Milliseconds())}
		}()
	}
	return l.waitGen()
}

// waitGen reads one generator result and re-issues itself so subsequent
// completions keep updating the view.
func (l *labView) waitGen() tea.Cmd {
	ch := l.genCh
	return func() tea.Msg { return <-ch }
}

// sendLab parses the editor body, fires SimulatorForward, and turns the result
// into a labResultMsg.
func (l *labView) sendLab() tea.Cmd {
	body := strings.TrimSpace(l.editor.Value())
	if !json.Valid([]byte(body)) {
		return func() tea.Msg {
			return labResultMsg{err: fmt.Errorf("editor body is not valid JSON")}
		}
	}
	l.labBusy = true
	l.labResp = ""
	l.labErr = nil
	l.labStatus = ""
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), chatStreamTimeout)
		defer cancel()
		raw, err := l.gw.SimulatorForward(ctx, core.SimulatorForwardRequest{
			Path:   "/v1/chat/completions",
			Method: "POST",
			VK:     l.session.VKSecret,
			Body:   json.RawMessage(body),
		})
		return labResultMsg{body: raw, err: err}
	}
}

func (l *labView) View(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Synthetic generator"))
	b.WriteString("\n")
	b.WriteString(l.genStatus())
	b.WriteString("\n\n")
	b.WriteString(styles.TileValue.Render("Request lab (admin-authed forward)"))
	b.WriteString("\n")
	// The request lab + generator need a VK (real traffic); the route dry-run
	// needs only a model, so it renders regardless.
	if !l.ready() {
		b.WriteString(styles.TileLabel.Render("Select a model + Virtual Key before using the generator/lab."))
	} else {
		b.WriteString(l.editor.View())
		b.WriteString("\n")
		if l.labBusy {
			b.WriteString(styles.TileLabel.Render("sending…"))
		} else if l.labErr != nil {
			b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + l.labErr.Error()))
		} else if l.labResp != "" {
			b.WriteString(styles.TileLabel.Render("response:"))
			b.WriteString("\n")
			b.WriteString(l.labResp)
		}
	}
	b.WriteString("\n\n")
	b.WriteString(l.routePanel())
	return b.String()
}

// routePanel renders the routing dry-run ("why this route") for the selected
// model — which provider/model a request resolves to, plus any warnings.
func (l *labView) routePanel() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Route dry-run (t)"))
	b.WriteString("\n")
	switch {
	case l.session.Model == "":
		b.WriteString(styles.TileLabel.Render("select a model to dry-run its route"))
	case l.routeBusy:
		b.WriteString(styles.TileLabel.Render("resolving…"))
	case l.routeErr != nil:
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + l.routeErr.Error()))
	case l.routeRes != nil:
		r := l.routeRes
		if r.Substituted {
			b.WriteString(styles.TileLabel.Render(fmt.Sprintf("%s → substituted by rule %q\n", l.session.Model, r.RuleName)))
		} else {
			b.WriteString(styles.TileLabel.Render(l.session.Model + " → no rule substitution\n"))
		}
		for _, t := range r.Targets {
			b.WriteString(fmt.Sprintf("  %s → %s\n", t.ProviderName, t.ModelCode))
		}
		for _, w := range r.Warnings {
			b.WriteString(lipgloss.NewStyle().Foreground(styles.Amber).Render("  ⚠ " + w + "\n"))
		}
	default:
		b.WriteString(styles.TileLabel.Render("press t to resolve the route for " + dash(l.session.Model)))
	}
	return b.String()
}

func (l *labView) genStatus() string {
	if !l.genRunning && l.genTotal == 0 {
		return styles.TileLabel.Render(fmt.Sprintf("press g to fire %d-request burst against %s", generatorBurstSize, dash(l.session.Model)))
	}
	state := "DONE"
	color := styles.Green
	if l.genRunning {
		state = "running"
		color = styles.Brand
	}
	if l.genFail > 0 && !l.genRunning {
		color = styles.Amber
	}
	badge := lipgloss.NewStyle().Bold(true).Foreground(color).Render(state)
	return badge + styles.TileLabel.Render(fmt.Sprintf("  %d/%d (ok %d, failed %d)",
		l.genOK+l.genFail, l.genTotal, l.genOK, l.genFail))
}

// prettyJSON returns indented JSON when body parses, or the raw string when it
// does not (e.g. an upstream error envelope from the gateway).
func prettyJSON(body json.RawMessage) string {
	if len(body) == 0 {
		return ""
	}
	var any interface{}
	if err := json.Unmarshal(body, &any); err != nil {
		return string(body)
	}
	out, err := json.MarshalIndent(any, "", "  ")
	if err != nil {
		return string(body)
	}
	return string(out)
}
