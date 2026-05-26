---
doc: shared-wirerewrite-architecture
area: cross-cutting
service: shared
tier: 1
updated: 2026-05-20
---

# `packages/shared/transport/wirerewrite/` Architecture

> **Tier 3 architecture doc.** Read before adding a rule, changing a circuit-breaker threshold, or modifying upstream wire-rewrite behavior.

## What it is

`packages/shared/transport/wirerewrite/` is the **byte-level wire rewriter** that runs just before the upstream send (and again just before Nexus L1 cache-key hashing). It owns a small set of provider-specific rule IDs and rewrites the adapter-wire body to match provider quirks.

**Distinct from `packages/shared/transport/normalize/`**, which is the canonical request/response shape framework. The two packages do different jobs — `wirerewrite` is byte-level; `normalize` is the canonical-shape framework.

## Two entry points

| Method | When it runs | What it does | Fail behavior |
|---|---|---|---|
| `NormalizeKey` | After `PrepareBody`, before `Cache.BuildKey` | Strips key-safe volatile fields so equivalent requests hash to the same L1 cache key | Always fail-open |
| `NormalizeUpstream` | After L1 MISS, before `runViaBroker` | Strips and/or injects bytes in the body sent to the upstream provider | Gated by `normaliser_enabled`; per-rule circuit breaker on recurring failure |

Both take the adapter-wire body produced by `PrepareBody`.

## Bundled rules

12+ rule IDs, each provider-specific:

- `claude-code-cch-strip`, `bedrock-claude-cch-strip` — strip Claude Code `cache_control` markers
- `openai-field-order-normalize`, `azure-openai-field-order-normalize` — reorder OpenAI/Azure fields to match the wire order Azure expects
- `deepseek-field-order-normalize`, `glm-field-order-normalize`, `moonshot-field-order-normalize`, `mistral-field-order-normalize`, `xai-field-order-normalize`, `groq-field-order-normalize`, `perplexity-field-order-normalize`, `together-field-order-normalize` — same pattern for OpenAI-compat providers
- `cache-normaliser` — strip bytes that the cache layer removed but the wire still carries (E38)

The full list is bundled in `bundled.go::bundledRules()`. Each rule's ID is a **stable wire-format identifier** — DB rows in `AdminCachePreview`, admin UI dry-run reports, and audit segments all carry the rule ID verbatim.

## Circuit breaker

`circuit.go` trips a per-rule circuit open after `defaultCBThreshold` (10) errors within `defaultCBWindow` (60s). Open stays open until an explicit reset, which is triggered by an `Engine.Reload`. This avoids silently corrupting upstream bytes after a recurring rule failure.

## Config flow

Admin UI → CP admin API → Hub `system_metadata` Cat B shadow blob → `thingclient` push → AI Gateway calls `Engine.Reload(...)` with the new `Config`. Per-rule `RuleOverride{Enabled, DryRunAlways}` lets admins switch a rule into dry-run without removing it.

`Config.NormaliserEnabled` is the global gate for `NormalizeUpstream` (NormalizeKey always runs). The Go field is named `NormaliserEnabled` and its JSON tag is `"normaliser_enabled"` — both preserved verbatim from the pre-rename era to keep config/shadow/DB shape stable across an in-place rename.

## API stability — preserved identifiers

These names changed in the Go API at rename time but **must NOT change in wire format**:

| Wire-format identifier | Lives in | Why preserved |
|---|---|---|
| `normaliser_enabled` (JSON tag, shadow key) | `Config.NormaliserEnabled` field | Admin shadow / DB row; renaming would force a coordinated config migration |
| Rule ID `cache-normaliser` | rule constants in `bundled.go` | Stored in audit segment SourceID; renaming would orphan historical audit rows |
| Rule IDs `<provider>-field-order-normalize`, `<provider>-cch-strip` | `bundled.go` constants | Same — audit history, admin UI references |

The Go package name (`wirerewrite`) and Go type names are internal and can change with normal API-stability discipline (§5 of `shared-package-architecture.md`).

## Audit attribution

When `NormalizeUpstream` strips bytes, it emits a `normalize.NormalizedSegment` with:
- `Source = normalize.SourceCacheNormaliser` (or the equivalent for other rules)
- `SourceID = <rule ID>` (e.g. `"openai-field-order-normalize"`)
- `Action = normalize.ActionStrip`

Hooks reading the audit record see exactly which rule touched the body.

## Consumers

- `packages/ai-gateway/internal/ingress/proxy/proxy.go` — instantiates the `Engine` and calls both entry points on the hot path.
- `packages/ai-gateway/cmd/ai-gateway/wiring/boot.go` + `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — own the `Engine` lifecycle (construct on boot, reload on shadow change).
- `packages/control-plane/internal/ai/cache/handler/cache_preview.go` — admin "cache preview" endpoint runs `Engine` in a dry-run mode to project what rules would do for a given trafficEvent row.

## Sources

- `packages/shared/transport/wirerewrite/` (the whole subpackage, 8 .go files).
- CLAUDE.md "Real implementation only" and "Development-phase policy" — wire-format identifiers are the boundary that the no-backward-compat rule does not cross.

## Cross-references

- `docs/developers/architecture/cross-cutting/shared/shared-package-architecture.md` — parent doc, dep-tier policy, API-stability rule.
- `docs/developers/architecture/services/ai-gateway/prompt-cache-architecture.md` — E38 prompt-cache pipeline that drives `Config` shape.
- `docs/developers/architecture/cross-cutting/storage/cache-multi-tier-architecture.md` — relationship to `Cache.BuildKey` (NormalizeKey runs just before it).
