---
doc: aiguard-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-21
---

# `packages/ai-gateway/internal/policy/aiguard/` Architecture

> **Tier 2 architecture doc.** Read this before adding an aiguard detector, changing the backend interface, modifying the classify pipeline, or wiring a new aiguard hook. Created during the 2026-05-16 open-source readiness review when the subpackage was found to be production-substantial (12 .go, 11 _test.go) but without a dedicated architecture doc.

## 1. What aiguard is

The AI-safety classification subsystem of the AI Gateway. When a hook needs to "classify" a request or response with a **judge model** (typical use cases: detect prompt injection, jailbreak content, off-topic abuse, PII attempts that pattern-match alone can't catch), it calls into `aiguard.Classify(...)`.

Architecturally distinct from `compliance/` and the hook implementations in `shared/hooks/`:

- `shared/hooks/` (keyword filter, regex PII, rate limiter, etc.) — **deterministic** classifiers: pattern matches, threshold checks. Fast, cheap, no upstream call.
- `aiguard/` — **model-based** classifier: calls an external judge model with a structured prompt and parses the verdict. Slower, more expensive, but handles cases deterministic rules miss.

Both can be wired in the same hook config; aiguard hooks declare a `BackendID` referencing one of the configured judge backends.

## 2. Backend interface

`aiguard.Backend` is the minimal seam:

```go
type Backend interface {
    Call(ctx context.Context, prompt string) (*Response, error)
}
```

Two concrete implementations:

- **`ExternalBackend`** (`backend_external.go`) — calls a configured external HTTP endpoint with the judge prompt. Used for self-hosted judge services or commercial detection APIs.
- **`AdapterBackend`** (`backend_provider.go`) — calls back through the AI Gateway's own provider-adapter chain (treats the judge model as just another `(provider, model)` pair). Used when the judge model is one of the gateway's already-configured providers.

Tests use stub backends via the same interface.

## 3. Classify pipeline

`Classify(ctx, req, cfg, backend, cache, sink)` (`classify.go:94-103`) delegates to `classifyImpl` (`classify.go:181-303`), which runs:

0. **Input staging** — when `req.Messages` is non-empty, `applyInputStaging` calls `inputstaging.Plan` to fit the judge's context window per `RuntimeConfig.InputStrategy` / `ModelContextLimit` and joins into a flat `req.Content`. Overflow is logged + counted but never blocks the request (fail-open).
1. **Validate** — `req.DetectorType` and `req.Content` are required; missing fields return a plain error (caller-contract violation, 400).
2. **Normalize + key** — `canonicalizeForCacheKey(req.Content)` (`normalize.go`) and `CacheKey(req.DetectorType, normalized, cfg.BackendFingerprint)` (`cache.go`).
3. **Cache lookup** — Redis-backed; hits stamp `Metadata.CacheHit=true`, emit an audit event, bump `DecisionsTotal`, and return immediately.
4. **Render prompt** — `Render(cfg.PromptTemplate, RenderInput{...})` (`prompt.go`) produces the judge prompt; render failures are mapped to `BackendUnavailable`.
5. **Backend call** — invokes `backend.Call(callCtx, prompt)` under a `cfg.TimeoutMs` budget (default 30s, matching the `ai_guard_config.timeout_ms` admin-UI default).
6. **Cache write** — `cache.Set(ctx, key, resp, ttl)` with `cfg.CacheTTLSeconds`.
7. **Emit audit + decision** — `TrafficSink.Emit` records the event (internal purpose `"ai-guard"` per `classify.go:89`) and `DecisionsTotal` is bumped.

Backend replies are parsed by `DecodeJudgeOutput` (`decoder.go:33`) into `Response{Decision, Confidence, Reason, Labels, Redactions, Metadata}` (`types.go:74-99`). `Decision` is one of `"approve" | "reject_hard" | "block_soft" | "modify"`.

A `BackendUnavailable` error is fail-open by design: when the judge backend is down, the hook chooses the fail-open / fail-closed policy declared in its config. Backend availability never blocks user-facing traffic without an explicit "fail closed" choice.

## 4. Configuration

`ConfigCache` (`config_cache.go`) holds the active `*configstore.AIGuardConfig` — loaded from the singleton `ai_guard_config` DB row via `configstore.AIGuardStore.Load`. The cache uses a lock-free atomic-pointer happy path (`config_cache.go:62-83`) with a single-flight slow path; on persistent loader failure it serves the stale snapshot (fail-open) rather than blocking the hot path.

Invalidation is push-based: the configdispatch loader registers `configkey.AIGuard` (constant `"ai_guard"`, `packages/shared/schemas/configkey/configkey.go:44`); when the Hub-pushed shadow signal fires, the loader calls `ConfigCache.Invalidate()` (`packages/ai-gateway/cmd/ai-gateway/configdispatch/configdispatch.go:300-313`), so the next `Get` re-reads the DB. The TTL acts as a safety net rather than the primary refresh mechanism.

`BackendFingerprint(mode, providerOrURL, model, promptTemplateSHA)` (`fingerprint.go:15`) returns a 64-char lowercase hex sha256 and feeds the cache key, so swapping the judge model / endpoint / prompt template invalidates only its own cached verdicts rather than the whole namespace.

## 5. Inproc and metrics

`InProcClient` (`inproc.go:13-51`) bundles `ConfigCache + backendFor + Cache + TrafficSink` and exposes a single `Classify(ctx, req)` method for ai-gateway hooks running in the same binary (P-D prompt-injection, P-E quality-checker). It looks up the current `AIGuardConfig` on every call, projects it into a `RuntimeConfig`, resolves the backend via the injected `backendFor` factory, then delegates to the package-level `Classify`. Constructed once in `main.go` and shared by all hooks.

`metrics.go` registers seven Prometheus collectors under the `nexus_aiguard_*` namespace (`metrics.go:9-53`): `cache_hits_total`, `cache_misses_total`, `cache_writes_total`, `judge_latency_seconds{backend_mode}`, `judge_errors_total{backend_mode, kind}`, `decisions_total{detector_type, decision}`, and `input_overflow_total{overflow_kind}`. Naming follows `prometheus-naming-architecture.md`.

## 6. Failure modes

| Failure | Behavior |
|---|---|
| Backend HTTP error / timeout | `BackendUnavailable` returned; hook policy decides fail-open vs fail-closed. |
| Cache (Redis) unavailable | Fall through to backend call; cache write-through fails silently. Cache is advisory. |
| Decoder failure (malformed verdict) | Treated as backend unavailable. |
| Config snapshot empty (cold-start before first Hub pull) | Hook short-circuits to "no judgment available"; policy decides. |

## 7. Sources

- `packages/ai-gateway/internal/policy/aiguard/classify.go` — the Classify entry point + `Backend` interface + `TrafficEvent` + `TrafficSink`.
- `packages/ai-gateway/internal/policy/aiguard/backend_{external,provider}.go` — the two backend implementations.
- `packages/ai-gateway/internal/policy/aiguard/{cache,config_cache}.go` — Redis cache + config snapshot.
- `packages/ai-gateway/internal/policy/aiguard/{normalize,prompt,decoder,fingerprint}.go` — pipeline helpers.
- `packages/ai-gateway/internal/policy/aiguard/inproc.go` — in-process entry point.
- `packages/ai-gateway/internal/policy/aiguard/metrics.go` — Prometheus instrumentation.

## 8. Cross-references

- `hook-architecture.md` — aiguard hooks are one species of hook, wired the same way as deterministic ones.
- `cache-multi-tier-architecture.md` — aiguard's verdict cache is one tier in the cache zoo.
- `prometheus-naming-architecture.md` — `nexus_aiguard_*` counter conventions.
- `provider-adapter-architecture.md` — `AdapterBackend` invokes the gateway's own adapter chain to reach the judge model.
