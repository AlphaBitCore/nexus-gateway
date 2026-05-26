# E27 S01 — AI Guard Endpoint

**Story:** As a compliance operator I need a shared ML/LLM judge so hooks
that require semantic judgment (prompt-injection, jailbreak, secret-leak)
can call a configured backend without each hook managing its own provider.

## Acceptance criteria

- [x] `POST /v1/ai-guard/classify` accepts service-token auth and returns
  the documented `ClassifyResponse` (spec §4.2) on valid input.
- [x] `configured_provider` backend mode calls `providers.Adapter.Execute`
  directly — `routing.Router.Route` is never invoked on this code path.
- [x] `external_url` backend mode does exactly one `http.Client.Post` per
  classify call; no other network activity.
- [x] Every call writes a `traffic_event` row with `internal_purpose="ai-guard"`.
- [x] Analytics default-filter (`excludeInternal=true`) hides these rows;
  toggle reveals them.
- [x] Redis response cache with TTL from `ai_guard_config.cache_ttl_seconds`
  (0 = disabled). Cache miss invokes backend; cache hit skips backend.
- [x] Admin UI at `Settings → AI Guard Backend`:
  - radio toggle between `configured_provider` / `external_url`
  - prompt template editor (plain textarea)
  - timeout + cache TTL numeric inputs
  - external-URL warning banner
  - dry-run panel showing side-by-side request/response JSON + latency
- [x] Malformed judge output surfaces as HTTP 503 with `error=backend_unavailable`,
  `detail=output_parse_failed`.
- [x] i18n keys exist in en/zh/es for every visible string.
- [x] OpenAPI 3.1 spec at `docs/users/api/openapi/admin/e27-s01-ai-guard-classify.yaml`
  matches the handler exactly.
- [x] `scripts/test-all.sh` is green at P-B Task 36.
- [x] `PUT /api/admin/ai-guard/config` publishes exactly one
  `nexus.event.admin-audit` message on the success path
  (`action=update`, `entityType=aiGuardConfig`, BeforeState/AfterState
  projected via `aiGuardConfigAuditSummary` — backend mode, provider id,
  model, timeouts, cache TTL, prompt template sha). Validation failures
  (4xx) and store errors (5xx) MUST NOT emit. Verified by
  `TestAdminAIGuard_Audit_PutConfig_*` using an MQ producer spy.

## Tasks

See [P-B implementation plan](../superpowers/plans/2026-04-22-builtin-hooks-B-ai-guard.md).

## Non-goals (explicit; enforced by code review)

- Per-detector backend config (P1).
- ESCALATE semantic on agent hooks (P1).
- Sampling (P1).
- Syntax-highlighted prompt editor (P1).

## Dependencies

- Requires P-A landed (Tags, ConnectionStageCompatible, contract suite).
- Uses existing `providers.Adapter` interface without modification.

## Architecture notes

- **Loop prevention is structural**, not flag-based: the classify handler
  calls `providers.Adapter.Execute` / `http.Client.Post` directly,
  bypassing `RoutingEngine` and `HookPipeline`. A hook calling
  `/v1/ai-guard/classify` → triggers provider call → provider call is
  not hook-intercepted → no cycle. No `InternalPurpose` flag needed in
  `HookInput`.
- **Cache key** = `aiguard:v1:sha256(detector_type + "\n" + normalized_content + "\n" + backend_fingerprint)`.
- **`backend_fingerprint`** = `sha256(backend_mode + "|" + (provider_id OR external_url) + "|" + model_name + "|" + prompt_template_sha)` — recomputed on every admin `PUT`.
- **Hot-path config cache**: 2-minute TTL + Redis pubsub invalidation on channel `nexus:config:invalidated` payload `aiguard`.
- **Control-plane <> ai-gateway shared types**: Go `internal/` rule forbids importing from `ai-gateway/internal/aiguard`. Control-plane mirrors `Request`/`Response`/`Context`/`Metadata` + reimplements `BackendFingerprint` / `PromptTemplateSHA`. Any future fingerprint algorithm change MUST land in both places (documented in the handoff).
