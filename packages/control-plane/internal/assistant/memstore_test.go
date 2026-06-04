package assistant

import (
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

func TestMemMemoryLifecycle(t *testing.T) {
	m := newMemMemory()
	if idx, _ := m.Index(); idx != "" {
		t.Fatalf("empty store must yield empty index, got %q", idx)
	}
	if err := m.Remember(agent.MemoryFact{Name: "region", Type: "entity", Description: "primary region", Body: "us-east"}); err != nil {
		t.Fatal(err)
	}
	f, ok, _ := m.Recall("region")
	if !ok || f.Body != "us-east" {
		t.Fatalf("recall failed: ok=%v body=%q", ok, f.Body)
	}
	if idx, _ := m.Index(); !strings.Contains(idx, "region") || !strings.Contains(idx, "primary region") {
		t.Fatalf("index must list the fact, got %q", idx)
	}
	if err := m.Update("region", "us-west"); err != nil {
		t.Fatal(err)
	}
	if f, _, _ := m.Recall("region"); f.Body != "us-west" {
		t.Fatalf("update did not take, got %q", f.Body)
	}
	if err := m.Update("ghost", "x"); err == nil {
		t.Fatal("updating a missing fact must error")
	}
	// A fact with no description falls back to body in the index.
	_ = m.Remember(agent.MemoryFact{Name: "nodesc", Type: "preference", Body: "terse"})
	if idx, _ := m.Index(); !strings.Contains(idx, "terse") {
		t.Fatalf("no-description fact must index its body, got %q", idx)
	}
	removed, _ := m.Forget("region")
	if !removed {
		t.Fatal("forget must report removal")
	}
	if _, ok, _ := m.Recall("region"); ok {
		t.Fatal("forgotten fact must not recall")
	}
	if removed, _ := m.Forget("region"); removed {
		t.Fatal("double-forget must report not-removed")
	}
}

func TestMemStoreLifecycle(t *testing.T) {
	s := newMemStore()
	sess := agent.NewSession("web")
	if err := s.Save(sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(sess.ID)
	if err != nil || loaded.ID != sess.ID {
		t.Fatalf("load failed: %v", err)
	}
	if _, err := s.Load("does-not-exist"); err == nil {
		t.Fatal("loading a missing session must error")
	}
	metas, _ := s.List()
	if len(metas) != 1 || metas[0].ID != sess.ID {
		t.Fatalf("list must return the one saved session, got %d", len(metas))
	}
}
