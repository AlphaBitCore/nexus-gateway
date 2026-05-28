package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

// killTarget names which write the pending confirmation will fire.
type killTarget int

const (
	targetKillSwitch killTarget = iota
	targetPassthrough
)

// killView is the emergency-controls surface: the global kill switch (halts TLS
// bumping fleet-wide) and the global emergency-passthrough tier (bypasses the
// compliance hooks). Both current states are read on open from the config-sync
// history and the passthrough snapshot; each toggle is a write, so a prod
// environment requires a typed confirmation (the env name) before it fires.
type killView struct {
	gw      Gateway
	session Session

	ks       *core.KillSwitchState     // current kill-switch state (read)
	ksResult *core.KillSwitchResult    // last toggle's fan-out counts
	snap     *core.PassthroughSnapshot // current 3-tier passthrough state (read)
	loading  bool
	ksErr    error // kill-switch read error (kept separate from the snapshot's)
	snapErr  error // passthrough-snapshot read error

	err        error
	busy       bool
	confirming bool
	target     killTarget
	wantEngage bool
	input      textinput.Model
}

type killLoadMsg struct {
	ks      *core.KillSwitchState
	ksErr   error
	snap    *core.PassthroughSnapshot
	snapErr error
}

type killResultMsg struct {
	result *core.KillSwitchResult
	err    error
}

func newKill(gw Gateway, s Session) *killView {
	ti := textinput.New()
	ti.Placeholder = s.EnvName
	ti.CharLimit = 40
	return &killView{gw: gw, session: s, loading: true, input: ti}
}

func (k *killView) Init() tea.Cmd { return k.fetch() }

// fetch reads the current kill-switch state + passthrough snapshot so the view
// renders the real state rather than a placeholder.
func (k *killView) fetch() tea.Cmd {
	gw := k.gw
	return func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		// Read both independently — a failure of one must not blank the other's
		// panel (a passthrough-read error shouldn't hide a good kill-switch state).
		ks, ksErr := gw.KillSwitchStatus(ctx)
		snap, snapErr := gw.PassthroughSnapshot(ctx)
		return killLoadMsg{ks: ks, ksErr: ksErr, snap: snap, snapErr: snapErr}
	}
}

// leave clears an in-flight prod confirmation when the operator tabs away, so
// tabbing back never re-shows a stale confirmation prompt.
func (k *killView) leave() {
	if k.confirming {
		k.confirming = false
		k.input.Blur()
	}
}

// capturing reports that the prod confirmation field is focused, so the root
// model suspends its single-letter shortcuts.
func (k *killView) capturing() bool { return k.confirming }

func (k *killView) help() string {
	if k.confirming {
		return fmt.Sprintf("type %q + enter to confirm · esc cancel", k.session.EnvName)
	}
	return "e/d kill-switch on/off · p/o passthrough on/off · q quit"
}

func (k *killView) Update(msg tea.Msg) (viewModel, tea.Cmd) {
	switch msg := msg.(type) {
	case killLoadMsg:
		k.loading = false
		k.ksErr, k.snapErr = msg.ksErr, msg.snapErr
		if msg.ks != nil {
			k.ks = msg.ks
		}
		if msg.snap != nil {
			k.snap = msg.snap
		}
		return k, nil
	case killResultMsg:
		k.busy = false
		k.confirming = false
		k.err = msg.err
		if msg.result != nil {
			k.ksResult = msg.result
		}
		// Re-read state so the display reflects the toggle just made.
		return k, k.fetch()
	case tea.KeyMsg:
		if k.confirming {
			return k.updateConfirm(msg)
		}
		if k.busy {
			return k, nil
		}
		switch msg.String() {
		case "e":
			return k.request(targetKillSwitch, true)
		case "d":
			return k.request(targetKillSwitch, false)
		case "p":
			return k.request(targetPassthrough, true)
		case "o":
			return k.request(targetPassthrough, false)
		}
	}
	return k, nil
}

// request begins a toggle: in prod it opens the typed-confirmation field;
// otherwise it fires immediately.
func (k *killView) request(target killTarget, engage bool) (viewModel, tea.Cmd) {
	k.target = target
	k.wantEngage = engage
	k.err = nil
	if k.session.IsProd {
		k.confirming = true
		k.input.SetValue("")
		return k, k.input.Focus()
	}
	return k.fire()
}

// updateConfirm handles keystrokes while the prod confirmation field is focused.
func (k *killView) updateConfirm(msg tea.KeyMsg) (viewModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		k.confirming = false
		k.input.Blur()
		return k, nil
	case "enter":
		if strings.TrimSpace(k.input.Value()) != k.session.EnvName {
			k.err = fmt.Errorf("confirmation %q did not match env %q — not toggled", k.input.Value(), k.session.EnvName)
			k.confirming = false
			k.input.Blur()
			return k, nil
		}
		k.input.Blur()
		return k.fire()
	}
	var cmd tea.Cmd
	k.input, cmd = k.input.Update(msg)
	return k, cmd
}

