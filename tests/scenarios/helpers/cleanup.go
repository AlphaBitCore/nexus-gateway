package helpers

import (
	"sync"
	"testing"
)

// Cleanup is the scenario harness's resource teardown registry. Scenarios
// register a cleanup function for every server-side resource they create
// (VK, routing rule, hook, org, ...) via Register, and the registered
// functions run in LIFO order at the end of the test.
//
// Two reasons we wrap the stdlib t.Cleanup pattern:
//
//  1. Single-line registration that returns the resource ID for fluent
//     "create-then-use" chains: id := admin.CreateVK(...) // already
//     registered.
//  2. Errors during cleanup are non-fatal but logged via t.Logf, so a
//     downstream test that depends on a missing teardown still gets a
//     readable trail rather than a silent leak.
type Cleanup struct {
	mu   sync.Mutex
	fns  []func() error
	t    *testing.T
}

// NewCleanup binds a cleanup registry to t. The registered fns are flushed
// in LIFO order on t.Cleanup.
func NewCleanup(t *testing.T) *Cleanup {
	t.Helper()
	c := &Cleanup{t: t}
	t.Cleanup(c.flush)
	return c
}

// Register adds fn to the cleanup queue. label is shown on failure.
func (c *Cleanup) Register(label string, fn func() error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	wrapped := func() error {
		err := fn()
		if err != nil {
			c.t.Logf("cleanup[%s]: %v", label, err)
		}
		return err
	}
	c.fns = append(c.fns, wrapped)
}

func (c *Cleanup) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.fns) - 1; i >= 0; i-- {
		_ = c.fns[i]()
	}
	c.fns = nil
}
