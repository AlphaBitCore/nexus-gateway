package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

func TestCanvasNavigateTool(t *testing.T) {
	c := &fakeCanvas{}
	nav := toolByName(canvasTools(c), "navigate")
	if nav.Tier() != agent.TierAuto {
		t.Fatal("navigate is auto-tier (driving the UI has no production side effect)")
	}
	res, err := nav.Run(context.Background(), rawArgs(map[string]any{"view": "radar", "status": "error", "model": "gpt-4o"}))
	if err != nil || res.IsError {
		t.Fatalf("navigate should succeed, got %+v err %v", res, err)
	}
	if c.view != "radar" || c.filter.StatusRange != "error" || c.filter.ModelUsed != "gpt-4o" {
		t.Fatalf("navigate must drive the canvas with view+filter, got view=%q filter=%+v", c.view, c.filter)
	}
}

func TestCanvasNavigateRequiresView(t *testing.T) {
	res, _ := toolByName(canvasTools(&fakeCanvas{}), "navigate").Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "view is required") {
		t.Fatalf("navigate must require a view, got %+v", res)
	}
}

func TestCanvasShowEventAndHighlight(t *testing.T) {
	c := &fakeCanvas{}
	if res, _ := toolByName(canvasTools(c), "show_event").Run(context.Background(), rawArgs(map[string]any{"id": "ev-9a3f"})); res.IsError || c.shownEvent != "ev-9a3f" {
		t.Fatalf("show_event must drive the canvas, got res=%+v shown=%q", res, c.shownEvent)
	}
	if res, _ := toolByName(canvasTools(c), "highlight").Run(context.Background(), rawArgs(map[string]any{"ref": "openai"})); res.IsError || c.highlighted != "openai" {
		t.Fatalf("highlight must drive the canvas, got res=%+v hi=%q", res, c.highlighted)
	}
}

func TestCanvasShowEventRequiresID(t *testing.T) {
	res, _ := toolByName(canvasTools(&fakeCanvas{}), "show_event").Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "id is required") {
		t.Fatalf("show_event must require an id, got %+v", res)
	}
}

func TestCanvasHighlightRequiresRef(t *testing.T) {
	res, _ := toolByName(canvasTools(&fakeCanvas{}), "highlight").Run(context.Background(), json.RawMessage(`{}`))
	if !res.IsError || !strings.Contains(res.Content, "ref is required") {
		t.Fatalf("highlight must require a ref, got %+v", res)
	}
}

func TestCanvasErrorsSurfaceAsResults(t *testing.T) {
	c := &fakeCanvas{err: errCanvasTest}
	tools := canvasTools(c)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"navigate", map[string]any{"view": "cost"}},
		{"show_event", map[string]any{"id": "ev-1"}},
		{"highlight", map[string]any{"ref": "openai"}},
	} {
		res, err := toolByName(tools, tc.tool).Run(context.Background(), rawArgs(tc.args))
		if err != nil {
			t.Fatalf("%s: a canvas error is a recoverable tool result, not a Go error", tc.tool)
		}
		if !res.IsError || !strings.Contains(res.Content, "canvas boom") {
			t.Fatalf("%s: canvas error must surface as an error result, got %+v", tc.tool, res)
		}
	}
}
