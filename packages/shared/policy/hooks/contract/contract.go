// Package contract holds canonical HookConfig + HookInput examples and a
// Suite function that every service embedding shared/hooks must run.
//
// The contract pins two invariants:
//
//  1. Each built-in ImplementationID accepts a documented minimal config
//     without error.
//  2. Each example hook's Execute against a canonical input returns the
//     documented decision.
//
// Any schema change in shared/hooks that would silently break consumers
// (factory config shape, HookInput/HookResult fields, decision constants)
// shows up as a compilation or runtime failure in every consumer's test
// binary.
package contract

import (
	"context"
	"fmt"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/builtins"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Suite runs the full contract matrix against builtins.Registry. Call from
// each consumer's *_test.go as t.Run("contract", contract.Suite) — or just
// contract.Suite(t) directly if the consumer only runs the shared fixtures.
func Suite(t *testing.T) {
	t.Helper()
	for _, ex := range Examples() {
		t.Run(ex.Name, func(t *testing.T) {
			if err := runExample(builtins.Registry, ex); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// runExample executes a single contract Example against the given registry
// and returns an error describing the first invariant breach. Extracted
// from Suite so the four failure branches (missing factory, factory error,
// Execute error, decision mismatch) are unit-testable without faking
// *testing.T. Behaviour from Suite's caller perspective is unchanged.
func runExample(registry *core.HookRegistry, ex Example) error {
	factory := registry.Get(ex.Config.ImplementationID)
	if factory == nil {
		return fmt.Errorf("no factory registered for impl %q", ex.Config.ImplementationID)
	}
	h, err := factory(&ex.Config)
	if err != nil {
		return fmt.Errorf("factory(%q) error: %w", ex.Name, err)
	}
	res, err := h.Execute(context.Background(), ex.Input)
	if err != nil {
		return fmt.Errorf("Execute error: %w", err)
	}
	if res.Decision != ex.ExpectedDecision {
		return fmt.Errorf("%s: got %s, want %s", ex.Name, res.Decision, ex.ExpectedDecision)
	}
	return nil
}
