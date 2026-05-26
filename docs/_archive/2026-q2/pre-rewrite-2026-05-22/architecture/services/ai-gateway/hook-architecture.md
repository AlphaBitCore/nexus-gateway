---
doc: hook-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-21
---

# Hooks Framework Architecture

> **Tier 1 architecture doc.** Read this before touching `packages/shared/policy/hooks/**`, `packages/ai-gateway/internal/policy/hooks/**`, `packages/agent/internal/compliance/**`, the `HookConfig` schema, the `onMatch` evaluation logic, or any built-in hook implementation. Where hooks run in each traffic path is in `compliance-pipeline-architecture.md` and `agent-forwarder-architecture.md`.
>
> **Multimodal scope.** Today's hook framework is endpoint-agnostic and modality-agnostic — it implicitly assumes chat-shaped text content everywhere. Embeddings + multimodal endpoints (image-gen, TTS / STT, video-gen) expose this gap: content-scanning hooks find no text on a vector / image / audio response and silently `Abstain`. The **endpoint + modality awareness** extension is designed in `endpoint-typology-architecture.md` §6 and lands in **E62-S1** (`HookInput.{EndpointType, InputModality, OutputModality}`, `Hook.SupportsEndpoint()` / `SupportsModality()`, pipeline filters at build time). Class-A / Class-B hook split per modality is locked in there. Hooks added between now and E62-S1 should declare `SupportsEndpoint(t) → t == EndpointTypeChat` as their default.

Hooks are the **cross-cutting enforcement mechanism**. The same Go code runs in all three traffic paths (AI Gateway, Compliance Proxy, Agent), driven by the same `HookConfig` shape from the same Hub-pushed shadow.

---

## 1. The Hook interface

```go
// packages/shared/policy/hooks/core/types.go
type Hook interface {
    Execute(ctx context.Context, input *HookInput) (*HookResult, error)
    SupportsEndpoint(EndpointType) bool   // E62-S1 modality awareness
    SupportsModality(Modality) bool
}

// Verdict vocabulary aliased from packages/shared/policy/decision/.
const (
    Approve    = decision.Approve
    RejectHard = decision.RejectHard
    BlockSoft  = decision.BlockSoft   // replaces the older RejectSoft name
    Modify     = decision.Modify
    Abstain    = decision.Abstain
)
```

`HookInput` carries the canonicalised request / response: the `NormalizedPayload` produced by `shared/transport/normalize` (extracted text, messages, usage, tools), plus metadata (request ID, stage, model, source IP, target host, ingress type, TLS context for connection-stage). It is identical in shape across all three traffic paths — only the **populator** differs. Hooks never receive raw provider JSON.

`HookResult` carries the verdict + reason + per-hook telemetry; latency is stamped by the runner. There is no `Classification` field — sensitivity classification is not part of the framework. Modify hooks emit `TransformSpan`s on the result (via the canonical `NormalizedPayload` addressing), not raw modified bytes.

## 2. Registry & dispatcher

- **Registry** — global; built-in hooks auto-register in their package `init()`. Custom hooks register at process start.
- **Dispatcher** — per-stage instance. Each service constructs one dispatcher per stage at boot, given the registry + a `HookConfig` snapshot.
- **Hot-swap** — `HookConfig` changes via shadow trigger an `atomic.Pointer` swap of the dispatcher's config snapshot. In-flight transactions complete on the old config; new transactions pick up the new config on dispatch.

## 3. The three stages

| Stage | Runs at | Input |
|---|---|---|
| `request` | Before forwarding upstream | Canonical request `NormalizedPayload` + metadata |
| `response` | After receiving upstream response (or per chunk during streaming, see §7) | Canonical response `NormalizedPayload` + metadata |
| `connection` | Once per upstream connection, before any request flows | TLS context (SNI, client-cert fingerprint, target host); no body |

`HookConfig.stage` is a `String @default("request")` column (`tools/db-migrate/schema.prisma:68`); the Go `HookConfig` type and `HookInput.Stage` carry the canonical three-value enum `"request" | "response" | "connection"`. The schema's `// request | response` comment is stale — connection-stage hooks (`ConnectionStageCompatible` marker interface) ship today. Streaming is **not** a separate stage — chunked-async traffic reuses the `response` stage and re-runs the hook per chunk (see §7).

## 4. `HookConfig.onMatch` schema (E46-S4 canonical)

```yaml
hookConfig:
  enabled: true
  stage: request | response | connection                   # enforced by DB CHECK
  applicableIngress: [ALL]                                 # or any subset of:
                                                           # [AI_GATEWAY, COMPLIANCE_PROXY, AGENT]
  applicableTrafficKinds: [ai-chat, ...]                   # optional, filters by NormalizedPayload.Kind
  scope: include_reasoning                                 # optional
  config:                                                  # hook-implementation-specific
    onMatch:
      inflightAction: block-hard | block-soft | redact | approve
      storageAction:  keep | redact | drop-content
      replacement:    "***"                                # optional; used by inflightAction=redact
                                                           # or storageAction=redact
```

