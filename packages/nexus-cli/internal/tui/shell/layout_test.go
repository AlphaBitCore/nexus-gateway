package shell

import "testing"

func TestSplitHeights_ChatFocusGrowsChat(t *testing.T) {
	top, bottom := splitHeights(40, focusChat)
	if bottom <= top {
		t.Fatalf("chat-focused: chat pane should be the larger half, got top=%d bottom=%d", top, bottom)
	}
	if top+bottom != 40 {
		t.Fatalf("panes must fill the content height: top=%d bottom=%d want sum 40", top, bottom)
	}
}

func TestSplitHeights_CanvasFocusGrowsCanvas(t *testing.T) {
	top, bottom := splitHeights(40, focusCanvas)
	if top <= bottom {
		t.Fatalf("canvas-focused: canvas pane should be the larger half, got top=%d bottom=%d", top, bottom)
	}
	if top+bottom != 40 {
		t.Fatalf("panes must fill the content height: top=%d bottom=%d", top, bottom)
	}
}

func TestSplitHeights_RespectsMinimums(t *testing.T) {
	// Even canvas-focused, the chat keeps a usable minimum band.
	top, bottom := splitHeights(10, focusCanvas)
	if bottom < minChatHeight {
		t.Fatalf("chat band below minimum: bottom=%d min=%d", bottom, minChatHeight)
	}
	if top < minCanvasHeight {
		t.Fatalf("canvas below minimum: top=%d min=%d", top, minCanvasHeight)
	}
	if top+bottom != 10 {
		t.Fatalf("must still fill: top=%d bottom=%d", top, bottom)
	}
}

func TestEaseHeights_StartsAtPreviousFocus(t *testing.T) {
	total := 40
	fromTop, _ := splitHeights(total, focusCanvas) // easing toward chat → start = canvas split
	gotTop, gotBot := easeHeights(total, focusChat, 0)
	if gotTop != fromTop {
		t.Fatalf("frame 0 must equal the previous focus's split top: got %d want %d", gotTop, fromTop)
	}
	if gotTop+gotBot != total {
		t.Fatalf("must fill: %d+%d != %d", gotTop, gotBot, total)
	}
}

func TestEaseHeights_EndsAtTargetFocus(t *testing.T) {
	total := 40
	targetTop, _ := splitHeights(total, focusChat)
	gotTop, gotBot := easeHeights(total, focusChat, easeFrames)
	if gotTop != targetTop || gotTop+gotBot != total {
		t.Fatalf("final frame must equal the target split: got %d want %d", gotTop, targetTop)
	}
	if past, _ := easeHeights(total, focusChat, easeFrames+5); past != targetTop {
		t.Fatalf("beyond easeFrames must stay at the target: got %d want %d", past, targetTop)
	}
}

func TestEaseHeights_MidFrameBetweenStartAndTarget(t *testing.T) {
	total := 40
	fromTop, _ := splitHeights(total, focusCanvas) // larger top
	toTop, _ := splitHeights(total, focusChat)     // smaller top
	midTop, midBot := easeHeights(total, focusChat, easeFrames/2)
	if !(midTop < fromTop && midTop > toTop) {
		t.Fatalf("a mid-ease top should be between start(%d) and target(%d), got %d", fromTop, toTop, midTop)
	}
	if midTop+midBot != total {
		t.Fatalf("mid-ease must fill: %d+%d != %d", midTop, midBot, total)
	}
}

func TestSplitHeights_TinyTerminalSinglePane(t *testing.T) {
	// Below the combined minimum the focused pane takes everything.
	top, bottom := splitHeights(4, focusChat)
	if top != 0 || bottom != 4 {
		t.Fatalf("tiny + chat focus → chat full: got top=%d bottom=%d", top, bottom)
	}
	top, bottom = splitHeights(4, focusCanvas)
	if bottom != 0 || top != 4 {
		t.Fatalf("tiny + canvas focus → canvas full: got top=%d bottom=%d", top, bottom)
	}
}
