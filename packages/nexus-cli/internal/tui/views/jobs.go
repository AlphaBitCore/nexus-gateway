package views

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// jobsView lists the scheduled background jobs with a row cursor; enter opens a
// detail drawer of the selected job (schedule + last run + id).
type jobsView struct {
	gw      kit.Gateway
	res     *core.JobsResult
	cursor  int
	detail  bool
	err     error
	loading bool
}

type jobsMsg struct {
	res *core.JobsResult
	err error
}
type jobsTick struct{}

func newJobs(gw kit.Gateway) *jobsView { return &jobsView{gw: gw, loading: true} }

func (j *jobsView) Init() tea.Cmd { return j.fetch() }

func (j *jobsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		res, err := j.gw.Jobs(ctx)
		return jobsMsg{res: res, err: err}
	}
}

func (j *jobsView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case jobsMsg:
		j.loading = false
		j.err = msg.err
		if msg.res != nil {
			j.res = msg.res
		}
		j.clampCursor()
		return j, kit.Tick(kit.PollSlow, jobsTick{})
	case jobsTick:
		return j, j.fetch()
	case tea.KeyPressMsg:
		if j.detail {
			return j, nil
		}
		switch msg.String() {
		case "up", "k":
			if j.cursor > 0 {
				j.cursor--
			}
		case "down", "j":
			if j.cursor < j.count()-1 {
				j.cursor++
			}
		case "enter":
			if _, ok := j.selected(); ok {
				j.detail = true
			}
		}
	}
	return j, nil
}

// back closes the detail drawer so esc returns to the list before the root pops
// the nav stack.
func (j *jobsView) Back() bool {
	if j.detail {
		j.detail = false
		return true
	}
	return false
}

func (j *jobsView) count() int {
	if j.res == nil {
		return 0
	}
	return len(j.res.Jobs)
}

func (j *jobsView) selected() (core.Job, bool) {
	if j.res == nil || j.cursor < 0 || j.cursor >= len(j.res.Jobs) {
		return core.Job{}, false
	}
	return j.res.Jobs[j.cursor], true
}

func (j *jobsView) clampCursor() {
	if j.cursor >= j.count() {
		j.cursor = j.count() - 1
	}
	if j.cursor < 0 {
		j.cursor = 0
	}
}

func (j *jobsView) Help() string {
	if j.detail {
		return "←/esc back · q quit"
	}
	return "↑/↓ select · enter open · ←/esc back · q quit"
}

func (j *jobsView) View(width, height int) string {
	if j.loading && j.res == nil {
		return styles.TileLabel.Render("loading jobs…")
	}
	if j.detail {
		return j.detailView()
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Scheduled jobs"))
	if j.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+j.err.Error()))
	}
	b.WriteString("\n")
	if j.count() == 0 {
		b.WriteString(styles.TileLabel.Render("  no jobs"))
		return b.String()
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-36s %-9s %-10s %s", "JOB", "ENABLED", "EVERY", "LAST RUN")))
	b.WriteString("\n")
	var rows []string
	for i, job := range j.res.Jobs {
		dot := lipgloss.NewStyle().Foreground(styles.Green).Render("●")
		if !job.Enabled {
			dot = lipgloss.NewStyle().Foreground(styles.Sub).Render("○")
		}
		cursor := "  "
		line := fmt.Sprintf("%s %-34s %-9t %-10s %s",
			dot, kit.Clip(job.Name, 34), job.Enabled, jobInterval(job.Interval), shortTime(job.LastRun))
		if i == j.cursor {
			cursor = lipgloss.NewStyle().Foreground(styles.Brand).Render("▸ ")
			line = lipgloss.NewStyle().Bold(true).Render(line)
		}
		rows = append(rows, cursor+line)
	}
	b.WriteString(strings.Join(rows, "\n"))
	return b.String()
}

// detailView renders the full record of the selected scheduled job.
func (j *jobsView) detailView() string {
	job, ok := j.selected()
	if !ok {
		return styles.TileLabel.Render("(no job selected)")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Job · " + job.Name))
	b.WriteString("\n\n")
	status := lipgloss.NewStyle().Foreground(styles.Green).Render("● enabled")
	if !job.Enabled {
		status = lipgloss.NewStyle().Foreground(styles.Sub).Render("○ disabled")
	}
	b.WriteString(kit.DetailRow("Status", status) + "\n")
	b.WriteString(kit.DetailRow("Runs every", jobInterval(job.Interval)) + "\n")
	b.WriteString(kit.DetailRow("Last run", shortTime(job.LastRun)) + "\n")
	b.WriteString(kit.DetailRow("Job id", kit.Dash(job.ID)))
	return b.String()
}

// jobInterval renders a nanosecond interval as a compact duration.
func jobInterval(ns int64) string {
	d := time.Duration(ns)
	if d <= 0 {
		return "—"
	}
	return d.String()
}

// shortTime trims an RFC3339 timestamp to date+time for display.
func shortTime(ts string) string {
	if len(ts) >= 19 {
		return strings.Replace(ts[:19], "T", " ", 1)
	}
	return kit.Dash(ts)
}
