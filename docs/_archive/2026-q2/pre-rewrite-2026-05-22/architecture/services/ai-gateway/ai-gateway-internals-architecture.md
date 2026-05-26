---
doc: ai-gateway-internals-architecture
area: service
service: ai-gateway
tier: 1
updated: 2026-05-21
---

# `packages/ai-gateway/internal/` — Internal Subpackages Reference

> **Tier 3 architecture doc.** Reference card for the ai-gateway subpackage layout, with a one-paragraph summary of each bucket that doesn't already own a dedicated architecture doc. Refreshed 2026-05-21 to track the post-refactor 11-bucket layout (auth/cache/config/credentials/execution/ingress/platform/policy/providers/routing/runtimeapi).

This doc does **not** cover the buckets that already have their own arch docs — when a subpackage maps to one of those, follow the cross-reference instead of duplicating here:

- `routing/{capability,core,llm,matcher,strategies}/` + `execution/executor/` + `execution/canonicalbridge/` + `policy/requestcontext/` — `routing-architecture.md`
- `providers/{builtins,canonicalext,core,dispatch,specs,specutil,target}/` + `execution/forwardheader/` + `execution/wireformat/` — `provider-adapter-architecture.md`
- `cache/{core,layer,stream,gemini,semantic,freshness,budget}/` — `prompt-cache-architecture.md` + `response-cache-architecture.md`
- `policy/aiguard/` — `aiguard-architecture.md`
- `policy/quota/` + `policy/ratelimit/` — `quota-architecture.md`
- `platform/audit/` — `audit-pipeline-architecture.md`
- `credentials/{decrypt,manager,pool,stats}/` + `auth/vkauth/` — `credentials-architecture.md`
- `execution/passthrough/` — `emergency-passthrough-architecture.md`
- `runtimeapi/` — `runtime-introspection-architecture.md`
- `platform/streaming/` — `provider-adapter-architecture.md` + `shared/streaming/` (covered in `shared-utility-subpackages-architecture.md`)
- `platform/metrics/` — `prometheus-naming-architecture.md`
- `policy/hooks/` — see `doc.go` in the directory (a contract-test mount, not production code; the production hook framework lives in `packages/shared/policy/hooks/` — covered by `hook-architecture.md`)
- `config/` — `service-bootstrap-config-architecture.md`
- `execution/estimator/` + the `nexus.dry_run` branch — `cost-estimation-architecture.md`

## `ingress/`

The **HTTP entry surface** of `/v1/*` traffic. Five sub-buckets, plus the proxy itself:

- `ingress/proxy/` — the main `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/embeddings`, `:generateContent` proxy path. Densest tested surface in the gateway. Convention: handlers do request parsing + ingress-format normalization + vkauth + cache lookup + routing + executor invocation; business logic stays in the subsystem packages. Notable files:
  - `proxy.go` + `proxy_cache.go` — non-stream and stream paths with response-cache integration (extract-cache + semantic-cache pre-lookup, in-flight singleflight coalesce).
  - `cross_format.go` — handles requests that arrive in one ingress format (e.g. Anthropic Messages) targeting a non-matching upstream (e.g. OpenAI); runs through `execution/canonicalbridge` to convert.
  - `dry_run.go` — the `nexus.dry_run: true` branch that returns an estimator-shaped response instead of forwarding upstream.
  - `estimate.go` — the `POST /v1/estimate` endpoint (predicted cost, no forwarding).
  - `ingress.go` + `ingress_model.go` — ingress-format detection + model-id extraction for routing.
  - `traffic_adapter.go` — bridges the proxy into the shared traffic-event audit pipeline.
  - `interfaces.go` — DI seams (`Resolver`, `CacheStore`, `HookRunner`, etc.).
- `ingress/proxy/classify/` — the `/v1/classify` endpoint that drives ai-guard directly without going through the proxy hot path.
- `ingress/models/` — the `/v1/models` listing endpoint.
- `ingress/envelope/` — provider-error → ingress-format error-envelope shaping (`error_envelope.go`). Translates an upstream 4xx/5xx into the OpenAI / Anthropic / Responses-API error shape the caller expects.
- `ingress/debug/` — admin probe endpoints (`credential_probe_endpoint.go`, `provider_test_endpoint.go`, `hooks_test_endpoint.go`) used by the CP "test connection" / "test hook" UI surfaces.

When adding a new ingress endpoint: implement the handler under the right sub-bucket, register routes in `cmd/ai-gateway/wiring/routes.go`, add IAM-action check at the route level if admin-scoped, write the corresponding `_test.go`.

## `platform/`

Cross-cutting infrastructure that doesn't belong to any single business domain:

