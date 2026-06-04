package views

import (
	tea "charm.land/bubbletea/v2"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
	"strings"
	"testing"
)

func TestModelsPick_EnterEmitsSetModel(t *testing.T) {
	v := newModels(sampleGateway())
	upd, _ := v.Update(v.fetch()()) // load the catalog
	v = upd.(*ModelsView)
	if len(v.flatModels()) == 0 {
		t.Fatal("precondition: the sample catalog must have models")
	}
	v.EnterPick("some-current-model")
	if !v.pick {
		t.Fatal("EnterPick must put the view in pick mode")
	}
	_, cmd := v.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter in pick mode must emit a command")
	}
	sm, ok := cmd().(kit.SetModelMsg)
	if !ok || sm.Code == "" {
		t.Fatalf("enter in pick mode must emit setModelMsg with the row's model code, got %#v", cmd())
	}
}

func TestModelsPick_EnterOpensDetailWhenNotPicking(t *testing.T) {
	v := newModels(sampleGateway())
	upd, _ := v.Update(v.fetch()())
	v = upd.(*ModelsView)
	v.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // not in pick mode → detail drawer
	if !v.detail {
		t.Fatal("enter outside pick mode must open the detail drawer (unchanged behavior)")
	}
}

func TestModelsPick_MarksCurrentModel(t *testing.T) {
	v := newModels(sampleGateway())
	upd, _ := v.Update(v.fetch()())
	v = upd.(*ModelsView)
	cur := v.flatModels()[0].m.Code
	v.EnterPick(cur)
	if !strings.Contains(v.View(120, 30), "●") {
		t.Fatalf("the current chat model must be marked in the catalog:\n%s", v.View(120, 30))
	}
}
