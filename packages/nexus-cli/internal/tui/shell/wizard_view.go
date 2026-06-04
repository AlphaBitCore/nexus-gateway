package shell

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/styles"
)

func (w *wizard) View(width, height int) string {
	var b strings.Builder
	b.WriteString(styles.StatusBar.Render("nexus setup · " + w.session.EnvName))
	b.WriteString("\n\n")
	switch w.stage {
	case stageEnv:
		b.WriteString(styles.TileValue.Render("Step 0 — Choose an environment"))
		b.WriteString("\n\n")
		b.WriteString(w.renderEnvs(height - 12))
		// Show URL detail for the selected env so the operator can confirm
		// where it points without re-running setup.
		if w.envCursor < len(w.deps.EnvNames) && w.deps.EnvDetail != nil {
			name := w.deps.EnvNames[w.envCursor]
			cpURL, aigwURL, prod, err := w.deps.EnvDetail(name)
			if err == nil {
				b.WriteString("\n")
				prodTag := ""
				if prod {
					prodTag = lipgloss.NewStyle().Foreground(styles.Red).Render(" prod")
				}
				b.WriteString(styles.TileLabel.Render(
					fmt.Sprintf("  cp: %s   aigw: %s%s", cpURL, aigwURL, prodTag)))
			}
		}
		// Delete-confirm banner — `d` arms, `d` again deletes. Drawn here so
		// the operator never has to recall what their first `d` did.
		if w.confirmDel && w.envCursor < len(w.deps.EnvNames) {
			b.WriteString("\n\n")
			b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render(
				fmt.Sprintf("⚠ press d again to delete %q (this clears its stored secrets)", w.deps.EnvNames[w.envCursor])))
		}
	case stageEnvName:
		b.WriteString(styles.TileValue.Render("New environment — name"))
		b.WriteString("\n\n")
		b.WriteString(w.input.View())
	case stageEnvURL:
		b.WriteString(styles.TileValue.Render(fmt.Sprintf("New environment %q — Control Plane URL", w.newEnvName)))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("e.g. https://cp.nexus.your-domain.com"))
		b.WriteString("\n\n")
		b.WriteString(w.input.View())
	case stageEnvAIGWURL:
		b.WriteString(styles.TileValue.Render(fmt.Sprintf("New environment %q — AI Gateway URL", w.newEnvName)))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("the host that serves /v1/chat/completions — defaults to the CP URL above; edit if your gateway is on a different host/port"))
		b.WriteString("\n\n")
		b.WriteString(w.input.View())
	case stageLogin:
		b.WriteString(styles.TileValue.Render("Step 1 — Log in"))
		b.WriteString("\n\n")
		if w.loggingIn {
			b.WriteString(styles.TileLabel.Render("opening browser… complete the login there, then return here"))
		} else {
			b.WriteString(styles.TileLabel.Render("sign in via your browser (OAuth2 + PKCE) — a short loopback redirect captures the code"))
		}
	case stageModel:
		b.WriteString(styles.TileValue.Render("Step 2 — Pick a model"))
		b.WriteString("\n\n")
		b.WriteString(w.renderModels(height - 8))
	case stageVK:
		b.WriteString(styles.TileValue.Render("Step 3 — Pick a Virtual Key (or press c to create your own)"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("a VK authenticates your traffic against the gateway — pick one you own, or create one"))
		b.WriteString("\n\n")
		b.WriteString(w.renderVKs(height - 9))
	case stageSecret:
		b.WriteString(styles.TileValue.Render("Step 4 — Paste the VK secret"))
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render(fmt.Sprintf("key %q — the secret is shown once at creation and never stored server-side", w.session.VKName)))
		b.WriteString("\n\n")
		b.WriteString(w.secret.View())
	}
	// Centralised key-hints footer — every stage gets a clear, OS-aware cheat line
	// at the bottom so an operator never has to guess which keys are live.
	b.WriteString("\n\n")
	b.WriteString(styles.HelpBar.Render(w.keyHints()))
	if w.loading {
		b.WriteString("\n")
		b.WriteString(styles.TileLabel.Render("loading…"))
	}
	if w.err != nil {
		b.WriteString("\n")
		b.WriteString(lipgloss.NewStyle().Foreground(styles.Red).Render("⚠ " + w.err.Error()))
	}
	return b.String()
}

