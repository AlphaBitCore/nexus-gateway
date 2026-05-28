package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// chatStreamTimeout bounds one streamed turn so a stuck upstream cannot hang
// the view forever (the ctx is cancelled when the timeout fires).
const chatStreamTimeout = 2 * time.Minute

// chatTurn is one rendered message in the solo transcript.
type chatTurn struct {
	role      string // "user" | "assistant" | "system"
	text      string
	usage     *core.ChatUsage
	latencyMs int
}

// sideResult is one model's answer within an A/B comparison round.
type sideResult struct {
	model     string
	text      string
	usage     *core.ChatUsage
	latencyMs int
	err       error
}

// compareRound is one A/B comparison: a prompt fanned out to two models.
type compareRound struct {
	prompt string
	a, b   sideResult
}

// chat is the Chat Playground: a VK-authed SSE chat against the selected model.
// With `/compare <model>` it enters A/B mode — each prompt fans out to the
// session model (A) and the chosen model (B) concurrently under the same VK,
// rendered side by side with a latency verdict. `/solo` returns to single-model.
type chat struct {
	gw      Gateway
	session Session

	input textinput.Model
	turns []chatTurn // solo-mode transcript

	compareModel string         // model B; "" → solo mode
	rounds       []compareRound // A/B-mode transcript
	notice       string         // transient slash-command feedback

	system      string                // optional system prompt (prepended at send)
	temperature *float64              // nil → model default
	pricing     map[string]modelPrice // model code → per-million pricing (catalog)
	sessionCost float64               // running estimated spend this session

	streaming bool
	stream    *chatStreamer // solo stream, or A/B side A
	streamB   *chatStreamer // A/B side B
	pending   int           // A/B streams still running
	startAt   time.Time
}

// modelPrice is the per-million-token input/output price used to estimate the
// running session cost from each turn's usage.
type modelPrice struct{ in, out float64 }

type chatDeltaMsg struct {
	side string // "" solo, "A"/"B" compare
	text string
}
type chatDoneMsg struct {
	side  string
	usage *core.ChatUsage
	err   error
}

// chatPricingMsg delivers the catalog pricing map (best-effort; cost stays 0 if
// the catalog is unavailable).
type chatPricingMsg struct{ pricing map[string]modelPrice }

func newChat(gw Gateway, s Session) *chat {
	ti := textinput.New()
	ti.Placeholder = "Type a message and press enter…  (/compare <model> for A/B)"
	ti.CharLimit = 4000
	ti.Width = 60
	ti.Focus()
	return &chat{gw: gw, session: s, input: ti}
}

func (c *chat) Init() tea.Cmd {
	if c.pricing == nil {
		return tea.Batch(textinput.Blink, c.fetchPricing())
	}
	return textinput.Blink
}

// fetchPricing loads the model catalog once and builds a code→price map so the
// running cost can be estimated from each turn's usage. Best-effort: an error
// leaves pricing nil (cost stays 0 and Init retries on the next visit).
func (c *chat) fetchPricing() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		cat, err := c.gw.AdminModels(ctx)
		if err != nil || cat == nil {
			return chatPricingMsg{}
		}
		m := make(map[string]modelPrice)
		for _, g := range cat.Data {
			for _, mod := range g.Models {
				m[mod.Code] = modelPrice{in: mod.InputPricePerMillion, out: mod.OutputPricePerMillion}
			}
		}
		return chatPricingMsg{pricing: m}
	}
}

// costOf estimates the spend of one turn from its usage and the model's catalog
// price. Returns 0 when usage or pricing is unavailable.
func (c *chat) costOf(model string, u *core.ChatUsage) float64 {
	if u == nil {
		return 0
	}
	p, ok := c.pricing[model]
	if !ok {
		return 0
	}
	return float64(u.PromptTokens)*p.in/1e6 + float64(u.CompletionTokens)*p.out/1e6
}

