// Package capabilities turns the pure internal/agent kernel into a live Nexus
// operator agent: it implements the kernel's Model, Tool, and SituationProvider
// seams over internal/core, supplies skills, and builds the tool Registry shared
// by the TUI agent loop and the MCP server. It imports both internal/agent (the
// seam interfaces) and internal/core (the data layer); the kernel itself imports
// neither, so it stays unit-testable in isolation.
package runtime
