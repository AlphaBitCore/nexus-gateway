// Package core defines the canonical, provider-agnostic representation
// of one captured request or response (NormalizedPayload), plus the
// Normalizer interface and registry that produce it.
//
// All three data-plane services — ai-gateway, compliance-proxy, agent —
// consume this package; same wire bytes captured anywhere in the system
// yield byte-identical NormalizedPayload (modulo per-source metadata).
//
// # Discrimination
//
// NormalizedPayload.Kind discriminates the structural shape:
//
//   - ai-chat / ai-completion / ai-embedding / ai-image — AI traffic,
//     uses Messages, Tools, Params, Usage.
//   - http-json / http-text / http-form / http-multipart / http-binary —
//     non-AI HTTP traffic captured by compliance-proxy and agent, uses Http.
//   - unsupported — the registry could not map the input.
//
// The same payload type carries both shapes; consumers branch on Kind.
//
// # Reasoning content
//
// Reasoning / thinking blocks emitted by providers (Anthropic thinking,
// OpenAI reasoning) are preserved as ContentBlock{Type: "reasoning"}.
// They are NOT dropped; operator-facing UIs can render or hide them as
// configured. The default text projection for hooks (see projection
// helpers in shared/hooks) excludes reasoning text.
//
// # Redaction
//
// RedactionSpan is the canonical record of "this rule replaced bytes
// [start, end) of content C with replacement R". A single set of spans
// drives both the in-flight rewrite (TrafficAdapter.RewriteRequestBody)
// and the storage rewrite (traffic_event_normalized) — both apply to
// the same NormalizedPayload, so storage and in-flight stay in sync.
//
// # Three-side consistency
//
// Normalizer implementations are pure functions of (raw bytes, meta) —
// they do not touch the database, do not call the network, and do not
// log. This makes byte-identical output across the three data-plane
// services a provable property (verified by cross-service integration
// tests in tests/integration/normalize_consistency_test.go).
//
package core
