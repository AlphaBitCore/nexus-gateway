package views

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (c *chat) View(width, height int) string {
	var b strings.Builder
	header := fmt.Sprintf("Chat Playground · model=%s · vk=%s",
		kit.Dash(c.session.Model), kit.Dash(c.session.VKName))
	if c.compareModel != "" {
		header = fmt.Sprintf("Chat Playground · A/B · A=%s vs B=%s · vk=%s",
			kit.Dash(c.session.Model), c.compareModel, kit.Dash(c.session.VKName))
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
		b.WriteString(c.transcript(width, height-4))
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
		parts = append(parts, "sys: "+kit.Clip(c.system, 40))
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
func (c *chat) transcript(width, budget int) string {
	if budget < 3 {
		budget = 3
	}
	var lines []string
	for _, t := range c.turns {
		lines = append(lines, c.formatTurn(t, width)...)
		lines = append(lines, "")
	}
	if len(lines) > budget {
		lines = lines[len(lines)-budget:]
	}
	return strings.Join(lines, "\n")
}

// formatTurn renders one solo turn as: "ROLE ▸ text" (word-wrapped to width) plus
// an optional usage line.
func (c *chat) formatTurn(t chatTurn, width int) []string {
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
	lines := strings.Split(kit.WrapText(head+t.text, width), "\n")
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
	title := lipgloss.NewStyle().Bold(true).Render(side + ": " + kit.Dash(res.model))
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
