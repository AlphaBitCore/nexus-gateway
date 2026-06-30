package pipeline

import (
	"io"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/hooks/core"
)

// Prewarm eagerly builds and caches the hook for every enabled request/response
// config so the factory's compile cost (notably the Vectorscan database, ~100s
// of ms) runs OFF the request path — at startup and in the background after each
// Swap. It is best-effort and idempotent:
//
//   - Hooks are built WITHOUT holding hookMu (the compile must not block
//     resolve()), then inserted under the lock with a double-check.
//   - It is guarded by swapGen: if a Swap races while Prewarm is building, the
//     captured generation no longer matches and Prewarm aborts without caching,
//     so it can never install a hook for a superseded config. The newer Swap
//     spawns its own Prewarm for the current snapshot.
//   - Connection-stage hooks are skipped (they are cheap metadata hooks and
//     resolve() applies an extra connection-compat gate before caching them).
//
// A hook left unbuilt (factory error, or aborted prewarm) is simply built
// lazily by the next resolve(), exactly as before — prewarm only removes the
// first-request latency spike, never changes correctness.
func (r *PolicyResolver) Prewarm() {
	gen, configs := r.loadSnapshot()
	for i := range configs {
		cfg := &configs[i]
		if !cfg.Enabled || strings.EqualFold(cfg.Stage, "connection") {
			continue
		}
		factory := r.registry.Get(cfg.ImplementationID)
		if factory == nil {
			continue
		}
		r.hookMu.RLock()
		_, hit := r.hookCache[cfg.ID]
		r.hookMu.RUnlock()
		if hit {
			continue
		}
		// Build outside the lock — the compile is the expensive part and must
		// not stall concurrent resolve() readers.
		hook, err := factory(cfg)
		if err != nil {
			continue // resolve() will log+skip per its fail posture
		}
		r.hookMu.Lock()
		switch {
		case r.swapGen.Load() != gen:
			// A swap raced; this snapshot may be stale. Drop our build.
			r.hookMu.Unlock()
			closeHook(hook)
		case r.hookCache[cfg.ID] != nil:
			// Someone (resolve or a concurrent prewarm) already built it.
			r.hookMu.Unlock()
			closeHook(hook)
		default:
			r.hookCache[cfg.ID] = hook
			r.hookMu.Unlock()
		}
	}
}

// closeHook releases a built-but-unused hook's resources (e.g. a Vectorscan
// matcher's cgo memory) when prewarm discards it.
func closeHook(h core.Hook) {
	if c, ok := h.(io.Closer); ok {
		_ = c.Close()
	}
}