// capturing tells the root model to suspend single-letter shortcuts while the
// prompt is focused. While streaming, the input is blurred and tab nav works.
func (c *chat) capturing() bool { return c.input.Focused() }

// leave cancels any in-flight solo or A/B stream when the operator navigates
// away, so a mid-stream tab-switch never leaks the goroutine + connection. It
// resets streaming state and refocuses the input so re-entry is usable (the
// terminal done frame is routed to the now-active view and harmlessly ignored).
func (c *chat) leave() {
	c.stream.stop()
	c.streamB.stop()
	if c.streaming {
		c.streaming = false
		c.pending = 0
		c.input.Focus()
	}
}

func (c *chat) help() string {
	if c.streaming {
		return "streaming… (tab to leave · ctrl+c to quit)"
	}
	if !c.ready() {
		return "no model/VK selected — switch tabs to set one"
	}
	if c.compareModel != "" {
		return "A/B on · enter compares · /solo to exit · tab switch · ctrl+c quit"
	}
	return "enter send · /help for commands · tab switch · ctrl+c quit"
}

// ready reports whether the session carries enough to talk to a model.
func (c *chat) ready() bool {
	return c.session.Model != "" && strings.TrimSpace(c.session.VKSecret) != ""
}

func (c *chat) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case chatPricingMsg:
		if msg.pricing != nil {
			c.pricing = msg.pricing
		}
		return c, nil
	case chatDeltaMsg:
		if msg.side == "" {
			c.appendAssistant(msg.text)
			return c, c.waitDelta()
		}
		c.appendCompare(msg.side, msg.text)
		return c, c.waitSide(msg.side)
	case chatDoneMsg:
		if msg.side == "" {
			c.finishTurn(msg)
			return c, nil
		}
		c.finishSide(msg)
		return c, nil
	case tea.KeyMsg:
		if c.streaming {
			return c, nil // ignore typing while streaming
		}
		if msg.Type == tea.KeyEnter {
			val := strings.TrimSpace(c.input.Value())
			switch {
			case strings.HasPrefix(val, "/"):
				return c, c.runCommand(val)
			case val != "" && c.ready():
				return c, c.send(val)
			}
			// empty or not-ready: fall through to the input (a harmless no-op).
		}
		var cmd tea.Cmd
		c.input, cmd = c.input.Update(msg)
		return c, cmd
	}
	return c, nil
}

// runCommand handles the slash commands typed into the prompt. It always
// consumes the input and reports outcome via the notice line.
func (c *chat) runCommand(val string) tea.Cmd {
	c.input.SetValue("")
	c.notice = ""
	fields := strings.Fields(val)
	switch fields[0] {
	case "/compare", "/ab":
		if len(fields) < 2 {
			c.notice = "usage: /compare <model-code>"
			return nil
		}
		c.compareModel = fields[1]
		c.notice = "A/B on — next prompt compares " + dash(c.session.Model) + " vs " + fields[1]
	case "/solo":
		c.compareModel = ""
		c.notice = "solo mode"
	case "/system":
		c.system = strings.TrimSpace(strings.TrimPrefix(val, fields[0]))
		if c.system == "" {
			c.notice = "system prompt cleared"
		} else {
			c.notice = "system prompt set"
		}
	case "/temp":
		if len(fields) < 2 {
			c.temperature = nil
			c.notice = "temperature: model default"
			return nil
		}
		t, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || t < 0 || t > 2 {
			c.notice = "usage: /temp <0.0–2.0>"
			return nil
		}
		c.temperature = &t
		c.notice = fmt.Sprintf("temperature %.2f", t)
	case "/clear", "/reset":
		c.turns = nil
		c.rounds = nil
		c.sessionCost = 0
		c.notice = "conversation cleared"
	case "/help", "/?":
		c.notice = "/compare <model> · /solo · /system <text> · /temp <0-2> · /clear"
	default:
		c.notice = "unknown command " + fields[0] + " — /help for commands"
	}
	return nil
}

