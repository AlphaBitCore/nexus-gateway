package shell

// paneFocus is which pane owns the keyboard. The zero value is focusChat, so the
// dashboard launches with the conversation focused (the operator's chosen default).
type paneFocus int

const (
	focusChat   paneFocus = iota // bottom conversation focused (INPUT mode)
	focusCanvas                  // top view focused (NORMAL mode)
)

// Pane minimums (content rows). Below their sum the layout collapses to the single
// focused pane so it never renders unusably on a short terminal.
const (
	minCanvasHeight = 3
	minChatHeight   = 4
)

// Auto-grow split: the focused pane takes the larger share. chatGrowNum/Den is the
// chat fraction when chat is focused (~80% — when chatting the operator isn't
// watching the canvas; they tab over if they need it); canvasGrowNum/Den is the
// canvas fraction when the canvas is focused (~72%). The canvas keeps its minimum
// (minCanvasHeight) so a sliver stays visible to glance at and to tab back into.
const (
	chatGrowNum, chatGrowDen     = 4, 5   // 80% chat when chat-focused
	canvasGrowNum, canvasGrowDen = 18, 25 // 72% canvas when canvas-focused
)

// easeFrames is how many animation ticks the split interpolates over when focus
// changes — a short, bounded ease (snap-like), not a long animation.
const easeFrames = 4

// easeHeights returns the split heights `frame` steps into the transition toward
// the focus target, linearly interpolated from the *other* focus's split. At
// frame>=easeFrames it equals the target split. Focus only ever toggles, so the
// start split is the opposite focus's split.
func easeHeights(total int, target paneFocus, frame int) (top, bottom int) {
	if frame >= easeFrames {
		return splitHeights(total, target)
	}
	from := focusChat
	if target == focusChat {
		from = focusCanvas
	}
	fromTop, _ := splitHeights(total, from)
	toTop, _ := splitHeights(total, target)
	top = fromTop + (toTop-fromTop)*frame/easeFrames
	if top < 0 {
		top = 0
	}
	if top > total {
		top = total
	}
	return top, total - top
}

// splitHeights divides total content rows into (top canvas, bottom chat) heights
// for the given focus. The focused pane grows; the other keeps a usable minimum.
// Below the combined minimum the focused pane takes the whole height (single pane).
func splitHeights(total int, f paneFocus) (top, bottom int) {
	if total < minCanvasHeight+minChatHeight {
		if f == focusCanvas {
			return total, 0
		}
		return 0, total
	}
	switch f {
	case focusCanvas:
		top = total * canvasGrowNum / canvasGrowDen
		if total-top < minChatHeight {
			top = total - minChatHeight
		}
		if top < minCanvasHeight {
			top = minCanvasHeight
		}
		return top, total - top
	default: // focusChat
		bottom = total * chatGrowNum / chatGrowDen
		if total-bottom < minCanvasHeight {
			bottom = total - minCanvasHeight
		}
		if bottom < minChatHeight {
			bottom = minChatHeight
		}
		return total - bottom, bottom
	}
}