- `platform/audit/` — the audit emit path (covered by `audit-pipeline-architecture.md`).
- `platform/metrics/` — Prometheus instrumentation (covered by `prometheus-naming-architecture.md`); also home of `metrics.CalculateCost` (covered by `cost-estimation-architecture.md`).
- `platform/middleware/` — Echo middleware that runs before handlers reach business logic: request-ID stamping, vkauth-token extraction (the token-validation logic itself is in `auth/vkauth/`), CORS, panic recovery. Each middleware exposes an `echo.MiddlewareFunc` constructed from injected dependencies.
- `platform/store/` — the ai-gateway's **read-side DB layer**. pgx-backed lookups for Provider / Model / Credential / VirtualKey / etc. — the source of truth that `cache/layer/` snapshots into memory. This is the *read* path only; admin writes go through the Control Plane's handler → Hub → shadow signal → ai-gateway reload pipeline. Convention: one file per table (`provider.go`, `credential.go`, …); `Get*` / `List*` functions return typed structs; pgx errors wrapped with context.
- `platform/streaming/` — SSE response-writing side (the inbound SSE parsing side lives in `shared/transport/normalize/extract/`); covered by `provider-adapter-architecture.md`.

## `policy/`

Per-request decisioning. Each sub-bucket carries its own architecture doc except `requestcontext/`:

- `policy/aiguard/` — `aiguard-architecture.md`.
- `policy/hooks/` — see `hook-architecture.md` (note: this directory is a contract-test mount; the real hook framework lives in `packages/shared/policy/hooks/`).
- `policy/quota/` + `policy/ratelimit/` — `quota-architecture.md`.
- `policy/requestcontext/` — per-request context plumbing. Two main types:
  - `context.go` — the `RequestContext` struct: the L3 immutable artefact built once at Phase 3.5 with the authenticated VK identity (`*vkauth.VKMeta`), the canonical `*normcore.NormalizedPayload`, the endpoint family string, the inbound `http.Header`, and the raw request body. Constructed via `NewBuilder().With….Build()`; getters are nil-receiver-safe.
  - `resolved.go` — `ResolvedRequest`: the L4 view built at Phase 4.5 via `Resolve(rc, route, ptc)` that bundles the wrapped `*RequestContext` with the post-routing `*routingcore.RouteResult` and the effective `*passthrough.Config` for the picked primary target. Stashed on `context.Context` via `WithResolved` / `ResolvedFrom`. Consumed by `execution/executor/`, the hooks pipeline, audit Writer, and response normalize.
  
  The split between `policy/requestcontext/` and `routing/`: `requestcontext` owns the *carrying* types; `routing/` owns the *evaluation logic that produces them*. Both are dependency-light packages; widely imported by handler, executor, hooks, audit.

## `execution/`

The dispatch + payload-transform layer:

- `execution/executor/` — consumes `ResolvedRequest`, runs the fallback chain, classifies errors via `error-taxonomy-architecture.md`'s `ErrorClass`. Covered by `routing-architecture.md` §4.
- `execution/canonicalbridge/` — converts ingress requests to/from the canonical (OpenAI chat-completions) shape; also home of `DecodeViaShared` that delegates response parsing to `shared/transport/normalize/`. Covered by `normalization-architecture.md` "Ai-gateway codec delegation (E58-S0)".
- `execution/estimator/` — the dry-run / `/v1/estimate` cost predictor. Covered by `cost-estimation-architecture.md` §4.
- `execution/passthrough/` — covered by `emergency-passthrough-architecture.md`.
- `execution/forwardheader/` — request/response header sanitization + forwarding rules.
- `execution/wireformat/` — wire-format identification helpers shared by codec and capability filter.

## When you change one of these

- If a subpackage gains its own architecture surface (e.g. `ingress/proxy/` grows a substantial new subsystem), promote it: write a dedicated Tier-2 doc, add a row to `architecture-doc-triggers.md`, remove the entry from this card.
- `policy/requestcontext/` types are widely imported — additive-only changes to `RequestContext` / `ResolvedRequest` fields; never rename or repurpose without coordinating with the executor + ingress + audit consumers.

## Sources

- `packages/ai-gateway/internal/ingress/{proxy,proxy/classify,models,envelope,debug}/`
- `packages/ai-gateway/internal/platform/{audit,metrics,middleware,store,streaming}/`
- `packages/ai-gateway/internal/policy/{aiguard,hooks,quota,ratelimit,requestcontext}/`
- `packages/ai-gateway/internal/execution/{executor,canonicalbridge,estimator,passthrough,forwardheader,wireformat}/`
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — single route-registration site.