// send dispatches a prompt to solo or A/B mode.
func (c *chat) send(prompt string) tea.Cmd {
	c.notice = ""
	if c.compareModel != "" {
		return c.sendCompare(prompt)
	}
	return c.sendSolo(prompt)
}

// sendSolo freezes the input, appends user + empty assistant turns, and kicks
// off the streaming goroutine + the wait command that drains its channels.
func (c *chat) sendSolo(prompt string) tea.Cmd {
	c.input.SetValue("")
	c.input.Blur()
	c.turns = append(c.turns, chatTurn{role: "user", text: prompt})
	c.turns = append(c.turns, chatTurn{role: "assistant"})

	c.streaming = true
	c.startAt = time.Now()
	req := core.ChatRequest{
		Model:       c.session.Model,
		Messages:    c.withSystem(messagesFromTurns(c.turns)),
		Temperature: c.temperature,
	}
	c.stream = startChatStream(c.gw, c.session.VKSecret, func(context.Context) core.ChatRequest { return req })
	return c.waitDelta()
}

// withSystem prepends the configured system prompt (if any) to a message list.
func (c *chat) withSystem(msgs []core.ChatMessage) []core.ChatMessage {
	if strings.TrimSpace(c.system) == "" {
		return msgs
	}
	return append([]core.ChatMessage{{Role: "system", Content: c.system}}, msgs...)
}

// sendCompare fans the prompt out to the session model (A) and the compare
// model (B) concurrently under the same VK, accumulating both answers into a
// new round. Both run as independent streams; the round finishes when both end.
func (c *chat) sendCompare(prompt string) tea.Cmd {
	c.input.SetValue("")
	c.input.Blur()
	c.rounds = append(c.rounds, compareRound{
		prompt: prompt,
		a:      sideResult{model: c.session.Model},
		b:      sideResult{model: c.compareModel},
	})
	c.streaming = true
	c.pending = 2
	c.startAt = time.Now()
	msgs := c.withSystem([]core.ChatMessage{{Role: "user", Content: prompt}})
	reqA := core.ChatRequest{Model: c.session.Model, Messages: msgs, Temperature: c.temperature}
	reqB := core.ChatRequest{Model: c.compareModel, Messages: msgs, Temperature: c.temperature}
	c.stream = startChatStream(c.gw, c.session.VKSecret, func(context.Context) core.ChatRequest { return reqA })
	c.streamB = startChatStream(c.gw, c.session.VKSecret, func(context.Context) core.ChatRequest { return reqB })
	return tea.Batch(c.waitSide("A"), c.waitSide("B"))
}

// waitDelta drains the next solo delta (or the terminal done) into messages.
func (c *chat) waitDelta() tea.Cmd {
	return c.stream.wait(
		func(d string) tea.Msg { return chatDeltaMsg{text: d} },
		func(sd streamDone) tea.Msg { return chatDoneMsg{usage: sd.usage, err: sd.err} },
	)
}

// waitSide drains the next delta/done for one A/B side, tagging it.
func (c *chat) waitSide(side string) tea.Cmd {
	s := c.stream
	if side == "B" {
		s = c.streamB
	}
	return s.wait(
		func(d string) tea.Msg { return chatDeltaMsg{side: side, text: d} },
		func(sd streamDone) tea.Msg { return chatDoneMsg{side: side, usage: sd.usage, err: sd.err} },
	)
}

// appendAssistant grows the in-flight solo assistant turn by one delta.
func (c *chat) appendAssistant(d string) {
	if n := len(c.turns); n > 0 && c.turns[n-1].role == "assistant" {
		c.turns[n-1].text += d
	}
}

// appendCompare grows one A/B side's answer in the latest round by one delta.
func (c *chat) appendCompare(side, d string) {
	n := len(c.rounds)
	if n == 0 {
		return
	}
	if side == "B" {
		c.rounds[n-1].b.text += d
	} else {
		c.rounds[n-1].a.text += d
	}
}

