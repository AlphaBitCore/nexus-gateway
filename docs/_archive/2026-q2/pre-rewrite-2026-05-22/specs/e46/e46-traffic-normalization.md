# E46 — Traffic Payload Normalization & Hook Refactor

Status: In progress
Epic owner: Gateway / Compliance / Platform
Related: E27 (rule packs), E29 (hooks region + rulepack refactor), E33 (dual-pipeline compliance), E37 (payload capture), E38 (prompt cache friendliness).

## 1. Background

Three independent capture surfaces — ai-gateway provider adapters, compliance-proxy MITM, and agent local interception — write `traffic_event` rows containing raw provider-protocol bytes. Today the audit pipeline has three structural problems:

1. **Operators reading the audit log** see fragmented SSE chunks (OpenAI `data: {…}`), Anthropic event streams (`event: message_delta`), Gemini chunked JSON, and raw HTTP bodies for non-AI traffic. There is no human-readable view; understanding what a model returned requires manually reassembling tokens.
2. **Compliance hooks** (PII detector, keyword filter, content-safety, rule-pack engine, AI-Guard) receive content as protocol-shaped fragments rather than fully assembled text. PII split across two SSE chunks is missed; multi-line keyword matches fragment across `data:` boundaries. Each hook implementation today carries its own ad-hoc assembly heuristics with no shared lifecycle.
3. **Hook action vocabularies are inconsistent.** PII-detector uses `action: block|warn|redact`. Keyword-filter uses `severity: hard|soft`. Content-safety uses `action: reject_hard|reject_soft`. Rule-pack uses `severity: hard|soft|info`. Quality-checker uses `blockOnAnomaly: bool`. Operators configuring four similar hooks must learn four different schemas; the audit reader cannot uniformly answer "what was the policy when this was flagged". Three of the four content hooks have no `redact` capability at all — yet redaction (in-flight or storage-only) is the most common enterprise compliance requirement.

A fourth issue surfaced during scoping: **storing the trigger content itself is a compliance problem.** When PII-detector blocks a request, the `traffic_event` row today still persists the raw normalized content, which contains the very PII that motivated the block. Today's PII `redact` action couples upstream rewrite and storage rewrite — there is no way to express "let the upstream call proceed unchanged, but never persist the sensitive content to our audit log", which is the common policy stance.

A fifth issue is **AI-Guard's structural separation.** Today AI-Guard is a side-service with its own HTTP endpoint (`/v1/ai-guard/classify`), its own cache (`aiguard.Cache`), its own audit sink (`aiguard.TrafficSink`), and its own ai-gateway-internal package. Hooks that delegate to AI-Guard pay an architectural tax for what is structurally just another classifier. cp/agent cannot use AI-Guard at all because the package is ai-gateway-private.

This epic resolves all five in one greenfield pass, consistent with the project's pre-GA "no backward compatibility, no defer" policy.

## 2. Scope

### Must

- Introduce a shared `packages/shared/transport/normalize/` package with a `Normalizer` interface and a typed `NormalizedPayload` schema discriminated by `kind` (`ai-chat`, `ai-completion`, `ai-embedding`, `ai-image`, `http-json`, `http-text`, `http-form`, `http-multipart`, `http-binary`, `unsupported`).
- Implement protocol normalizers covering OpenAI Chat Completions, Anthropic Messages, and Google Gemini Generate for AI traffic, and a GenericHTTPNormalizer dispatching by `Content-Type` for non-AI HTTP traffic captured by compliance-proxy and agent.
- Preserve reasoning / thinking content as a first-class `content[].type = "reasoning"` block in `NormalizedPayload.messages[].content`. Do not silently drop.
- Multimodal content (image, audio) is represented as a content block whose `inline_data` is replaced by a `binary_ref { size, content_type, sha256 }` reference. Inline binary bytes are never duplicated into the normalized JSON.
- Persist normalized payloads in a new `traffic_event_normalized` table (1:1 sidecar to `traffic_event`).
- Replace `HookInput.Content []ContentBlock` with `HookInput.Normalized *NormalizedPayload`. All eleven shared hook implementations migrate to the new shape in a single atomic change; no parallel-run period.
- Introduce a unified `OnMatchConfig` shape `{ inflightAction, storageAction, replacement }` and adopt it on every content-touching hook (pii-detector, keyword-filter, content-safety, rulepack-engine, quality-checker, webhook-forward). Drop the legacy `action` / `severity` / `blockOnAnomaly` fields.
- Express redaction as `HookResult.RedactionSpans []RedactionSpan` carrying `{rule_id, content_index, start, end, replacement}`. The pipeline applies spans to the stored normalized copy (always when `storageAction = redact`) and to the upstream-bound body (when `inflightAction = redact`) via the existing `TrafficAdapter.RewriteRequestBody`.
- Enable `allowModify = true` on compliance-proxy and agent pipelines and route their inflight redact through the shared rewrite path so redact behaves consistently three-side. When a protocol's mid-stream rewrite is unsafe, the adapter returns `ErrRewriteUnsupported`; the pipeline downgrades to storage-only redact and emits `ReasonCode = REDACT_INFLIGHT_UNSUPPORTED` on the existing `HookResult.Reason` / `ReasonCode` channel — no schema additions.
- Rename `Decision.RejectSoft` → `Decision.BlockSoft` across the entire repository (Go enum, metric labels, prompt templates, tests, audit `request_hook_decision` / `response_hook_decision` written values).
- Re-platform AI-Guard as a first-class shared hook: move the package to `packages/shared/policy/hooks/aiguard/`, implement the `Hook` interface, consume `NormalizedPayload`, return `RedactionSpan[]`, delete `/v1/ai-guard/classify` and `aiguard.TrafficSink`, split `aiguard_config` into `aiguard_hook_settings` sidecar table FK'd to `hook_config.id`. cp/agent register the factory in their hook registries.
- Add `HookConfig.applicableTrafficKinds []string` (default `["ai"]`). The pipeline filters hooks by `NormalizedPayload.kind` so non-AI traffic is not subject to AI-only hooks unless an operator opts it in.
- Surface ReasonCode constants as a closed set: `REDACT_INFLIGHT_UNSUPPORTED`, `REDACT_STORAGE_ONLY_BY_POLICY`, `STORAGE_DROPPED_BY_POLICY`, `AIGUARD_SUGGESTED_VS_POLICY`. The pre-E46 `MODIFY_DOWNGRADED_TO_REJECT` code is deleted (the downgrade no longer happens once cp/agent support MODIFY).
- TrafficEvent UI detail page gains two tabs: `Normalized` (default) and `Raw`. Normalized renders chat bubbles for AI kinds, JSON tree for `http-json`, decoded text for `http-text`, and a binary placeholder card for `http-binary`. Redaction spans render as inline badges. Failure to normalize surfaces a banner with `error_reason` from `traffic_event_normalized` and the Raw tab is always available as fallback.
- i18n parity across `en / zh / es` for every new UI string.

