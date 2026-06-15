//go:build darwin

package platformshim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/systembundles"
)

// quicFallbackFilePath is the canonical location the NE system extension
// reads. Owned by the daemon (root); world-readable so the unsandboxed
// extension process can stat + read it on its 60s refresh tick. A var (not a
// const) only so tests can redirect it to a temp dir — production never
// reassigns it.
var quicFallbackFilePath = "/var/run/nexus-agent/quic-bundles.json"

// writeQUICFallbackBundlesFile writes the bundle-ID allowlist that the
// NE extension consumes. Atomic write: tmp file + rename so a partial
// write never leaves the NE reading half a JSON document. Idempotent:
// nil/empty slice writes the literal "[]" so the NE knows admin
// explicitly cleared the list (no special "missing" semantics).
//
// Why a flat file and not IPC: NE's handleNewFlow MUST decide
// synchronously and is hot-path (one call per outbound flow). A
// blocking IPC roundtrip per flow would dominate connection latency.
// File read is O(1) per refresh tick (60s) and survives daemon
// restarts cleanly (NE keeps its last-known list).
func WriteQUICFallbackBundlesFile(bundles []string, logger *slog.Logger) error {
	dir := filepath.Dir(quicFallbackFilePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if bundles == nil {
		bundles = []string{}
	}
	// Defense-in-depth: strip any entry that would close UDP for a
	// macOS system networking/push/continuity daemon BEFORE it reaches the file
	// the NE reads. The CP write path already rejects these, but a hostile node that
	// pushes the agent_settings shadow directly bypasses that handler — this is
	// the last Go-side gate before the safety-critical NE honors the list
	// (CLAUDE.md NE rule 5). Stripped entries are logged loudly, never silently.
	if clean, stripped := systembundles.Filter(bundles); len(stripped) > 0 {
		if logger != nil {
			logger.Warn("quic-bundles.json: stripped protected system-service entries",
				"stripped", stripped, "kept", len(clean))
		}
		bundles = clean
	}
	data, err := json.Marshal(bundles)
	if err != nil {
		return fmt.Errorf("marshal bundles: %w", err)
	}
	tmp := quicFallbackFilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, quicFallbackFilePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp→final: %w", err)
	}
	if logger != nil {
		logger.Info("wrote quic-bundles.json",
			"path", quicFallbackFilePath,
			"count", len(bundles),
			"bytes", len(data),
		)
	}
	return nil
}

// anySlice converts a []string to []any for embedding in the
// remoteOverlay map MergeConfig expects (its slice handlers use the
// JSON-decoded []any shape, not the typed []string shape).
func AnySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
