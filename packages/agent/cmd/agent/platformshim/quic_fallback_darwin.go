//go:build darwin

package platformshim

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// quicFallbackFilePath is the canonical location the NE system extension
// reads. Owned by the daemon (root); world-readable so the unsandboxed
// extension process can stat + read it on its 60s refresh tick.
const quicFallbackFilePath = "/var/run/nexus-agent/quic-bundles.json"

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
