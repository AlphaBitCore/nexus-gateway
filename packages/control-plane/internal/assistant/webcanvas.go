package assistant

import (
	"errors"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/runtime"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
)

// NavigateDirective is the payload emitted when the agent drives the canvas. The
// browser maps view→route (+ optional traffic filter / event id) and navigates.
type NavigateDirective struct {
	View    string `json:"view"`
	Status  string `json:"status,omitempty"`
	Model   string `json:"model,omitempty"`
	EventID string `json:"eventId,omitempty"`
}

// webCanvas implements the kernel Canvas seam for the web face: instead of driving
// a TUI cockpit, it emits a navigation directive over SSE so the browser routes to
// the matching CP-UI page. Highlight has no web equivalent and is a no-op (the
// kernel tool degrades gracefully rather than erroring).
type webCanvas struct {
	emit func(NavigateDirective)
}

func newWebCanvas(emit func(NavigateDirective)) *webCanvas { return &webCanvas{emit: emit} }

func (c *webCanvas) Navigate(view string, filter core.TrafficFilter) error {
	if c.emit != nil {
		c.emit(NavigateDirective{View: view, Status: filter.StatusRange, Model: filter.ModelUsed})
	}
	return nil
}

func (c *webCanvas) ShowEvent(id string) error {
	if c.emit != nil {
		c.emit(NavigateDirective{View: "event", EventID: id})
	}
	return nil
}

// Highlight has no web equivalent. It returns an error so the kernel tool reports a
// recoverable failure to the model ("not available, use navigation") rather than
// falsely claiming success — the model must never narrate a highlight that did not
// happen.
func (c *webCanvas) Highlight(string) error {
	return errors.New("highlight is not available on the web; navigate to a page to surface information instead")
}

var _ runtime.Canvas = (*webCanvas)(nil)
