package core

import (
	"fmt"
	"sync"
)

// Registry maps [Format] values to [Adapter] instances. Reads after
// [Registry.Freeze] are lock-free; writes after Freeze panic.
type Registry struct {
	mu       sync.RWMutex
	adapters map[Format]Adapter
	frozen   bool
}

// NewRegistry creates an empty registry. Typical call order at startup
// is RegisterBuiltins(reg, log) followed by reg.Freeze().
func NewRegistry() *Registry {
	return &Registry{adapters: make(map[Format]Adapter)}
}

// Register adds an adapter. Returns an error for duplicate formats and
// panics if called after [Registry.Freeze] (programming error).
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("providers: nil adapter")
	}
	f := a.Format()
	if !f.Valid() {
		return fmt.Errorf("providers: invalid format %q", f)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.frozen {
		panic(fmt.Sprintf("providers: cannot register %q after registry is frozen", f))
	}
	if _, exists := r.adapters[f]; exists {
		return fmt.Errorf("providers: duplicate registration for format %q", f)
	}
	r.adapters[f] = a
	return nil
}

// MustRegister is [Registry.Register] that panics on error. Intended
// for RegisterBuiltins: startup failures should abort the process.
func (r *Registry) MustRegister(a Adapter) {
	if err := r.Register(a); err != nil {
		panic(err.Error())
	}
}

// Freeze prevents further registrations. Called after startup init.
func (r *Registry) Freeze() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.frozen = true
}

// Get returns the adapter for the given format. Unknown formats yield
// (nil, false) — there is no silent openai fallback; callers must
// decide how to surface "no compatible provider".
func (r *Registry) Get(f Format) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[f]
	return a, ok
}

// List returns every registered format in stable order (matches
// [AllFormats] when the registry is complete).
func (r *Registry) List() []Format {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Format, 0, len(r.adapters))
	for _, f := range AllFormats() {
		if _, ok := r.adapters[f]; ok {
			out = append(out, f)
		}
	}
	return out
}