The `onMatch` object lives under the hook's `config` map and follows the canonical Go shape `core.OnMatchConfig{InflightAction, StorageAction, Replacement}` (`packages/shared/policy/hooks/core/types.go:58`).

**`inflightAction`** — what the hook does to the upstream-bound copy of the request/response that's about to be forwarded:

- `block-hard` — reject; pipeline short-circuits; client sees HTTP 451 (request stage) or stream-terminated (streaming).
- `block-soft` — pipeline continues but the soft reject is recorded; the upstream request still goes through. Audit captures the violation. Used for "alert but don't block" policies.
- `redact` — modify the in-flight body (PII replaced by `replacement`); request continues with redacted content.
- `approve` — let the request through unchanged.

**`storageAction`** — what the hook does to the **stored** audit copy (independent of inflight):

- `keep` — store the original content as-is.
- `redact` — store with `replacement` substituted for matched spans (audit-only redaction).
- `drop-content` — store metadata only; the matched content is not persisted.

The split lets policies say "block the request, but also keep the original in audit so we can investigate" (`inflightAction=block-hard, storageAction=keep`) or "let it through but redact in storage" (`inflightAction=approve, storageAction=redact`).

There is no `flag`, `log-only`, `redactStrategy`, or `redactFields` field in the canonical shape. Older hook configs that used those names were migrated to the unified onMatch shape in E46-S4.

**The PII scanner correctness saga (memory `project_hookconfig_e46s4_migration`).** Before 2026-05-13, six prod HookConfig rows had the wrong `onMatch.inflightAction` value — the PII scanner was running `block-hard` by mistake, not `redact`. The fix was a data migration. **Validate `onMatch.inflightAction` and `onMatch.storageAction` against the hook's expected semantics whenever you change a HookConfig** — it is easy to silently mis-configure.

## 5. Pipeline aggregation

```
Results = [hook1.Execute(ctx, input), hook2.Execute(ctx, input), ...]   // priority-ordered

if any(Verdict == RejectHard):
    final = RejectHard with attribution to first hard reject
elif any(Verdict == BlockSoft):
    final = BlockSoft with attribution to first soft reject (pipeline did NOT short-circuit)
else:
    final = Approve

modifications = compose(r.TransformSpans for r in Results where Verdict == Modify)
```

Hard rejects short-circuit the pipeline (subsequent hooks are skipped). Soft rejects do not short-circuit — every hook still gets a chance to record an opinion.

The aggregated decision is stamped on the traffic event with `blocking_rule` (the `RuleID` / `BlockingRule` attribution of the first hard reject, if any) and the per-stage pipeline trace.

## 6. Built-in hooks

Registered in `packages/shared/policy/hooks/builtins/builtins.go` — the shared registry is consumed identically by all three data-plane services (`三端一致`); service-specific overrides happen via `Registry.Clone().Replace()` at consumer setup time, not via per-service registries.

| Registered name | Implementation | What it does |
|---|---|---|
| `keyword-filter` | `validators/keyword_filter.go` | Configurable patterns (exact + regex) |
| `pii-detector` | `validators/pii_detector.go` | Pattern + ML pre-filter; classifies and optionally redacts |
| `content-safety` | `validators/content_safety.go` | Policy-based safety evaluation |
| `rate-limiter` | `ratelimit/rate_limiter.go` | Per-source / per-VK / per-org limits (Redis or local) |
| `request-size-validator` | `validators/request_size.go` | Body-size limits |
| `ip-access-filter` | `access/ip_access_filter.go` | Source IP allowlist / denylist |
| `data-residency` | `access/data_residency.go` | Geo / jurisdiction allowlist for outbound traffic |
| `rulepack-engine` | `validators/rulepack_engine.go` | Executes installed rule-pack matchers |
| `noop` | `core.NewNoop` | Always Approve; used as scaffolding / test scaffolding |
| `webhook-forward` | `webhook/webhook_forward.go` | Forward to admin-configured webhook for custom evaluation |
| `quality-checker` | `validators/quality_checker.go` | Response quality evaluation against criteria |

Each hook implementation lives under `packages/shared/policy/hooks/{access,ratelimit,validators,webhook,core}/` and is responsible for its own metrics, logging, and timeout handling.

**AI-Guard is NOT a built-in hook in this registry.** AI-Guard is a separate subsystem at `packages/ai-gateway/internal/policy/aiguard/` (covered by `aiguard-architecture.md`); ai-gateway hooks that want a model-based verdict call into it via `aiguard.InProcClient`, not via a `Hook` registration.

## 7. Streaming compliance modes

The interaction between streaming and hooks is configured per host (Compliance Proxy / Agent) or per provider (AI Gateway):

