// Command nexus is the operator toolkit: a single binary with three faces
// (TUI for humans, CLI for scripts, MCP server for agents) over one core.
package main

import (
	"os"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
