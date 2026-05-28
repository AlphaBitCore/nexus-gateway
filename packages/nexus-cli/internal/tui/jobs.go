package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// jobsView lists the scheduled background jobs and whether they are enabled.
type jobsView struct {
	gw      Gateway
	res     *core.JobsResult
	err     error
	loading bool
}

type jobsMsg struct {
	res *core.JobsResult
	err error
}
type jobsTick struct{}

func newJobs(gw Gateway) *jobsView { return &jobsView{gw: gw, loading: true} }

func (j *jobsView) Init() tea.Cmd { return j.fetch() }

func (j *jobsView) fetch() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := j.gw.Jobs(ctx)
		return jobsMsg{res: res, err: err}
	}
}

func (j *jobsView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case jobsMsg:
		j.loading = false
		j.err = msg.err
		if msg.res != nil {
			j.res = msg.res
		}
		return j, tick(pollSlow, jobsTick{})
	case jobsTick:
		return j, j.fetch()
	}
	return j, nil
}

func (j *jobsView) View(width, height int) string {
	if j.loading && j.res == nil {
		return styles.TileLabel.Render("loading jobs…")
	}
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Scheduled jobs"))
	if j.err != nil {
		b.WriteString("  " + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+j.err.Error()))
	}
	b.WriteString("\n")
	if j.res == nil || len(j.res.Jobs) == 0 {
		b.WriteString(styles.TileLabel.Render("  no jobs"))
		return b.String()
	}
	b.WriteString(lipgloss.NewStyle().Foreground(styles.Sub).Render(
		fmt.Sprintf("  %-36s %-9s %-10s %s", "JOB", "ENABLED", "EVERY", "LAST RUN")))
	b.WriteString("\n")
	var rows []string
	for _, job := range j.res.Jobs {
		dot := lipgloss.NewStyle().Foreground(styles.Green).Render("●")
		if !job.Enabled {
			dot = lipgloss.NewStyle().Foreground(styles.Sub).Render("○")
		}
		name := job.Name
		if len(name) > 34 {
			name = name[:33] + "…"
		}
		rows = append(rows, fmt.Sprintf("  %s %-34s %-9t %-10s %s",
			dot, name, job.Enabled, jobInterval(job.Interval), shortTime(job.LastRun)))
	}
	b.WriteString(strings.Join(rows, "\n"))
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
	return dash(ts)
}
