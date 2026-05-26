// Package normalize — shared Registry construction so every data-plane
// service (agent, compliance-proxy, ai-gateway, nexus-hub agent_audit)
// runs the same Tier 1+2+3 normalize chain.
//
// Without a shared builder, each service had its own way of getting
// the chain wired:
//   - nexus-hub (agent_audit handler) built one locally and called
//     core.BuildAuditFn.
//   - ai-gateway built one for its audit emitter via BuildAuditFn.
//   - agent + compliance-proxy invoked adapter.Normalize directly
//     (Tier 1 only) and silently dropped any spec mismatch — chatgpt
//     web `event: delta_encoding` SSE returned nil, hook pipelines
//     received an empty Normalized field, and audit rows persisted no
//     normalized payload at all. The Agent UI "Normalized" tab then
//     fell back to raw bytes because the wire field was absent.
//
// BuildRegistry returns a frozen Registry wired identically to the
// nexus-hub variant: Tier 1 AI builtins + per-host Tier 1 adapters,
// Tier 2 pattern probe, Tier 3 verbatim http-text. Call once per
// service at wiring time; reuse the returned pointer for the life of
// the process.
package normalize

import (
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/traffic/adapters"
	codecs "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/codecs"
	core "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/extract"
)

// BuildRegistry constructs the canonical Tier 1+2+3 normalize Registry
// shared by every data-plane service. Frozen on return.
func BuildRegistry() *core.Registry {
	reg := core.NewRegistry()
	// Tier 1 — AI builtins (anthropic, gemini, openai-chat, openai-responses, …).
	codecs.RegisterDefaultAIBuiltins(reg)
	// Tier 1 — per-host adapters (chatgpt-web, claude-web, gemini-web,
	// openai-compat, …). Skips IDs already covered by AI builtins.
	adapters.RegisterTier1AdapterNormalizers(reg)
	// Tier 2 — pattern-based extraction fallback. Tier 3 (GenericHTTP
	// verbatim → http-text) is the registry's default fallthrough.
	extract.WireTier2(reg)
	reg.Freeze()
	return reg
}