// renderEnvs renders the env list with a trailing "create new" row.
// goBack walks the wizard back one stage, the natural reverse of how each stage
// was entered. err is cleared so a stale validation message from the abandoned
// stage doesn't leak; the focused input is restored where applicable.
func (w *wizard) goBack() (*wizard, tea.Cmd) {
	w.err = nil
	switch w.stage {
	case stageEnvName:
		w.stage = stageEnv
	case stageEnvURL:
		w.input.SetValue(w.newEnvName)
		w.input.Placeholder = "new environment name (e.g. dev)"
		w.stage = stageEnvName
		return w, w.input.Focus()
	case stageEnvAIGWURL:
		// Restore the CP URL into the input so the previous step renders with what
		// the operator typed; their AI Gateway draft is dropped (it will be
		// re-prefilled to the CP URL when they advance again).
		w.input.SetValue(w.newEnvCPURL)
		w.input.Placeholder = "Control Plane base URL (https://…)"
		w.input.CursorEnd()
		w.stage = stageEnvURL
		return w, w.input.Focus()
	case stageLogin:
		// Cancel the in-flight browser login so the loopback listener releases its
		// port immediately and the late loginDoneMsg arrives with a
		// context-cancelled err that the Update handler discards (stage already
		// moved off stageLogin). Without this, esc only flips the spinner off but
		// leaves the listener + browser tab hanging until the 3-minute timeout.
		if w.loginCancel != nil {
			w.loginCancel()
			w.loginCancel = nil
		}
		w.loggingIn = false
		w.stage = stageEnv
	case stageModel:
		w.stage = stageEnv
	case stageVK:
		w.stage = stageModel
	case stageSecret:
		w.stage = stageVK
		w.secret.SetValue("")
	}
	return w, nil
}

// keyHints returns the cheat line shown at the bottom of every wizard stage so
// the operator never has to guess which keys are live. Stage-specific, short,
// and consistent in shape so the eye can scan it.
func (w *wizard) keyHints() string {
	switch w.stage {
	case stageEnv:
		if w.confirmDel {
			return "press d again to confirm · any other key cancels"
		}
		return "↑/↓ move · enter use · e edit urls · v change key · d delete · ctrl+c quit"
	case stageEnvName:
		return "type the name · enter continue · esc back"
	case stageEnvURL:
		return "type or paste the URL · enter continue · esc back"
	case stageEnvAIGWURL:
		return "edit if needed · enter create · esc back"
	case stageLogin:
		if w.loggingIn {
			return "waiting on the browser… · esc cancel"
		}
		return "enter sign in (browser opens) · esc cancel"
	case stageModel:
		return "↑/↓ move · enter pick · esc back"
	case stageVK:
		return "↑/↓ move · enter use selected · c create new · esc back"
	case stageSecret:
		if w.loading {
			return "verifying the Virtual Key against the gateway… · esc cancel"
		}
		return "paste the VK secret · enter verify + save · esc back"
	}
	return ""
}

func (w *wizard) renderEnvs(budget int) string {
	n := len(w.deps.EnvNames)
	return kit.RenderCursorList(budget, w.envCursor, n+1, func(i int) string {
		if i == n {
			return "＋ create a new environment"
		}
		return w.deps.EnvNames[i]
	})
}

func (w *wizard) renderModels(budget int) string {
	if len(w.models) == 0 {
		if w.loading {
			return ""
		}
		return styles.TileLabel.Render("  (no chat models configured)")
	}
	return kit.RenderCursorList(budget, w.modelCursor, len(w.models), func(i int) string {
		m := w.models[i]
		return fmt.Sprintf("%-26s %s", m.code, styles.TileLabel.Render(m.provider))
	})
}

func (w *wizard) renderVKs(budget int) string {
	if len(w.vks) == 0 {
		if w.loading {
			return ""
		}
		return styles.TileLabel.Render("  (no enabled virtual keys — create one in the web UI)")
	}
	return kit.RenderCursorList(budget, w.vkCursor, len(w.vks), func(i int) string {
		v := w.vks[i]
		return fmt.Sprintf("%-28s %s", v.Name, styles.TileLabel.Render(v.KeyPrefix))
	})
}

// renderCursorList renders a windowed, cursor-highlighted list of n rows.
