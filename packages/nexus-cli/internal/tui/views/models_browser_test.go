package views

import (
	tea "charm.land/bubbletea/v2"
	"errors"
	"fmt"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/core"
	"strings"
	"testing"
)

// TestModels_CatalogBrowser verifies the catalog renders friendly provider
// labels, model code+name, context window, pricing, enabled state, and scrolls.
func TestModels_CatalogBrowser(t *testing.T) {
	gw := &fakeGateway{models: &core.ModelCatalog{Data: []core.ModelGroup{
		{Provider: core.Provider{ID: "p1", Name: "anthropic", DisplayName: "Anthropic"}, Models: []core.Model{
			{Code: "claude-sonnet-4-6", Name: "Claude Sonnet 4.6", Type: "chat", Enabled: true, MaxContextTokens: 200000, InputPricePerMillion: 3, OutputPricePerMillion: 15},
			{Code: "claude-old", Name: "Claude Old", Type: "chat", Enabled: false, MaxContextTokens: 100000},
		}},
	}}}
	m := newModels(gw)
	if !strings.Contains(m.View(120, 20), "loading") {
		t.Fatal("initial catalog shows loading")
	}
	v, cmd := m.Update(m.Init()())
	if cmd == nil {
		t.Fatal("catalog schedules a poll tick")
	}
	out := v.View(120, 20)
	for _, want := range []string{"Model catalog", "Anthropic", "claude-sonnet-4-6", "Claude Sonnet 4.6", "200k", "$3.00", "$15.00", "on", "off"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog view missing %q:\n%s", want, out)
		}
	}
	if _, c := v.Update(modelsTick{}); c == nil {
		t.Fatal("modelsTick should refetch")
	}

	// error + empty paths.
	er := newModels(&fakeGateway{err: errors.New("catalog-down")})
	ev, _ := er.Update(er.Init()())
	if !strings.Contains(ev.View(120, 20), "catalog-down") {
		t.Fatal("catalog error should surface")
	}
	empty := newModels(&fakeGateway{models: &core.ModelCatalog{}})
	emv, _ := empty.Update(empty.Init()())
	if !strings.Contains(emv.View(120, 20), "no models") {
		t.Fatal("empty catalog placeholder")
	}
}

// TestModels_CursorScrollAndDrill covers the row cursor (with auto-scroll in a
// short viewport) and the enter → detail drawer surfacing type/status.
func TestModels_CursorScrollAndDrill(t *testing.T) {
	var models []core.Model
	for i := 0; i < 20; i++ {
		models = append(models, core.Model{
			Code: fmt.Sprintf("m-%d", i), Name: fmt.Sprintf("Model %d", i),
			Type: "chat", Status: "active", Enabled: true, MaxContextTokens: 128000,
			InputPricePerMillion: 0.15, OutputPricePerMillion: 0.60,
		})
	}
	gw := &fakeGateway{models: &core.ModelCatalog{Data: []core.ModelGroup{
		{Provider: core.Provider{Name: "openai", DisplayName: "OpenAI"}, Models: models},
	}}}
	m := newModels(gw)
	v, _ := m.Update(m.Init()())
	mv := v.(*ModelsView)
	// up at the top row is a no-op.
	mv.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if mv.cursor != 0 {
		t.Fatalf("up at the top should stay at row 0, got %d", mv.cursor)
	}
	// drive the cursor to the bottom; the view auto-scrolls to keep it visible.
	for i := 0; i < 50; i++ {
		mv.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if mv.cursor != len(models)-1 {
		t.Fatalf("the cursor should clamp at the last model, got %d", mv.cursor)
	}
	out := mv.View(100, 8) // short viewport
	if !strings.Contains(out, "m-19") {
		t.Fatalf("the catalog should auto-scroll the cursor row into view:\n%s", out)
	}
	if !strings.Contains(mv.Help(), "enter open") {
		t.Fatalf("list help should advertise enter open, got %q", mv.Help())
	}
	// enter opens the detail drawer, which surfaces the type + status the list omits.
	mv.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !mv.detail {
		t.Fatal("enter should open the model detail drawer")
	}
	det := mv.View(100, 20)
	for _, want := range []string{"m-19", "Type", "chat", "Status", "active", "$0.15"} {
		if !strings.Contains(det, want) {
			t.Errorf("the model drawer should show %q:\n%s", want, det)
		}
	}
	if !strings.Contains(mv.Help(), "esc back") {
		t.Fatalf("detail help should advertise esc back, got %q", mv.Help())
	}
	// back closes the drawer; a second back at the list level declines.
	if !mv.Back() || mv.detail {
		t.Fatal("back should close the drawer")
	}
	if mv.Back() {
		t.Fatal("back at the list level must return false so the root pops the nav stack")
	}
	// a very short viewport clamps the budget without panicking.
	if mv.View(100, 3) == "" {
		t.Fatal("short viewport should still render")
	}
}
