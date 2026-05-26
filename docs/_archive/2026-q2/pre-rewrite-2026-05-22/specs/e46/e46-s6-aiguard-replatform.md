# E46 S6 — AI-Guard re-platform to shared hook

**Epic:** E46
**Requirements:** [e46-traffic-normalization.md](../../../../docs/developers/specs/e46/e46-traffic-normalization.md)
**OpenAPI:** [e46-s6-aiguard.yaml](../openapi/e46-s6-aiguard.yaml)
**Status:** Approved
**Date:** 2026-05-13

---

## Architecture summary

Phase 5 moves AI-Guard from `packages/ai-gateway/internal/pipeline/aiguard/` to `packages/shared/policy/hooks/aiguard/` and folds it into the standard hook lifecycle:

- AI-Guard implements the `Hook` interface; `Execute(ctx, *HookInput)` consumes `NormalizedPayload`.
- The `/v1/ai-guard/classify` HTTP endpoint is **deleted**. Admin testing moves to the generic `POST /api/admin/hooks/{id}/dry-run` introduced in S4.
- `aiguard.TrafficSink` is **deleted**. Hook execution emits a `HookResult` that the pipeline records via the same path used by every other hook; the `internal_purpose = "ai-guard"` label survives on `traffic_event` so billing / analytics can isolate AI-Guard cost.
- `aiguard.Cache` (Redis) survives but is keyed differently: `sha256(canonical-json(NormalizedPayload subset + detectorType + judgeModel + payloadScope))`.
- `aiguard_config` table is split: shared columns move to `hook_config.config` JSON; structured prompt/model/template fields move to a new `aiguard_hook_settings` sidecar (FK to `hook_config.id`).
- compliance-proxy and agent register the `aiguard` factory in their hook registries — they get LLM-as-judge classification automatically.

AI-Guard's returned decision is a *suggestion*; the hook's `onMatch.inflightAction` is the *policy ceiling*. The strictest of the two wins:

| AI-Guard suggests | onMatch.inflight | effective |
| --- | --- | --- |
| approve | * | onMatch.inflight |
| block-soft | approve / redact | block-soft |
| block-hard | * (except already block-hard) | block-hard |
| redact | approve | redact |
| redact | block-hard / block-soft | strictest block |

When AI-Guard's suggestion is overridden by policy ceiling, `HookResult.ReasonCode = "AIGUARD_SUGGESTED_VS_POLICY"` and `Reason` carries both values.

---

## Story

### S6 — AI-Guard as a shared hook

**User story:** As a Nexus platform engineer, I configure AI-Guard like any other hook — picking a detector, a judge model, an action — and it works on all three data-plane services without separate plumbing.

**Tasks:**

- **T6.1** — Move `packages/ai-gateway/internal/pipeline/aiguard/{types,classify,prompt,backend_external,backend_provider,cache,config_cache,decoder,fingerprint,metrics}.go` → `packages/shared/policy/hooks/aiguard/`. Delete `inproc.go` (the in-proc Classifier interface is no longer used; the hook calls Backend directly).
- **T6.2** — New `packages/shared/policy/hooks/aiguard/hook.go` implementing `Hook`:
  - `Config{ DetectorType, JudgeModel, PayloadScope, ConfidenceThreshold, PromptTemplate, BackendMode, OnMatch }`.
  - `Execute(ctx, *HookInput)` derives the judge prompt from `input.Normalized` scoped by `PayloadScope`, calls `Backend.Call`, parses the structured response (`{ decision, confidence, reason, labels, redactions }`), reconciles AI-Guard's suggested decision with `OnMatch.InflightAction` (strictest wins), emits the resulting `HookResult` including `RedactionSpans` and the `AIGUARD_SUGGESTED_VS_POLICY` reason code when reconciliation changed the decision.
- **T6.3** — Update the judge prompt (`prompt.go`) to ask for structured span output:
  - Output schema: `{"decision":"approve|block-hard|block-soft|redact", "confidence":<0-1>, "reason":"…", "labels":["…"], "redactions":[{"message_index":N,"content_index":N,"start":N,"end":N,"replacement":"…"}]}`.
  - Few-shot examples included in the template.
