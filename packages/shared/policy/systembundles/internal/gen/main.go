// Command gen renders the macOS NE Swift copy of the system-protection floor
// from the Go SSOT in packages/shared/policy/systembundles. It reads
// systembundles.ProtectedBundles plus its normalize()/related() matching
// semantics and emits SystemBundles.generated.swift into the NexusAgentExtension
// directory so the Swift fast-decline set can NEVER drift from the Go set.
//
// The protected set is a HARDCODED FLOOR on both sides: it is intentionally
// NOT shadow-controllable. That immutability is the A3-defense — an
// attacker who can push the agent_settings shadow cannot widen a kill-list to
// close UDP for a system daemon, because the floor is compiled in, not config.
//
// Run via `go generate ./policy/systembundles/...` (the //go:generate directive
// lives on the systembundles package).
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/AlphaBitCore/nexus-gateway/packages/shared/policy/systembundles"
)

func main() {
	out := systembundles.RenderSwift()

	// Locate this source file to anchor the output path independent of cwd.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "gen: cannot resolve own source path")
		os.Exit(1) //nolint:forbidigo // standalone code-generator CLI (go:generate), not runtime library code
	}
	// thisFile = .../packages/shared/policy/systembundles/internal/gen/main.go
	// climb to the repo's packages/ dir: gen -> internal -> systembundles ->
	// policy -> shared -> packages
	packagesDir := filepath.Clean(filepath.Join(filepath.Dir(thisFile),
		"..", "..", "..", "..", ".."))
	dest := filepath.Join(packagesDir,
		"agent", "platform", "darwin", "NexusAgent", "NexusAgentExtension",
		"SystemBundles.generated.swift")

	if err := os.WriteFile(dest, []byte(out), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "gen: write %s: %v\n", dest, err)
		os.Exit(1) //nolint:forbidigo // standalone code-generator CLI (go:generate), not runtime library code
	}

	// Sanity: the file we wrote round-trips to identical bytes.
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, []byte(out)) {
		fmt.Fprintf(os.Stderr, "gen: written file does not match rendered output: %v\n", err)
		os.Exit(1) //nolint:forbidigo // standalone code-generator CLI (go:generate), not runtime library code
	}
	fmt.Printf("gen: wrote %s (%d bytes)\n", dest, len(out)) //nolint:forbidigo // standalone code-generator CLI (go:generate) human-facing stdout, not runtime library code
}