### Should

- Same Anthropic request captured by ai-gateway, compliance-proxy, and agent produces byte-identical `NormalizedPayload` (cross-service consistency property).
- Admin can dry-run any hook (including aiguard) via a generic `POST /api/admin/hooks/{id}/dry-run` endpoint that accepts a `NormalizedPayload` and returns the `HookResult` without side effects.
- Default `storageAction = redact` system-wide so freshly installed instances are compliance-default. Operators can opt to `keep` per hook.
- AI-Guard `payloadScope` configuration limits how much of the normalized payload reaches the judge model: `text-only` / `messages` / `messages+tools` / `full`. Default `messages`.

### Could

- Streaming-aware partial normalization: emit a partial `NormalizedPayload` snapshot on every flush so very long responses can be inspected before stream end. Out of scope for E46; tracked separately.
- Per-org-defaults for `applicableTrafficKinds`. Out of scope for E46.

### Won't

- Backward-compatibility shims for the legacy `HookInput.Content` field, the legacy `action` / `severity` config fields, or the `/v1/ai-guard/classify` endpoint. Greenfield switch per project policy.
- Re-introduce a separate ai-guard side-service after the hook re-platform.
- Add a normalize-on-button-click admin action. Normalized data is produced eagerly at capture time; UI reads it directly.

## 3. Non-Functional Requirements

- **Latency.** Normalization adds ≤ 2 ms p99 to the request hot path on a 4 KB OpenAI Chat request and ≤ 10 ms p99 on a 100 KB Anthropic Messages request with tools and multi-turn history. Streaming response normalization runs at stream finalize, off the wire-bytes-to-client path, and must not block client delivery.
- **Memory.** Normalization buffers the assembled response body in memory once, sized to the configured payload-capture cap (default 256 KiB inline + spill threshold). No double-buffering between the capture pipeline and the normalizer.
- **Storage.** Normalized JSON is typically 30–50 % smaller than the raw provider stream after removal of SSE framing, heartbeats, and chunk boundaries. The `traffic_event_normalized` table inherits the retention policy of `traffic_event` unless an operator configures a separate retention.
- **Three-side consistency.** Same protocol, same wire body → identical `NormalizedPayload` regardless of which of the three data-plane services captured it. Verified by cross-service integration tests.
- **Failure isolation.** A normalization failure for one request never affects request delivery or other requests. The `traffic_event_normalized` row is written with `status ∈ {partial, failed}` and `error_reason` populated; the parent `traffic_event` row is unaffected.
- **Hook latency budget unchanged.** The pipeline's per-hook timeout (default 5 s) applies to hook execution. Normalization happens before pipeline entry and is bounded by adapter-level deadlines (10 s default).
- **Security: no raw secrets in normalized JSON.** Authentication headers (`Authorization`, `x-api-key`, `cookie`) are stripped from `http.headers_filtered`. Request bodies that include OAuth bearer tokens or API keys in JSON fields are pattern-stripped at capture time before normalization (this is existing behavior in `payload_capture`; E46 only inherits it). Multimodal binary bytes are referenced by hash, never inlined into normalized JSON.

## 4. User Roles & Personas

