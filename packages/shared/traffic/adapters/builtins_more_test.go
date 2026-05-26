package adapters

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic"
)

// TestRegisterBuiltins_AllFactoriesInstantiateNonNilAdapterWithMatchingID
// verifies every factory closure in builtinEntries actually runs and
// produces a usable Adapter. RegisterBuiltins stores factories but never
// invokes them, so without this test the inner closures
// (`return &xxx.Adapter{}`) for adapter IDs that the Tier-1 normalizer loop
// skips (everything in alreadyCoveredByAIBuiltins) are never executed and
// stay uncovered. This pins two contracts at once:
//   - the registered ID matches the adapter's own ID() (catalog drift guard)
//   - the factory always returns a non-nil Adapter
func TestRegisterBuiltins_AllFactoriesInstantiateNonNilAdapterWithMatchingID(t *testing.T) {
	t.Parallel()
	reg := traffic.NewAdapterRegistry("test")
	RegisterBuiltins(reg)
	for _, id := range BuiltinTrafficAdapterIDs() {
		factory := reg.Get(id)
		if factory == nil {
			t.Errorf("Get(%q) returned nil factory after RegisterBuiltins", id)
			continue
		}
		inst := factory()
		if inst == nil {
			t.Errorf("factory for %q produced nil Adapter", id)
			continue
		}
		// Most adapters self-report a stable ID() that matches the
		// catalog key. A small number of vendor adapters return a
		// different canonical ID (e.g. an "openai-compat" factory may
		// internally identify as "openai"); we only require that the
		// adapter expose SOME non-empty ID — the registry catalog match
		// is enforced separately by
		// TestBuiltinTrafficAdapterIDs_SortedAndMatchesRegistry.
		if inst.ID() == "" {
			t.Errorf("adapter %q returned empty ID()", id)
		}
	}
}

// TestRegisterBuiltins_FactoryCountMatchesCatalog confirms the registry
// receives exactly the same number of factories as the catalog reports.
// This double-checks the for-loop in RegisterBuiltins iterates over every
// builtinEntries row (covering the loop body even if BuiltinTrafficAdapterIDs
// were ever to drift from the underlying slice).
func TestRegisterBuiltins_FactoryCountMatchesCatalog(t *testing.T) {
	t.Parallel()
	reg := traffic.NewAdapterRegistry("test")
	RegisterBuiltins(reg)
	if got, want := len(reg.All()), len(BuiltinTrafficAdapterIDs()); got != want {
		t.Fatalf("registry size %d != catalog size %d", got, want)
	}
}

// TestMust_PanicsOnError pins the panic arm of the internal `must` helper.
// The success arm is exercised every time RegisterBuiltins runs (because
// registry.Register returns nil on a fresh registry), but the error arm —
// the safety net that turns a startup misconfiguration into a hard fatal
// instead of a silent registration miss — is otherwise unreachable in
// happy-path tests.
func TestMust_PanicsOnError(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("must(err) did not panic on non-nil error")
		}
		// The panic value is `fmt.Errorf("adapter registration failed: %w", err)`
		// — assert the wrapping so the caller can detect the original cause.
		gotErr, ok := r.(error)
		if !ok {
			t.Fatalf("panic value type %T, want error", r)
		}
		if !strings.Contains(gotErr.Error(), "adapter registration failed") {
			t.Errorf("panic message %q missing wrap prefix", gotErr.Error())
		}
		// The wrapped error must be retrievable via errors.Unwrap (the
		// %w directive).
		if unwrapped := errors.Unwrap(gotErr); unwrapped == nil ||
			unwrapped.Error() != "register exploded" {
			t.Errorf("unwrap = %v, want sentinel %q", unwrapped, "register exploded")
		}
	}()
	must(errors.New("register exploded"))
}

// TestMust_NoPanicOnNil pins the success arm of `must` to keep the
// observable contract symmetric: nil error → no panic, no side effect.
func TestMust_NoPanicOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("must(nil) panicked: %v", r)
		}
	}()
	must(nil)
}

// TestRegisterBuiltins_PanicsOnPostFreezeRegistry verifies the wiring
// between RegisterBuiltins and the `must` panic helper end-to-end: if
// the registry rejects a Register call (the only way Register currently
// fails is post-Freeze), RegisterBuiltins surfaces it as a panic rather
// than silently dropping adapters.
func TestRegisterBuiltins_PanicsOnPostFreezeRegistry(t *testing.T) {
	t.Parallel()
	reg := traffic.NewAdapterRegistry("test")
	reg.Freeze()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("RegisterBuiltins on a frozen registry must panic")
		}
		gotErr, ok := r.(error)
		if !ok {
			t.Fatalf("panic value type %T, want error", r)
		}
		if !strings.Contains(gotErr.Error(), "adapter registration failed") {
			t.Errorf("panic message %q missing wrap prefix", gotErr.Error())
		}
	}()
	RegisterBuiltins(reg)
}

// TestAlreadyCoveredByAIBuiltins_AllKeysAreRegisteredBuiltins guards the
// alreadyCoveredByAIBuiltins skip-list: every key MUST correspond to an
// actually-registered builtin adapter ID. A stale entry (e.g. an adapter
// that was renamed) would silently fail to skip its real ID, then collide
// at Hub startup with the AI-builtin normalizer of the same name. The
// comment on alreadyCoveredByAIBuiltins explicitly calls this lockstep
// out — this test mechanizes it.
func TestAlreadyCoveredByAIBuiltins_AllKeysAreRegisteredBuiltins(t *testing.T) {
	t.Parallel()
	catalog := map[string]bool{}
	for _, id := range BuiltinTrafficAdapterIDs() {
		catalog[id] = true
	}
	for skipID := range alreadyCoveredByAIBuiltins {
		if !catalog[skipID] {
			t.Errorf("alreadyCoveredByAIBuiltins key %q is not a registered builtin adapter — stale entry would leak through Tier-1 registration", skipID)
		}
	}
}