// fire dispatches the pending write for the active target.
func (k *killView) fire() (viewModel, tea.Cmd) {
	k.busy = true
	k.confirming = false
	target, engage, gw := k.target, k.wantEngage, k.gw
	if target == targetPassthrough {
		return k, func() tea.Msg {
			ctx, cancel := fetchCtx()
			defer cancel()
			// Engaging the global passthrough bypasses the compliance hooks — the
			// canonical "let traffic through" emergency. Disengaging clears it.
			err := gw.SetPassthroughGlobal(ctx, core.PassthroughGlobalRequest{
				Enabled:     engage,
				BypassHooks: engage,
				Reason:      "engaged via nexus operator toolkit",
			})
			return killResultMsg{err: err}
		}
	}
	return k, func() tea.Msg {
		ctx, cancel := fetchCtx()
		defer cancel()
		res, err := gw.SetKillSwitch(ctx, engage)
		return killResultMsg{result: res, err: err}
	}
}

func (k *killView) View(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Emergency controls"))
	b.WriteString("\n\n")

	if k.confirming {
		what := "the kill switch"
		if k.target == targetPassthrough {
			what = "global emergency passthrough"
		}
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Bold(true).Render(
			fmt.Sprintf("⚠ PROD %s — confirm %s of %s", k.session.EnvName, engageVerb(k.wantEngage), what)))
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("Type the environment name (%s) to confirm:\n", k.session.EnvName))
		b.WriteString(k.input.View())
		return b.String()
	}

	b.WriteString(k.killSwitchPanel())
	b.WriteString("\n\n")
	b.WriteString(k.passthroughPanel())
	if k.busy {
		b.WriteString("\n\n" + styles.TileLabel.Render("toggling…"))
	}
	if k.err != nil {
		b.WriteString("\n\n" + lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ "+k.err.Error()))
	}
	return b.String()
}

// killSwitchPanel renders the global kill switch (halts TLS bumping fleet-wide).
func (k *killView) killSwitchPanel() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Kill switch"))
	b.WriteString(styles.TileLabel.Render("  — halts TLS bumping on every node (e engage · d disengage)"))
	b.WriteString("\n")
	switch {
	case k.loading:
		b.WriteString(styles.TileLabel.Render("  loading…"))
	case k.ksErr != nil:
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("  ⚠ " + k.ksErr.Error()))
	case k.ks != nil && !k.ks.Known:
		b.WriteString(styles.TileLabel.Render("  never toggled (off)"))
	case k.ks != nil:
		b.WriteString("  " + stateBadge(k.ks.Engaged))
		meta := fmt.Sprintf("  version %d", k.ks.Version)
		if k.ks.By != "" {
			meta += " · by " + k.ks.By
		}
		b.WriteString(styles.TileLabel.Render(meta))
	}
	if k.ksResult != nil {
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("   (last toggle: %d/%d nodes online notified)",
			k.ksResult.ThingsNotified, k.ksResult.ThingsOnline)))
	}
	return b.String()
}

// passthroughPanel renders the global emergency-passthrough tier plus a count of
// any per-adapter / per-provider overrides currently bypassing the pipeline.
func (k *killView) passthroughPanel() string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Emergency passthrough"))
	b.WriteString(styles.TileLabel.Render("  — bypasses compliance hooks (p engage · o disengage)"))
	b.WriteString("\n")
	switch {
	case k.loading:
		b.WriteString(styles.TileLabel.Render("  loading…"))
		return b.String()
	case k.snapErr != nil:
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("  ⚠ " + k.snapErr.Error()))
		return b.String()
	case k.snap == nil:
		b.WriteString(styles.TileLabel.Render("  unavailable"))
		return b.String()
	}
	g := k.snap.Global
	engaged := g.Enabled && (g.BypassHooks || g.BypassCache || g.BypassNormalize)
	b.WriteString("  global " + stateBadge(engaged))
	if engaged {
		var by []string
		if g.BypassHooks {
			by = append(by, "hooks")
		}
		if g.BypassCache {
			by = append(by, "cache")
		}
		if g.BypassNormalize {
			by = append(by, "normalize")
		}
		b.WriteString(styles.TileLabel.Render("  bypassing: " + strings.Join(by, ", ")))
	}
	adapters, providers := k.snap.ActiveOverrides()
	if adapters > 0 || providers > 0 {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Amber).Render(
			fmt.Sprintf("  overrides active: %d adapter(s), %d provider(s) bypassing", adapters, providers)))
	}
	return b.String()
}

// stateBadge renders an ENGAGED/clear badge colored by danger.
func stateBadge(engaged bool) string {
	if engaged {
		return lipgloss.NewStyle().Bold(true).Foreground(styles.Red).Render("ENGAGED")
	}
	return lipgloss.NewStyle().Bold(true).Foreground(styles.Green).Render("off")
}

func engageVerb(engage bool) string {
	if engage {
		return "ENGAGE"
	}
	return "DISENGAGE"
}
