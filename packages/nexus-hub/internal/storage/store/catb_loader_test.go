package store

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

type stubLoader struct {
	state   any
	version int64
	err     error
}

func (s *stubLoader) Load(_ context.Context, _ string) (any, int64, error) {
	return s.state, s.version, s.err
}

func TestCatBRegistry_RegisterAndLookup(t *testing.T) {
	r := NewCatBRegistry()
	l := &stubLoader{state: map[string]any{"hookConfigs": []any{}}, version: 42}
	r.Register("agent", "hook_config", l)

	got, ok := r.Lookup("agent", "hook_config")
	if !ok {
		t.Fatal("Lookup miss on just-registered key")
	}
	if got != l {
		t.Errorf("Lookup returned a different loader instance")
	}

	state, ver, err := got.Load(context.Background(), "thing-xyz")
	if err != nil {
		t.Fatalf("stub Load err=%v", err)
	}
	if ver != 42 {
		t.Errorf("version = %d want 42", ver)
	}
	// Sanity-check the state still serialises as-is.
	raw, _ := json.Marshal(state)
	if string(raw) != `{"hookConfigs":[]}` {
		t.Errorf("state marshalled to %s", raw)
	}
}

func TestCatBRegistry_LookupMiss(t *testing.T) {
	r := NewCatBRegistry()
	if _, ok := r.Lookup("agent", "unregistered_key"); ok {
		t.Fatal("expected miss on unregistered key")
	}
	if _, ok := r.Lookup("does-not-exist", "hook_config"); ok {
		t.Fatal("expected miss on unknown thingType")
	}
}

func TestCatBRegistry_NilReceiverSafe(t *testing.T) {
	var r *CatBRegistry
	// Both operations must be safe on a nil pointer — this is the
	// escape hatch that lets Hub skip Cat B entirely when the operator
	// has not wired the registry yet.
	if _, ok := r.Lookup("agent", "hook_config"); ok {
		t.Fatal("nil-receiver Lookup must miss")
	}
	r.Register("agent", "hook_config", &stubLoader{})
}

func TestCatBRegistry_OverwriteLastWriteWins(t *testing.T) {
	r := NewCatBRegistry()
	first := &stubLoader{version: 1}
	second := &stubLoader{version: 2}
	r.Register("agent", "hook_config", first)
	r.Register("agent", "hook_config", second)

	got, _ := r.Lookup("agent", "hook_config")
	if got != second {
		t.Fatal("re-register must replace the previous loader")
	}
}

func TestCatBRegistry_ConcurrentAccess(t *testing.T) {
	r := NewCatBRegistry()
	var wg sync.WaitGroup
	// Writers register under different keys while readers lookup a
	// known key; race detector catches any unsynchronised map access.
	for i := range 50 {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.Register("agent", "key_"+itoa(i), &stubLoader{})
		}(i)
		go func() {
			defer wg.Done()
			_, _ = r.Lookup("agent", "hook_config")
		}()
	}
	wg.Wait()
}

// TestCatBRegistry_Register_NilLoadersMap verifies the defensive branch in
// Register that re-initialises a nil loaders map. This branch is reached when
// a CatBRegistry is constructed by value copy (bypassing NewCatBRegistry) so
// the internal map was never initialised.
func TestCatBRegistry_Register_NilLoadersMap(t *testing.T) {
	// Struct literal — loaders field stays nil.
	r := &CatBRegistry{}
	l := &stubLoader{version: 99}
	r.Register("agent", "agent_settings", l) // must not panic
	got, ok := r.Lookup("agent", "agent_settings")
	if !ok || got != l {
		t.Error("Register on nil-loaders-map must store and return the loader")
	}
}

// itoa avoids pulling in strconv just for a test helper.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
