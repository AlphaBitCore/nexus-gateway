package views

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
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
// history and the passthrough snapshot. Each toggle is a write, so it is raised
// behind the shared confirm gate in EVERY environment — the same Allow/Deny gate
// every other mitigation uses (prod adds its red banner + env-named Apply button).
type killView struct {
	gw      kit.Gateway
	session kit.Session

	ks       *core.KillSwitchState     // current kill-switch state (read)
	ksResult *core.KillSwitchResult    // last toggle's fan-out counts
	snap     *core.PassthroughSnapshot // current 3-tier passthrough state (read)
	loading  bool
	ksErr    error // kill-switch read error (kept separate from the snapshot's)
	snapErr  error // passthrough-snapshot read error

	err  error
	busy bool
	cf   kit.Confirm // shared Allow/Deny gate — every toggle is confirmed before it fires
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

// setSession follows a runtime chat-model switch (sessionSetter).
func (v *killView) SetSession(s kit.Session) { v.session = s }

func newKill(gw kit.Gateway, s kit.Session) *killView {
	return &killView{gw: gw, session: s, loading: true, cf: kit.NewConfirm(s)}
}

func (k *killView) Init() tea.Cmd { return k.fetch() }

// fetch reads the current kill-switch state + passthrough snapshot so the view
// renders the real state rather than a placeholder.
func (k *killView) fetch() tea.Cmd {
	gw := k.gw
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		// Read both independently — a failure of one must not blank the other's
		// panel (a passthrough-read error shouldn't hide a good kill-switch state).
		ks, ksErr := gw.KillSwitchStatus(ctx)
		snap, snapErr := gw.PassthroughSnapshot(ctx)
		return killLoadMsg{ks: ks, ksErr: ksErr, snap: snap, snapErr: snapErr}
	}
}

// leave clears an in-flight confirmation when the operator tabs away, so tabbing
// back never re-shows a stale confirmation prompt.
func (k *killView) Leave() { k.cf.Cancel() }

// capturing reports that the confirm gate owns keystrokes, so the root model
// suspends its single-letter shortcuts.
func (k *killView) Capturing() bool { return k.cf.Capturing() }

func (k *killView) Help() string {
	if k.cf.Capturing() {
		return k.cf.HelpHint()
	}
	return "e/d kill-switch on/off · p/o passthrough on/off · q quit"
}

func (k *killView) Update(msg tea.Msg) (kit.ViewModel, tea.Cmd) {
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
		k.err = msg.err
		if msg.result != nil {
			k.ksResult = msg.result
		}
		// Re-read state so the display reflects the toggle just made.
		return k, k.fetch()
	case tea.KeyPressMsg:
		// The gate owns keys while it is up; it returns handled=false otherwise so
		// the view keeps the key for its own bindings.
		if handled, cmd := k.cf.Update(msg); handled {
			return k, cmd
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

// request raises the shared confirm gate for a toggle. The write fires only when
// the operator authorizes it — in every environment (prod adds its env-named
// ceremony). No toggle ever fires without confirmation.
func (k *killView) request(target killTarget, engage bool) (kit.ViewModel, tea.Cmd) {
	k.err = nil
	verb := "engage"
	if !engage {
		verb = "disengage"
	}
	what := "the kill switch"
	if target == targetPassthrough {
		what = "global emergency passthrough"
	}
	return k, k.cf.Begin(fmt.Sprintf("%s %s", verb, what), func() tea.Cmd {
		k.busy = true
		return k.fireCmd(target, engage)
	})
}

// fireCmd builds the write command for the active target, run once the gate allows.
func (k *killView) fireCmd(target killTarget, engage bool) tea.Cmd {
	gw := k.gw
	if target == targetPassthrough {
		return func() tea.Msg {
			ctx, cancel := kit.FetchCtx()
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
	return func() tea.Msg {
		ctx, cancel := kit.FetchCtx()
		defer cancel()
		res, err := gw.SetKillSwitch(ctx, engage)
		return killResultMsg{result: res, err: err}
	}
}

func (k *killView) View(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.TileValue.Render("Emergency controls"))
	b.WriteString("\n\n")

	if k.cf.Capturing() {
		b.WriteString(k.cf.View())
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
