// Package agent is the Nexus operator agent kernel: a Claude-Code-modeled,
// tool-using LLM loop with a typed tool registry, risk-tiered permission gate,
// context assembly, session compaction, file memory, system-prompt assembly,
// and session persistence. It is pure: it
// depends only on the stdlib and the Model, Tool, and SituationProvider seam
// interfaces it defines, so it is fully unit-testable with a fake model and
// stub tools. Concrete gateway-backed implementations live in Layer 2.
package agent
