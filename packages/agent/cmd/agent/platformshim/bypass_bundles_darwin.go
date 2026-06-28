//go:build darwin

package platformshim

import (
	"fmt"
	"github.com/goccy/go-json"
	"log/slog"
	"os"
	"path/filepath"
)

// bypassBundlesFilePath is the canonical location the NE system extension
// reads to decide which source-app bundles to pass through WITHOUT a TLS
// bump (self-exemption). Owned by the daemon (root); world-readable so the
// unsandboxed extension process can stat + read it on its refresh tick. A
// var (not a const) only so tests can redirect it to a temp dir.
var bypassBundlesFilePath = "/var/run/nexus-agent/bypass-bundles.json"

// WriteBypassBundlesFile writes the source-bundle exemption list the NE
// extension consumes. A flow whose sourceAppSigningIdentifier matches an
// entry is passed through to native routing — no bump, no daemon bridge —
// so an admin can keep a trusted developer tool (e.g. the local
// claude-code CLI) off the inspection path without blinding the agent to
// the same destination host from OTHER apps. Matching is by SOURCE bundle,
// never by host, precisely so destination visibility is preserved.
//
// Atomic write (tmp + rename) so the NE never reads half a document.
// nil/empty writes the literal "[]" — the empty-as-fail-safe contract: an
// empty list means "exempt nothing" (inspect everything), which is the
// safe default. No protected-bundle stripping here (unlike the QUIC list):
// exempting a bundle only REMOVES it from inspection, which is harmless for
// system daemons (they are declined/passed through anyway) and cannot close
// any UDP socket.
//
// Why a flat file and not IPC: NE's handleNewFlow MUST decide
// synchronously, one call per outbound flow. A blocking IPC roundtrip per
// flow would dominate connection latency. File read is O(1) per refresh
// tick and survives daemon restarts (NE keeps its last-known list).
func WriteBypassBundlesFile(bundles []string, logger *slog.Logger) error {
	dir := filepath.Dir(bypassBundlesFilePath)
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
	tmp := bypassBundlesFilePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, bypassBundlesFilePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tmp→final: %w", err)
	}
	if logger != nil {
		logger.Info("wrote bypass-bundles.json",
			"path", bypassBundlesFilePath,
			"count", len(bundles),
			"bytes", len(data),
		)
	}
	return nil
}