// finishTurn closes the solo streaming state, attaches usage/latency to the
// last assistant turn, and refocuses the input for the next prompt.
func (c *chat) finishTurn(m chatDoneMsg) {
	c.streaming = false
	c.stream.stop()
	latency := int(time.Since(c.startAt).Milliseconds())
	if n := len(c.turns); n > 0 && c.turns[n-1].role == "assistant" {
		c.turns[n-1].usage = m.usage
		c.turns[n-1].latencyMs = latency
		if m.err != nil {
			c.turns[n-1].text += "\n⚠ " + m.err.Error()
		}
	}
	c.sessionCost += c.costOf(c.session.Model, m.usage)
	c.input.Focus()
}

// finishSide records one A/B side's terminal result; when both sides are done
// the round closes, the streams are torn down, and the input refocuses.
func (c *chat) finishSide(m chatDoneMsg) {
	n := len(c.rounds)
	if n == 0 {
		return
	}
	res := &c.rounds[n-1].a
	if m.side == "B" {
		res = &c.rounds[n-1].b
	}
	res.usage = m.usage
	res.latencyMs = int(time.Since(c.startAt).Milliseconds())
	res.err = m.err
	c.sessionCost += c.costOf(res.model, m.usage)
	c.pending--
	if c.pending <= 0 {
		c.streaming = false
		c.stream.stop()
		c.streamB.stop()
		c.input.Focus()
	}
}

// messagesFromTurns drops the trailing empty assistant turn (the one we just
// started) so the request only carries actual conversation.
func messagesFromTurns(turns []chatTurn) []core.ChatMessage {
	out := make([]core.ChatMessage, 0, len(turns))
	for _, t := range turns {
		if t.text == "" {
			continue
		}
		out = append(out, core.ChatMessage{Role: t.role, Content: t.text})
	}
	return out
}

func (c *chat) View(width, height int) string {
	var b strings.Builder
	header := fmt.Sprintf("Chat Playground · model=%s · vk=%s",
		dash(c.session.Model), dash(c.session.VKName))
	if c.compareModel != "" {
		header = fmt.Sprintf("Chat Playground · A/B · A=%s vs B=%s · vk=%s",
			dash(c.session.Model), c.compareModel, dash(c.session.VKName))
	}
	b.WriteString(styles.TileValue.Render(header))
	b.WriteString("\n")
	if sl := c.statusLine(); sl != "" {
		b.WriteString(sl)
		b.WriteString("\n")
	}
	if c.notice != "" {
		b.WriteString(styles.TileLabel.Render(c.notice))
		b.WriteString("\n")
	}
	if !c.ready() {
		b.WriteString(styles.TileLabel.Render(
			"Select a model + Virtual Key in the entry wizard (re-launch) before chatting."))
		return b.String()
	}
	if c.compareModel != "" {
		b.WriteString(c.compareTranscript(width, height-5))
	} else {
		b.WriteString(c.transcript(height - 4))
	}
	b.WriteString("\n")
	prompt := lipgloss.NewStyle().Foreground(styles.Brand).Render("you ▸ ")
	b.WriteString(prompt + c.input.View())
	return b.String()
}

// statusLine summarizes the active system prompt, temperature override, and the
// running session cost — only the parts that are set.
func (c *chat) statusLine() string {
	var parts []string
	if c.system != "" {
		parts = append(parts, "sys: "+clip(c.system, 40))
	}
	if c.temperature != nil {
		parts = append(parts, fmt.Sprintf("temp %.2f", *c.temperature))
	}
	if c.sessionCost > 0 {
		parts = append(parts, fmt.Sprintf("$%.4f this session", c.sessionCost))
	}
	if len(parts) == 0 {
		return ""
	}
	return styles.TileLabel.Render(strings.Join(parts, " · "))
}

// transcript renders the solo turn history, trimmed to fit budget lines
// (tail-first so the latest message is always visible).
func (c *chat) transcript(budget int) string {
	if budget < 3 {
		budget = 3
	}
	var lines []string
	for _, t := range c.turns {
		lines = append(lines, c.formatTurn(t)...)
		lines = append(lines, "")
	}
	if len(lines) > budget {
		lines = lines[len(lines)-budget:]
	}
	return strings.Join(lines, "\n")
}