- **Compliance officer.** Reads `traffic_event` rows from the UI to audit AI usage. Needs human-readable assembled messages, visible decision attribution, and clear redaction markers. Today must read raw SSE; with E46 reads the Normalized tab.
- **Security admin.** Configures hooks (PII, keyword, rule-pack, AI-Guard) via the admin UI. Needs one consistent action vocabulary across all content hooks. Today learns four. With E46 learns one `onMatch` shape.
- **Platform engineer (Nexus).** Maintains the hook framework, AI-Guard, and capture pipelines. Needs a single normalize implementation shared across all three data-plane services. Today maintains three divergent extraction paths and one ai-gateway-private AI-Guard. With E46 maintains one shared package.
- **Org admin / end developer.** Uses the gateway to send AI traffic. No direct interaction with normalization — but if a policy redacts content from the audit log, the dev should see no behavioral change in their request (redact + storage-only is invisible to them).

## 5. Constraints & Assumptions

- The project is **pre-GA** with no installed user base. Per CLAUDE.md, no backward-compatibility shims, no deprecated paths, no data migration for historical rows. Fresh seed acceptable.
- All capture occurs on the data plane (ai-gateway / compliance-proxy / agent); the control plane consumes already-captured rows and never touches the hot path. E46 keeps this invariant.
- The existing `traffic_event` schema is dual-pipeline (E33): request and response stages each have their own `*_hook_decision` / `*_reason` / `*_reason_code` columns. E46 routes `RedactionSpan` and `ReasonCode` through the appropriate stage's existing column; no schema changes to `traffic_event` itself.
- AI-Guard backends remain pluggable (external-mode calls a configured judge provider; provider-mode reuses one of the user's configured AI providers). The hook implementation owns the backend abstraction; the rest of the pipeline is backend-agnostic.
- Multimodal content (images, audio) is captured today via `payload_capture` spill store. E46 references the spill artifact by `binary_ref` rather than re-encoding it into normalized JSON.

## 6. Glossary

- **NormalizedPayload** — Canonical, provider-agnostic representation of one captured request or response. Discriminated by `kind`.
- **ContentBlock (E46 redefinition)** — Element of `NormalizedPayload.messages[].content`, typed `text` / `image_ref` / `tool_use` / `tool_result` / `reasoning`.
- **RedactionSpan** — Audit-log-grade record of "rule R caused bytes [start, end) of content block C to be replaced with replacement R". Drives both inflight rewrite and storage rewrite uniformly.
- **InflightAction** — Hook policy for the upstream-bound copy of the body: `approve` / `block-hard` / `block-soft` / `redact`.
- **StorageAction** — Hook policy for the audit-log-bound copy: `keep` / `redact` / `drop-content`.
- **AI-Guard (E46 redefinition)** — A shared hook implementation backed by an LLM-as-judge classifier. No longer a separate service.
- **applicableTrafficKinds** — Per-hook filter `[]string` controlling which `NormalizedPayload.kind` values trigger this hook. Default `["ai"]`.
- **traffic_event_normalized** — 1:1 sidecar table to `traffic_event` storing the normalized request and response JSON, status, error reason, and redaction spans.
- **Three-side consistency** — Same wire body captured by ai-gateway, compliance-proxy, or agent yields identical `NormalizedPayload`. Verified by integration tests.

## 7. Out-of-Scope Cleanups (recorded so they are not lost)

- **`/v1/ai-guard/classify` HTTP endpoint deletion.** No external consumers (pre-GA); deletion is greenfield.
- **`aiguard.TrafficSink` deletion.** Replaced by hook-pipeline record path with `internal_purpose = "ai-guard"` label.
- **`aiguard.normalizeContent` (cache-key whitespace canonicalizer) deletion / rename.** The function name collides with the new `shared/normalize` package's mental model. Renamed `canonicalizeForCacheKey` in Phase 0 and replaced entirely once cache keying moves to `sha256(canonical-json(NormalizedPayload subset + detectorType + judgeModel + payloadScope))` in Phase 5.
- **`pipeline.go:269-281` MODIFY_DOWNGRADED_TO_REJECT path deletion.** Once cp/agent support MODIFY (Phase 4), this branch is unreachable.

## 8. Phasing (informative — full breakdown in SDD)

| Phase | Theme |
| --- | --- |
| 0 | Docs + foundation types (this requirements doc, SDD stories, OpenAPI stubs, package skeleton, schema migration, BlockSoft rename, ReasonCode catalog) |
| 1 | ai-gateway + OpenAIChatNormalizer end-to-end |
| 2 | AnthropicMessagesNormalizer + atomic HookInput swap |
| 3 | Unified onMatch hook config + admin UI |
| 4 | RedactionSpan framework + cp/agent inflight redact |
| 5 | AI-Guard re-platform to shared hook |
| 6 | TrafficEvent UI Normalized + Raw tabs |
| 7 | GeminiGenerateNormalizer + cp/agent fully on shared/normalize |
| 8 | GenericHTTPNormalizer + non-AI traffic |

Each phase ships as one independent PR with no feature flag, no compat shim, and no half-finished surface left between phases.
