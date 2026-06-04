package kit

import (
	"runtime"
	"strings"
)

// Key hints are OS-aware: a MacBook has no dedicated Page Up/Down keys (they are
// Fn+↑/↓), so a "PgUp/PgDn" label misleads Mac users. These helpers render the
// label that matches the host so the operator knows which physical keys to press.

// isMac reports whether the host is macOS (no dedicated PgUp/PgDn keys).
func isMac() bool { return runtime.GOOS == "darwin" }

// pageHint is the label for the half-page scroll keys for this OS. Both ↑/↓ (line)
// and the page keys are bound; the line keys need no OS-specific label.
func pageHint() string {
	if isMac() {
		return "fn+↑/↓"
	}
	return "PgUp/PgDn"
}

// GlobalHints is the always-available navigation strip shown in the otherwise
// unused top-right of the breadcrumb row, so the core controls are discoverable
// from any view without crowding the contextual keybar at the bottom.
func GlobalHints() string {
	return "tab pane · / cmds · ctrl+c quit"
}

// HelpReference is the full keys-and-commands reference rendered by /help. It is
// markdown (the transcript renders it) and OS-aware (the page keys differ on a
// Mac), so /help is a complete cheat sheet rather than a one-line hint.
func HelpReference() string {
	var b strings.Builder
	b.WriteString("## Nexus console — keys & commands\n\n")
	b.WriteString("**Navigate**\n")
	b.WriteString("- `tab` — switch focus between this chat and the view above\n")
	b.WriteString("- `1`–`9` — jump to a view · `/` — open the command palette\n")
	b.WriteString("- `←` / `esc` — back one level (close a detail, then up the trail)\n")
	b.WriteString("- `q` — quit (view focused) · `ctrl+c` — quit anytime\n\n")
	b.WriteString("**Chat (this pane)**\n")
	b.WriteString("- `enter` — send; you can keep typing while the agent works (messages queue)\n")
	b.WriteString("- `↑` / `↓` — scroll the transcript · `" + pageHint() + "` — page · `enter` jumps to latest\n")
	b.WriteString("- `esc` — interrupt the running turn\n\n")
	b.WriteString("**Slash commands** (press `/` on an empty prompt)\n")
	b.WriteString("- `/<view>` — open a view: overview, radar, cost, slo, nodes, alerts, jobs, sync, models, keys, rules, kill, lab\n")
	b.WriteString("- `/resource` — browse ANY admin kind: pick a kind, list it, drill a record (no LLM)\n")
	b.WriteString("- `/model [name]` — switch the chat model (no name → pick from the catalog)\n")
	b.WriteString("- `/event <id>` — open a traffic event · `/clear` — reset the conversation\n\n")
	b.WriteString("**In a list view**\n")
	b.WriteString("- `↑` / `↓` select · `enter` open the detail · `←` / `esc` back")
	return b.String()
}