// formatTurn renders one solo turn as: "ROLE  text… (usage)".
func (c *chat) formatTurn(t chatTurn) []string {
	roleStyle := styles.TileValue
	tag := "you "
	switch t.role {
	case "assistant":
		roleStyle = lipgloss.NewStyle().Foreground(styles.Brand).Bold(true)
		tag = "asst"
	case "system":
		roleStyle = styles.TileLabel
		tag = "sys "
	}
	head := roleStyle.Render(tag + " ▸ ")
	lines := []string{head + t.text}
	if t.usage != nil {
		stat := fmt.Sprintf("    ↳ tokens=%d (prompt %d, completion %d, cached %d) · %dms",
			t.usage.TotalTokens, t.usage.PromptTokens, t.usage.CompletionTokens,
			t.usage.PromptTokensDetails.CachedTokens, t.latencyMs)
		lines = append(lines, styles.TileLabel.Render(stat))
	}
	return lines
}

// compareTranscript renders the A/B rounds as stacked side-by-side panels,
// tail-trimmed to the line budget so the latest comparison stays visible.
func (c *chat) compareTranscript(width, budget int) string {
	if budget < 4 {
		budget = 4
	}
	colW := (width - 5) / 2
	var blocks []string
	for _, r := range c.rounds {
		blocks = append(blocks, compareRoundView(r, colW))
	}
	if len(blocks) == 0 {
		return styles.TileLabel.Render("Type a prompt — it will be sent to both models.")
	}
	lines := strings.Split(strings.Join(blocks, "\n"), "\n")
	if len(lines) > budget {
		lines = lines[len(lines)-budget:]
	}
	return strings.Join(lines, "\n")
}

// compareRoundView renders one round: the prompt over two model panels, the
// faster side flagged.
func compareRoundView(r compareRound, colW int) string {
	you := lipgloss.NewStyle().Foreground(styles.Brand).Render("you ▸ ") + r.prompt
	faster := fasterSide(r.a, r.b)
	boxA := compareSideBox("A", r.a, colW, faster == "A")
	boxB := compareSideBox("B", r.b, colW, faster == "B")
	return you + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, boxA, " ", boxB)
}

// compareSideBox renders one model's panel: title, answer (or error), and a
// tokens·latency stat line, highlighted green when this side was faster.
func compareSideBox(side string, res sideResult, colW int, faster bool) string {
	if colW < 16 {
		colW = 16
	}
	body := res.text
	if res.err != nil {
		body += "\n⚠ " + res.err.Error()
	}
	if strings.TrimSpace(body) == "" {
		body = "…"
	}
	var stat string
	switch {
	case res.usage != nil:
		stat = fmt.Sprintf("%d tok · %dms", res.usage.TotalTokens, res.latencyMs)
	case res.latencyMs > 0:
		stat = fmt.Sprintf("%dms", res.latencyMs)
	}
	if faster && stat != "" {
		stat = lipgloss.NewStyle().Foreground(styles.Green).Render(stat + " ⚡ faster")
	}
	title := lipgloss.NewStyle().Bold(true).Render(side + ": " + dash(res.model))
	content := title + "\n" + body
	if stat != "" {
		content += "\n" + styles.TileLabel.Render(stat)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(styles.Brand).
		Width(colW).
		Padding(0, 1)
	return box.Render(content)
}

// fasterSide reports which side ("A"/"B") had the lower latency, or "" for a tie
// or when either side has not completed (latency unknown).
func fasterSide(a, b sideResult) string {
	if a.latencyMs == 0 || b.latencyMs == 0 {
		return ""
	}
	switch {
	case a.latencyMs < b.latencyMs:
		return "A"
	case b.latencyMs < a.latencyMs:
		return "B"
	default:
		return ""
	}
}
