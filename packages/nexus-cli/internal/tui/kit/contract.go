// Package kit is the tui's shared contract + leaf widgets: the types every view
// and the shell agree on (Session, the ViewModel interface), the Gateway capability
// surface, and the pure rendering widgets + timing helpers. It is a dependency-free
// leaf within the tui tree (imports only stdlib + bubbletea + lipgloss + ntcharts +
// core + the styles sibling), so the domain sub-packages (views, conversation,
// resource, wizard) and the root shell can all import it without a cycle. Root
// re-exports the public names (Session, Gateway, …) via type aliases so the cli's
// tui.Session / tui.Gateway references stay unchanged.
package kit

import tea "charm.land/bubbletea/v2"

// Session is the resolved context the dashboard renders against: the active
// environment plus the remembered model/VK selection. VKSecret is held only in
// memory for the conversation + Chat/Lab views — it is never written to disk.
type Session struct {
	EnvName       string
	Addr          string // Control Plane base URL, shown in the location indicator
	IsProd        bool
	Model         string // selected model code/slug
	ContextWindow int    // the model's max context tokens (0 = unknown); seeds the context gauge before the first turn
	VKID          string
	VKName        string
	VKSecret      string
}

// ViewModel is one resource view. Views are self-contained: Init starts their
// data fetch/poll, Update folds messages, View renders into the given content box.
type ViewModel interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (ViewModel, tea.Cmd)
	View(width, height int) string
}
