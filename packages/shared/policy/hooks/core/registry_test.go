package core

import (
	"strings"
	"testing"
)

func dummyFactory(_ *HookConfig) (Hook, error) {
	return &NoopHook{cfg: &HookConfig{ID: "dummy"}}, nil
}

func alternateFactory(_ *HookConfig) (Hook, error) {
	return &NoopHook{cfg: &HookConfig{ID: "alternate"}}, nil
}

// --- NewHookRegistry / Register --------------------------------------------

func TestHookRegistry_RegisterThenGet(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	if got := r.Get("alpha"); got == nil {
		t.Fatal("Get(alpha) returned nil after Register")
	}
}

func TestHookRegistry_Register_DuplicatePanics(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on duplicate Register")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "duplicate registration") {
			t.Errorf("panic msg: %q", msg)
		}
	}()
	r.Register("alpha", dummyFactory)
}

func TestHookRegistry_Register_FrozenPanics(t *testing.T) {
	r := NewHookRegistry()
	r.Freeze()
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on Register after Freeze")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "frozen") {
			t.Errorf("panic msg should mention frozen: %q", msg)
		}
	}()
	r.Register("alpha", dummyFactory)
}

func TestHookRegistry_Get_UnknownReturnsNil(t *testing.T) {
	r := NewHookRegistry()
	if r.Get("missing") != nil {
		t.Fatal("Get(missing) should return nil")
	}
}

// --- Replace ----------------------------------------------------------------

func TestHookRegistry_Replace_OverridesExisting(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	r.Replace("alpha", alternateFactory)
	h, err := r.Get("alpha")(nil)
	if err != nil {
		t.Fatalf("factory call: %v", err)
	}
	noop, ok := h.(*NoopHook)
	if !ok {
		t.Fatalf("expected *NoopHook, got %T", h)
	}
	if noop.cfg.ID != "alternate" {
		t.Errorf("Replace did not override; cfg.ID = %q want alternate", noop.cfg.ID)
	}
}

func TestHookRegistry_Replace_AddsWhenAbsent(t *testing.T) {
	// Replace must NOT require a prior Register (unlike many registry
	// patterns). The docstring says "Replace overrides an existing factory"
	// but the implementation simply writes the slot, so absent IDs are
	// inserted. Verify this observable behavior.
	r := NewHookRegistry()
	r.Replace("never-registered", dummyFactory)
	if r.Get("never-registered") == nil {
		t.Error("Replace should insert when ID was absent")
	}
}

func TestHookRegistry_Replace_FrozenPanics(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	r.Freeze()
	defer func() {
		rec := recover()
		if rec == nil {
			t.Fatal("expected panic on Replace after Freeze")
		}
		msg, _ := rec.(string)
		if !strings.Contains(msg, "frozen") {
			t.Errorf("panic msg should mention frozen: %q", msg)
		}
	}()
	r.Replace("alpha", alternateFactory)
}

// --- Clone ------------------------------------------------------------------

func TestHookRegistry_Clone_CopiesAllFactories(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	r.Register("beta", dummyFactory)
	r.Freeze()

	c := r.Clone()
	if c.Get("alpha") == nil || c.Get("beta") == nil {
		t.Fatal("clone missing factories from source")
	}
}

func TestHookRegistry_Clone_IsUnfrozen(t *testing.T) {
	// Clone must produce an unfrozen registry so consumers can extend it
	// (e.g. swap a service-specific impl) — this is the documented contract.
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	r.Freeze()
	c := r.Clone()
	// Should be able to Register on the clone without panic.
	c.Register("gamma", dummyFactory)
	if c.Get("gamma") == nil {
		t.Error("Register on clone should have added 'gamma'")
	}
}

func TestHookRegistry_Clone_IndependentFromSource(t *testing.T) {
	// Mutations on the clone must NOT bleed back into the source.
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	c := r.Clone()
	c.Register("clone-only", dummyFactory)
	if r.Get("clone-only") != nil {
		t.Error("clone mutation leaked back to source")
	}
}

// --- All --------------------------------------------------------------------

func TestHookRegistry_All_ReturnsSnapshot(t *testing.T) {
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)
	r.Register("beta", dummyFactory)

	snap := r.All()
	if len(snap) != 2 {
		t.Fatalf("All() returned %d entries, want 2", len(snap))
	}
	if _, ok := snap["alpha"]; !ok {
		t.Error("snapshot missing alpha")
	}
	if _, ok := snap["beta"]; !ok {
		t.Error("snapshot missing beta")
	}
}

func TestHookRegistry_All_SnapshotIsIndependentCopy(t *testing.T) {
	// Mutating the returned map MUST NOT affect the registry's internal state.
	r := NewHookRegistry()
	r.Register("alpha", dummyFactory)

	snap := r.All()
	delete(snap, "alpha")
	if r.Get("alpha") == nil {
		t.Error("deleting from snapshot affected the registry")
	}
	snap["foo"] = dummyFactory
	if r.Get("foo") != nil {
		t.Error("adding to snapshot affected the registry")
	}
}

func TestHookRegistry_All_EmptyRegistry(t *testing.T) {
	r := NewHookRegistry()
	if got := r.All(); len(got) != 0 {
		t.Errorf("empty registry: got %d entries, want 0", len(got))
	}
}

// TestDefaultRegistry_AllExpectedFactoriesPresent is now in the parent
// hooks package (aliases_test.go) where the global Registry is defined.
