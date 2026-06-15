package shell

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// SessionBrowser is the dashboard's view of the local session store: list past
// conversations (newest-first, titled by their first user message), load one to
// resume it, delete one. *agent.Store satisfies it; the CLI binds the active
// env's on-disk store via Deps.OpenSessions.
type SessionBrowser interface {
	List() ([]agent.SessionMeta, error)
	Load(id string) (*agent.Session, error)
	Delete(id string) error
}

// sessionListCap bounds the picker to the newest sessions so a long-lived
// device's history stays scannable; the truncation is announced, never silent.
const sessionListCap = 50

// sessionWindow is how many rows render at once: a viewport around the cursor
// with "… N more" markers, so the picker never outgrows the terminal.
const sessionWindow = 10

// openSessionsMsg asks the root to open the session picker (the /sessions and
// /history slash commands, from the palette or typed into the chat prompt).
type openSessionsMsg struct{}

// sessionResumeMsg is the picker's enter: resume the chosen session.
type sessionResumeMsg struct{ id string }

// sessionsCloseMsg dismisses the picker (esc).
type sessionsCloseMsg struct{}

// sessionPicker is the /sessions overlay: past conversations newest-first
// (title + birthplace + relative time + message count). Typing filters the
// list live; ↑/↓ move within the filtered view, enter resumes, ctrl+d deletes
// (durably, via the browser), esc clears the filter first and closes when it
// is already empty. It owns the footer like the slash palette while open.
type sessionPicker struct {
	br     SessionBrowser
	all    []agent.SessionMeta // capped newest-first source
	metas  []agent.SessionMeta // the filtered view the cursor walks
	total  int                 // pre-cap count, for the honest cap line
	filter string
	cursor int
	err    string
}

func newSessionPicker(br SessionBrowser, metas []agent.SessionMeta) sessionPicker {
	total := len(metas)
	if len(metas) > sessionListCap {
		metas = metas[:sessionListCap]
	}
	p := sessionPicker{br: br, all: metas, total: total}
	p.refilter()
	return p
}

// refilter recomputes the visible view from the filter (case-insensitive
// substring on the title) and clamps the cursor into it.
func (p *sessionPicker) refilter() {
	if p.filter == "" {
		p.metas = p.all
	} else {
		q := strings.ToLower(p.filter)
		p.metas = nil
		for _, m := range p.all {
			if strings.Contains(strings.ToLower(m.Title), q) {
				p.metas = append(p.metas, m)
			}
		}
	}
	if p.cursor >= len(p.metas) {
		p.cursor = len(p.metas) - 1
	}
	if p.cursor < 0 {
		p.cursor = 0
	}
}

// Update folds one keystroke. Printable text edits the filter; enter emits
// sessionResumeMsg for the highlighted session; ctrl+d deletes it in place
// (the splice keeps the listing truthful without a re-list); esc clears a
// non-empty filter, else emits sessionsCloseMsg.
func (p sessionPicker) Update(msg tea.KeyPressMsg) (sessionPicker, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if p.filter != "" {
			p.filter = ""
			p.refilter()
			return p, nil
		}
		return p, func() tea.Msg { return sessionsCloseMsg{} }
	case "up":
		if p.cursor > 0 {
			p.cursor--
		}
		return p, nil
	case "down":
		if p.cursor < len(p.metas)-1 {
			p.cursor++
		}
		return p, nil
	case "enter":
		if p.cursor >= 0 && p.cursor < len(p.metas) {
			id := p.metas[p.cursor].ID
			return p, func() tea.Msg { return sessionResumeMsg{id: id} }
		}
		return p, nil
	case "ctrl+d":
		if p.cursor >= 0 && p.cursor < len(p.metas) {
			doomed := p.metas[p.cursor].ID
			if err := p.br.Delete(doomed); err != nil {
				p.err = "couldn't delete: " + err.Error()
				return p, nil
			}
			p.err = ""
			for i, m := range p.all {
				if m.ID == doomed {
					p.all = append(p.all[:i], p.all[i+1:]...)
					p.total--
					break
				}
			}
			p.refilter()
		}
		return p, nil
	case "backspace":
		if p.filter != "" {
			r := []rune(p.filter)
			p.filter = string(r[:len(r)-1])
			p.refilter()
		}
		return p, nil
	}
	if msg.Text != "" {
		p.filter += msg.Text
		p.cursor = 0
		p.refilter()
	}
	return p, nil
}

// View renders the picker panel: a title row, the live filter, any error,
// then a cursor-centered window of session rows with explicit "… N more"
// markers (or the named empty / no-match state).
func (p sessionPicker) View() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Past conversations") +
		styles.TileLabel.Render("  type to filter · enter resume · ctrl+d delete · esc close"))
	if p.filter != "" {
		b.WriteString("\n" + styles.TileLabel.Render("  filter: ") + lipgloss.NewStyle().Bold(true).Render(p.filter))
	}
	if p.err != "" {
		b.WriteString("\n" + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+p.err))
	}
	b.WriteString("\n")
	if len(p.all) == 0 {
		b.WriteString(styles.TileLabel.Render("  no saved conversations yet — they're saved automatically as you chat"))
		return styles.Panel.Render(b.String())
	}
	if len(p.metas) == 0 {
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("  no conversations match %q — backspace to widen", p.filter)))
		return styles.Panel.Render(b.String())
	}

	start := p.cursor - sessionWindow/2
	if start > len(p.metas)-sessionWindow {
		start = len(p.metas) - sessionWindow
	}
	if start < 0 {
		start = 0
	}
	end := start + sessionWindow
	if end > len(p.metas) {
		end = len(p.metas)
	}
	if start > 0 {
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("  … %d more above", start)) + "\n")
	}
	for i := start; i < end; i++ {
		m := p.metas[i]
		prefix := "  "
		title := m.Title
		if i == p.cursor {
			prefix = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			title = lipgloss.NewStyle().Bold(true).Render(title)
		}
		// Birthplace badge: a session whose env is "web" was created in the web
		// chat and arrived here via sync; everything else was born on this device.
		badge := ""
		if m.Env == "web" {
			badge = " " + lipgloss.NewStyle().Foreground(styles.BrandHi).Render("[web]")
		}
		b.WriteString(prefix + title + badge + styles.TileLabel.Render(
			fmt.Sprintf("  %s · %d msgs", relTime(m.Updated), m.Messages)) + "\n")
	}
	if end < len(p.metas) {
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("  … %d more below", len(p.metas)-end)) + "\n")
	}
	if p.filter == "" && p.total > len(p.all) {
		b.WriteString(styles.TileLabel.Render(
			fmt.Sprintf("  showing the newest %d of %d — older conversations not shown", len(p.all), p.total)) + "\n")
	}
	return styles.Panel.Render(strings.TrimRight(b.String(), "\n"))
}

// relTime renders an age compactly ("just now", "5m ago", "3h ago", "2d ago").
func relTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