| Mode | Behavior |
|---|---|
| `passthrough` | Relay only. No hook execution, no body capture. Use for non-AI traffic that should not be inspected. |
| `buffer_full_block` | Assemble the full extracted prompt + completion before forwarding any byte. Response-stage hook runs once at stream end. On hard reject: HTTP 451; upstream body never reaches client. Trades real-time UX for blocking ability. |
| `chunked_async` | Relay bytes to client in real time; asynchronously accumulate extracted content in chunks. Response-stage hook runs per chunk + once at stream end. Cannot stop bytes already sent, but produces a complete audit trail and triggers post-hoc alerting on violation. |

Per-scope `fail_behavior` (`fail_open` or `fail_close`) determines what happens on hook timeout / error / oversize buffer.

## 8. Body capture & spillstore

Body capture is enabled per host / per provider. When enabled, the canonical prompt + completion text is extracted by the appropriate traffic adapter (cross-ref `provider-adapter-architecture.md`).

Two-tier storage keeps the hot path bounded:

- < 256 KiB → inline JSONB column on `traffic_event_payload`.
- ≥ 256 KiB → spillstore (S3 prod, local-FS dev). Row stores content-hashed reference; admin UI fetches via presigned URL.

Capture round-trips JSON, SSE text, multipart, and binary bytes losslessly.

## 9. Agent policy evaluation

Today's agent runs the **3 stages locally** with the same Go code. Admin exemptions and rule packs are both wired:

- **Exemptions ARE consulted at hook-decide time.** `core.Engine.Evaluate(host)` (`packages/agent/internal/policy/core/engine.go:79-89`) calls `exemptionStore.Load().IsExempt(host)` BEFORE the hook pipeline and returns `passthrough` on match. Wired via `wiring/compliance.go:57` + invoked at `wiring/bridge.go:145`.
- **Rule packs ARE consumed locally.** `AgentPipeline.ApplyRulePacksShadowState` (`packages/agent/internal/compliance/pipeline.go:279`) consumes the `installed_rule_packs` Cat B shadow key; `injectRulePacks` (pipeline.go:259, 341) injects `_rulePackInstalls` into matching `HookConfig.Config` maps before the pipeline runs.

Remaining gap: the `auto_exempt_cert_pinned` knob on `exemption.Store.ApplyShadowState` is accepted but not yet consumed at runtime — auto-exempt-on-cert-pin is still proxy-side only.

Cross-ref `agent-policy-eval-architecture.md` §5 and `agent-exemption-grants-architecture.md` §5 for the full wiring details.

## 10. `applicableIngress` filtering

Every `HookConfig` declares which paths it applies to via the uppercase-snake enum (`packages/shared/policy/hooks/core/types.go:245`):

- `["ALL"]` — runs on every traffic path.
- Any subset of `["AI_GATEWAY", "COMPLIANCE_PROXY", "AGENT"]` — runs only on the listed paths.

The Hub's change-signal only fans out to Things whose path appears in the subset, so an AI-Gateway-only hook does not bother the compliance proxy or agents.

This is also how new hooks roll out safely: start with `applicableIngress: ["AI_GATEWAY"]`, validate, then extend to `COMPLIANCE_PROXY`, then `AGENT`, or graduate to `["ALL"]`.

## 11. Adding a new hook

Checklist:

1. Implement `Hook` interface in `packages/shared/policy/hooks/<hook_name>/`.
2. Register in `init()`.
3. Wire metrics: per-hook decision counter (by verdict), latency histogram.
4. Define the `HookConfig` extension fields (if any) and add to the canonical schema.
5. Add unit tests (table-driven, cover each verdict + each `inflightAction × storageAction` combination the hook supports).
6. Smoke test through `/smoke-gateway` (or new dedicated skill if hook-specific).
7. Update `provider-adapter-architecture.md` if the hook needs a new extractor.
8. Document operational semantics (timeout, fail-open vs fail-closed) in the hook's package README.

## 12. Sources

- `packages/shared/policy/hooks/core/types.go` — `Hook` interface, `HookInput`, `HookResult`, verdict / `InflightAction` / `StorageAction` / `OnMatchConfig` types.
- `packages/shared/policy/hooks/{builtins,access,ratelimit,webhook,validators,contract}/` — built-in hook implementations.
- `packages/ai-gateway/internal/policy/hooks/` — AI-GW-specific contract-test mount (`doc.go`); production dispatcher wiring lives in `policy/hooks/` siblings of the other policy buckets.
- `packages/compliance-proxy/internal/conn/` — compliance-proxy hook integration.
- `packages/agent/internal/compliance/` — agent-side hook execution.
- `tools/db-migrate/schema.prisma` — `HookConfig` model (line 62; `stage` column allows `request | response | connection` per the schema comment at line 68).

## 13. Cross-references

- `compliance-pipeline-architecture.md` — where hooks run in compliance-proxy.
- `agent-forwarder-architecture.md` — where hooks run in the agent.
- `routing-architecture.md` — `applicableIngress` and per-route hook config.
- `audit-pipeline-architecture.md` — how hook decisions land in audit rows.
- `provider-adapter-architecture.md` — content extraction that hooks consume.
