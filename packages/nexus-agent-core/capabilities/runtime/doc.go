// Package runtime turns the pure packages/nexus-agent-core/agent kernel into a
// live Nexus operator agent: it implements the kernel's Model, Tool, and
// SituationProvider seams over packages/nexus-agent-core/core and builds the
// tool Registry behind the TUI agent loop.
// It imports both packages/nexus-agent-core/agent (the seam interfaces) and
// packages/nexus-agent-core/core (the data layer); the kernel itself imports
// neither, so it stays unit-testable in isolation.
package runtime
