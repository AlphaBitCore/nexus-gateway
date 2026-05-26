# Features Index

Nexus Gateway ships as a 5-service architecture covering three independent traffic capture paths, a compliance hook pipeline, intelligent routing, caching, cost tracking, IAM, and a desktop agent for endpoint coverage. This section provides one focused page per major capability — each page is self-contained enough to share with a stakeholder evaluating that specific feature. Start here, follow the link that matches your concern.

---

## How to read this section

Each Feature page is a product card. It describes what the feature does, where it lives in Nexus, and how to enable it — written for an evaluator or decision-maker who may share it directly with a team or vendor. Feature pages do not duplicate the deep technical reference; each ends with a "Canonical docs" footer linking to the architecture documents for contributors who want implementation detail.

The ten features below map to the capability areas in the canonical feature matrix at [`features.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/features.md).

---

## Multi-provider routing

Nexus routes every AI request through a declarative strategy tree. A routing rule matches on model, virtual key, project, or provider, then dispatches through one of seven strategy types: single, fallback, load-balance, conditional, A/B split, policy-narrowing, or smart. Every routing decision is recorded in a `routing_trace` field on the traffic event for post-hoc debugging in the Control Plane UI.

[Feature-Multi-Provider-Routing](Feature-Multi-Provider-Routing)

## Smart routing

An optional LLM-dispatch layer that selects the best provider for each request based on the canonical prompt content, cost sensitivity, and available model capabilities. Smart routing always runs inside a PolicyNarrowing fence — the LLM cannot choose a provider or model the admin has not approved.

[Feature-Smart-Routing](Feature-Smart-Routing)

## Prompt cache

Nexus transparently manages provider-side KV-cache for Anthropic (`cache_control`), OpenAI (Service Tier auto-cache), and Gemini (explicit `cachedContents`). Cache-read and cache-creation token counts are stamped on every traffic event and factored into cost calculations. No application code changes are needed — the gateway emits the right directives per provider.

[Feature-Prompt-Cache](Feature-Prompt-Cache)

## Response cache

A gateway-managed cache with an exact-match tier (L1) and an optional semantic-similarity tier (L2), both backed by Valkey. L1 keys on the canonical request hash; L2 uses embedding KNN for paraphrased prompt matching. Both tiers record per-request cache savings. A time-sensitive prompt detector ensures stale content is never served for time-varying queries.

[Feature-Response-Cache](Feature-Response-Cache)

## Cost tracking

Every traffic event carries a stamped cost derived from the `Model` row's four price fields (input, output, cache-read, cache-write) via a single cost function. The Traffic Event drawer shows a per-request cost breakdown; the Cache ROI page shows aggregate savings. A `/v1/estimate` endpoint provides pre-flight estimates before committing to a request.

[Feature-Cost-Tracking](Feature-Cost-Tracking)

## PII redaction

The hook pipeline's PII Detector recognises emails, phone numbers, credit cards, SSNs, IBANs, API keys, JWTs, and private-key blocks using pattern matching plus checksum validation. Per-category default strategies (token, mask, hash) are configurable per route. The `inflightAction` and `storageAction` are independent — enforcement and audit retention are controlled separately.

[Feature-PII-Redaction](Feature-PII-Redaction)

## Audit and SIEM

Every AI interaction across all three traffic paths (AI Gateway, Compliance Proxy, Desktop Agent) lands in the `traffic_event` table with full hook decisions, routing traces, token usage, and cost data. Large request/response bodies spill to S3 or local filesystem. The SIEM bridge fans events out to Splunk HEC, Datadog, Elasticsearch, or any HTTPS webhook receiver.

[Feature-Audit-And-SIEM](Feature-Audit-And-SIEM)

## IAM and SSO

Nexus IAM is AWS-IAM-shaped: policies with Allow/Deny effects, resource NRNs, and action patterns. Evaluation is deny-overrides. Enterprise tenants federate admin logins through Okta, Azure AD, or any OIDC-compatible IdP; on first login a Nexus user is provisioned just-in-time and mapped to roles via IdP assertion claims.

[Feature-IAM-And-SSO](Feature-IAM-And-SSO)

## Desktop Agent

A Go binary installed on macOS or Linux workstations that intercepts AI-bound network traffic at the OS level — before it leaves the machine. On macOS it uses a Network Extension; on Linux it uses iptables NAT-redirect. The agent enrolls via mTLS, syncs hook policies via config sync, and uploads encrypted audit events to the Hub. macOS NE fail-open invariants are safety-critical and enforced in code.

[Feature-Desktop-Agent](Feature-Desktop-Agent)

## Hooks framework

A 3-stage (request / response / connection) extensible compliance pipeline that runs the same Go code across all three traffic paths. Eight built-in hooks cover PII detection, keyword filtering, content safety, rate limiting, request-size validation, IP filtering, webhook forwarding, and quality checking. Custom hooks and Rule Packs extend the framework without code changes.

[Feature-Hooks-Framework](Feature-Hooks-Framework)

---

## Canonical docs

- [`features.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/features.md) — canonical feature matrix with deployment mode per feature

**Adjacent wiki pages**: [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Use Cases](Use-Cases) · [Three Traffic Paths](Three-Traffic-Paths) · [AI Gateway Overview](AI-Gateway-Overview) · [Glossary](Glossary)
