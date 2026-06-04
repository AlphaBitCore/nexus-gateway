package kit

// Cross-boundary messages: emitted by a view (in the views sub-package) and handled
// by the root shell, so they live in the shared leaf rather than in either package.
// A view's OWN private messages stay with the view; only these shell-handled ones
// are here.

// OpenEventMsg is emitted by the radar when a row is selected (or by the agent's
// show_event canvas drive); the shell drills into the Event view, loads the id, and
// — when Explain is set — auto-starts the LLM explanation once the event loads.
type OpenEventMsg struct {
	ID      string
	Explain bool
}

// SetModelMsg is emitted by the Models view when the operator picks a model; the
// shell switches the chat/agent model everywhere.
type SetModelMsg struct{ Code string }

// JumpMsg asks the shell to switch to a top-level view by index (a lateral jump
// that resets the drill path).
type JumpMsg struct{ Index int }
