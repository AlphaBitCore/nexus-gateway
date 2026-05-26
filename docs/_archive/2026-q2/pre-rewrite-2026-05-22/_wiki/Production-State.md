# Production State

Nexus Gateway is in production. The architecture is shipped, all five services are running, and the Control Plane, AI Gateway, Compliance Proxy, Hub, and three-platform Agent are serving real traffic as of the date of this document. This page describes exactly what "in production" means — and what it does not yet mean — so evaluators can make an accurate assessment of maturity.

---

## What is production-validated today

The following surfaces are in active production with real traffic, passing smoke tests, and cost/token audit records verified end-to-end.

**AI Gateway.** Five providers in production: OpenAI, Anthropic, Google (Gemini), Moonshot, and DeepSeek. 35 models across those five providers. Four ingress shapes: OpenAI `/v1/chat/completions`, OpenAI `/v1/responses`, Anthropic `/v1/messages`, and Gemini `:generateContent`. Full canonical-bus translation, cross-format routing (route OpenAI-shaped requests to Anthropic or Gemini adapters), prompt-cache integration, per-VK quota enforcement, and the full audit pipeline. The `/smoke-gateway --all-ingress` baseline is green.

**Compliance Proxy.** MITM TLS bump on `:3128` with an admin-trusted CA. Tier-1 adapters verified end-to-end for `chatgpt.com`, `claude.ai`, `gemini.google.com`, Cursor IDE, and the five AI-Gateway-deployed API hosts. Hook pipeline running per Hub-pushed `HookConfig`. Audit drain via mTLS + MQ fallback. The `/test-compliance-proxy` baseline is green.

**Desktop Agent.** Code complete and installable on macOS, Linux, and Windows. macOS uses `NETransparentProxyProvider` for metadata-level interception; content-aware hooks are deliberately limited by NE sandbox restrictions and will be expanded via E74 (macOS `pf`-intercept replacement). Linux (`pf`) and Windows (WinDivert) intercept paths have content-aware hooks active.

**Nexus Hub.** Thing Registry, Device Shadow (Category A / B / C config taxonomy), agent CA with Ed25519 attestation, audit chain pipeline with body capture and spillstore, scheduled jobs (retention purge, drift checker, agent-cert rotation), Prometheus metrics rollup — all in production.

**Control Plane.** Full IAM (resource catalog, NRN permissions, super-admin policy), virtual-key CRUD with budgets and quotas, provider/model catalog, routing rules (priority + weighting + LLM-dispatch smart routing), analytics (cost / savings / latency / cache-ROI), hook-config management, kill switch, SIEM bridge, IdP/SSO (OIDC + SAML JIT provisioning), JWT verifier (multi-issuer) — all serving real admin traffic.

**Control Plane UI.** Admin dashboard, theme system, i18n (EN / ZH / ES), design-token framework (light / dark themes), traffic audit drawer with NormalizedPayload viewer, and observability surfaces — deployed and in use.

---

## What is adapter-only (not yet production-validated)

The codebase contains adapter implementations for 14 additional provider spec packages beyond the five production-validated providers. These adapters exist in code, compile, and pass unit tests, but have not yet been exercised against a live upstream API in a production-equivalent smoke test.

| Category | Adapters awaiting validation |
|---|---|
| Bespoke codecs | `specs/azure`, `specs/bedrock`, `specs/cohere`, `specs/glm`, `specs/minimax`, `specs/replicate`, `specs/vertex` |
| OpenAI-compat siblings | `compat/fireworks`, `compat/groq`, `compat/huggingface`, `compat/mistral`, `compat/perplexity`, `compat/together`, `compat/xai` |

E72 (AI Gateway adapter verification) covers the per-adapter smoke test and `traffic_event` correctness validation for all 14. Until E72 closes for a given adapter, that adapter should be treated as best-effort rather than production-validated. The OpenAI-compatible siblings (S8–S14 in E72) are lower-effort; the bespoke-codec adapters (S1–S7) require more per-model wire testing.

Compliance Proxy Tier-1 adapter verification for providers beyond the five production-validated hosts is covered by E73. Approximately 40 adapters await systematic verification against their respective upstream endpoints.

---

## High-availability status

The current production deployment is a single-instance baseline. All five services run on one host; Postgres, Valkey, and NATS run as Docker containers on the same host. There is no load balancer, no replica set, no automated failover.

E81 (High-availability + multi-instance clustering) is planned. The architecture supports horizontal scaling of the stateless data-plane services (AI Gateway, Compliance Proxy) and the stateless Control Plane. Nexus Hub's Thing Registry and Shadow state live in Postgres, so Hub horizontal scaling requires session affinity or a distributed locking layer — the E81 design has not been finalised.

For evaluators assessing availability SLOs: the single-instance baseline provides gateway-level availability equal to the host's uptime. A rolling restart of any service takes that service offline for the restart duration (typically seconds). E81 closes this before any multi-tenant or high-availability SLA can be quoted.

---

## Air-gapped deployment status

The Nexus Gateway architecture is compatible with air-gapped deployment. All runtime dependencies are Go binaries + PostgreSQL + Valkey + NATS — no external network calls at runtime. Provider API calls go to the upstream LLM providers configured in the credential vault; for an air-gapped deployment using only on-premises inference endpoints (Ollama, vLLM, or any OpenAI-compatible self-hosted model server), no external network access is required by the gateway itself.

The [Air-Gapped Deployment Runbook](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/air-gapped-deployment.md) covers the step-by-step operator procedure: pre-bundled binaries, offline infrastructure bring-up, Prisma migration application, and provider configuration for both internal mirrors and self-hosted LLM servers.

---

## Performance baseline

From a production benchmark (k6 → prod Nexus → OpenAI, 28,591 requests, `gpt-4o`, mixed cache/no-cache workload):

| Metric | Value |
|---|---|
| Gateway-only p95, cache-miss | 2 ms (on top of an 11-second upstream call) |
| Cache-hit total p95 | 4–5 ms (no upstream call) |
| Long-context (16K tokens) gateway-only p95 | 28 ms |
| Cache hit rate under mixed workload | 70.5% |
| Cost reduction from cache alone | 61.7% |
| Gateway errors over 28,591 events | 0 |

Full report: [`docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md).

---

## Canonical docs

- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — single source of truth for epic status, production validation state, and what is planned vs shipped
- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — product overview and capability summary

**Adjacent wiki pages**: [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Comparisons](Comparisons) · [Roadmap Active](Roadmap-Active) · [AI Gateway Providers And Models](AI-Gateway-Providers-And-Models) · [Deployment Models](Deployment-Models)