- **T6.4** — Delete `/v1/ai-guard/classify` HTTP endpoint, handler, mux registration, the wiring in `packages/ai-gateway/cmd/ai-gateway/main.go` (`aiguardLiveClassifier`, `aiguardModelLookup`, `classifyHandler`, `wiring_aiguard.go`, the X-RS-Token gating). Delete `/v1/ai-guard/compliance-webhook` as well — it was a redirect to the same internal endpoint.
- **T6.5** — Delete `aiguard.TrafficSink` and its emitter. The `internal_purpose="ai-guard"` label moves into the hook's per-execution metric labels and gets propagated into `traffic_event.internal_purpose` when the hook is run as part of a pipeline whose primary purpose is the ai-guard call (rare; the more common case is hook-as-content-filter where `internal_purpose` stays NULL).
- **T6.6** — Cache rework: `aiguard.Cache.Key(NormalizedPayload, DetectorType, JudgeModel, PayloadScope) string`. Delete the old `normalizeContent` (now `canonicalizeForCacheKey`) helper; the new key is `sha256(canonical-json(subset))` where the subset is the messages / tools / params per `PayloadScope`.
- **T6.7** — Database split:
  - New table `aiguard_hook_settings` (FK to `hook_config.id`, on delete cascade): `prompt_template TEXT NULL`, `judge_model_id TEXT NULL`, `backend_mode TEXT NOT NULL`, `payload_scope TEXT NOT NULL DEFAULT 'messages'`, `confidence_threshold NUMERIC(4,3) NOT NULL DEFAULT 0.500`, `cache_ttl_seconds INT NOT NULL DEFAULT 600`.
  - Migration moves any seeded `aiguard_config` rows into the new shape (development data only; per policy no production migration code).
  - Drop the old `aiguard_config` table.
- **T6.8** — cp/agent register the aiguard factory. Add a config sample to their `*.dev.yaml` showing how an operator enables ai-guard on compliance-proxy.
- **T6.9** — Admin UI: aiguard configuration is now part of the hook editor (S4). Add an "AI-Guard advanced" subform that surfaces the aiguard-specific fields. The standalone aiguard backend page in Settings is deleted; what survives is the backend-mode picker inside the hook editor + a read-only "Recent AI-Guard runs" panel that filters traffic events by `internal_purpose = 'ai-guard'`.
- **T6.10** — IAM impact audit: the deleted `/v1/ai-guard/classify` endpoint was gated by `X-RS-Token`; removing it removes the route from the allowed set. The deleted Settings/AI Guard backend page is removed from the IAM-allowed routes. `iam.ResourceHook` continues to cover the new aiguard editor surface.

**Acceptance:**

- **AC-S6.1** — `grep -rn "POST /v1/ai-guard\|/v1/ai-guard/classify\|aiguard\.TrafficSink\|aiguardLiveClassifier" packages/` returns zero hits.
- **AC-S6.2** — `packages/ai-gateway/internal/pipeline/aiguard/` directory is removed.
- **AC-S6.3** — `packages/shared/policy/hooks/aiguard/` contains a working `Hook` implementation, all surviving unit tests still pass.
- **AC-S6.4** — compliance-proxy with `aiguard` hook configured detects a PII match in an Anthropic request via the judge model and applies redact spans visible in the audit log.
- **AC-S6.5** — A hook whose `onMatch.inflightAction = "block-hard"` overrides an AI-Guard suggestion of `redact`: the request is blocked (403) and `traffic_event.request_hook_reason_code = "AIGUARD_SUGGESTED_VS_POLICY"`, `Reason` includes both values.
- **AC-S6.6** — Cache hit rate is preserved across the cache-key reshape (the subset that maps to a cache key is canonical-JSON, so logically equivalent payloads still match).
- **AC-S6.7** — Admin UI hook editor exposes the AI-Guard advanced subform with i18n parity (`en / zh / es`).
