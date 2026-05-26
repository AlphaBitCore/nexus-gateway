# FAQ Product

Nexus Gateway is an enterprise AI traffic gateway that enforces compliance, routing, caching, and audit policies across every AI API call in an organization. This page answers the product questions a serious evaluator typically asks before committing to a deployment. Answers are grounded in the source code and documentation; follow the canonical-docs links at the end for deeper detail.

---

## Drop-in compatibility and modality support

**Q: Is Nexus a drop-in OpenAI replacement?**

For applications already using the OpenAI SDK, yes. Point `base_url` at the AI Gateway
(`http://<host>:3050/v1`), replace the provider API key with a Nexus virtual key, and
the gateway handles `/v1/chat/completions`, `/v1/responses`, and `/v1/embeddings`
transparently. The gateway translates every outbound request into the correct provider
wire format on the way out — the caller's code does not change when switching providers.
Streaming, function/tool calling, structured outputs, and vision inputs all pass through
unchanged from the caller's perspective. The canonical bus also supports cross-format
routing: a caller using the OpenAI ingress shape can be routed to Anthropic, and the
adapter translates the wire format transparently.

**Q: Does Nexus support streaming (SSE)?**

Yes. Streaming (Server-Sent Events) is supported across all three traffic paths — AI
Gateway, Compliance Proxy, and Desktop Agent — using the same shared Go compliance
pipeline code. Per-host and per-provider streaming compliance modes are configurable:

- `passthrough` — relay only, no hook, no body capture. Use for non-AI traffic that is
  allowed through but should not be inspected.
- `buffer_full_block` — assemble the full upstream response before forwarding a single
  byte to the client. The response-stage hook runs once at stream end; on hard reject
  the proxy returns HTTP 451 and never forwards the upstream body. Trades real-time UX
  for the ability to block before the client sees any content.
- `chunked_async` — relay bytes to the client in real time and asynchronously accumulate
  the extracted content in chunks. The response hook evaluates per chunk and at stream
  end. Cannot recall bytes already delivered, but produces a complete audit trail and
  triggers post-hoc alerting when content violates policy.

Each scope additionally chooses its `fail_behavior` (`fail_open` or `fail_close`) for
hook errors, timeouts, and oversize buffers. See `docs/users/product/features.md` §2 for
the full trade-off table.

**Q: Does Nexus support reasoning models?**

Yes. Reasoning tokens — OpenAI o-series, Anthropic extended thinking, Moonshot kimi
`reasoning_content`, Gemini `thoughtsTokenCount` — pass through the canonical translation
layer and are preserved end-to-end. The provider adapter normalizes the wire format so
callers receive a consistent shape regardless of which provider generated the reasoning
tokens. Reasoning tokens are extracted, counted, cost-stamped, and recorded in the
`traffic_event` row. When a provider does not expose explicit reasoning token counts
(Moonshot/kimi, Anthropic on prompts that do not trigger thinking blocks), the gateway
derives an estimate from the `reasoning_content` field character length using a
chars/3.5 heuristic.

**Q: Can I call Anthropic, Google Gemini, DeepSeek, or other providers through Nexus?**

Yes. The AI Gateway supports four ingress shapes: OpenAI `/v1/chat/completions`, OpenAI
`/v1/responses`, Anthropic `/v1/messages`, and Gemini `:generateContent`. Callers can
use any of these ingress shapes regardless of which provider ultimately handles the
request — the cross-format canonical bus translates transparently. Five providers are
production-validated with real traffic: OpenAI, Anthropic, Google Gemini, Moonshot, and
DeepSeek, across 35 models. Additional adapters (Azure OpenAI, Cohere, GLM, MiniMax,
Fireworks, Groq, Mistral, xAI, Vertex, Replicate, and more) are shipped in the codebase
and pending end-to-end verification under E72.

**Q: What modalities are supported today?**

Chat completions, streaming responses (SSE), function/tool calling, vision inputs (image
URLs and base64), structured outputs, reasoning tokens, and embeddings are all supported.
Audio, image generation, and video endpoints are deferred — they are not planned for the
current development cycle pending customer signal. The endpoint typology framework (E62)
is in place and supports future modality extensions without architecture changes.

---

## Security, credentials, and encryption

**Q: Where are provider API keys stored, and how are they encrypted?**

Provider API keys are stored in PostgreSQL, encrypted at rest with AES-256-GCM. The
encryption key comes from the `CREDENTIAL_ENCRYPTION_KEY` environment variable — a
secret that must be set for both the Control Plane (which writes credentials) and the AI
Gateway (which reads them). This key is never stored in database rows or YAML config
files; Nexus enforces a "secrets are env-only" rule as a project-level binding. The raw
provider key value never lands in the database — only the AES-256-GCM ciphertext is
persisted. Virtual keys are matched server-side against HMAC-SHA256 hashes; the raw
virtual key string is also never stored.

**Q: How do I rotate provider credentials?**

Update the credential in the Control Plane (Credentials section of the admin UI) and
save. The Control Plane writes the new encrypted ciphertext to PostgreSQL, the Nexus Hub
signals every AI Gateway node over its persistent WebSocket connection, and each node
pulls the updated credential and hot-swaps its in-memory copy via an atomic pointer —
the change propagates in under a second without any service restart. The rotation is
fully zero-downtime: in-flight requests using the old credential complete normally while
new requests immediately see the new credential. Rollback is equally instant: save the
old ciphertext back via the UI.

**Q: Is the audit log persisted on disk? Can it be forwarded to a SIEM?**

Every AI interaction is written to the `traffic_event` table in PostgreSQL via an
asynchronous NATS JetStream pipeline — the request hot path never blocks on database
writes. Each audit row includes: source service, provider, model, virtual key ID, token
counts (prompt/completion/reasoning/cache), cost (USD stamped per request), latency
phases (TTFB and upstream total), hook pipeline results per stage (request and response),
data classification label (Public/Internal/Confidential/Restricted), and a spillstore
reference for large payloads. A SIEM bridge built into Nexus Hub forwards audit events to
external SIEM systems via webhook or OTEL sinks; configuration lives in the admin UI
(Security → SIEM Bridge).

**Q: What data classification labels are assigned to requests?**

Every AI interaction is classified into one of four sensitivity levels: Public, Internal,
Confidential, or Restricted. The classification is the highest sensitivity level returned
by any hook in the pipeline — a single Confidential detection overrides all Public
results from other hooks. Rules are configured through the admin dashboard (keyword
patterns, PII category toggles, and per-hook sensitivity thresholds). Today's PII
detector covers four categories (email, phone, SSN, credit card) in English regex
patterns. Multi-language PII expansion is a documented gap relative to Kong AI Gateway
(20 categories × 12 languages); it is on the backlog but not yet planned for the current
cycle.

---

## Availability, fail-open behavior, and infrastructure

**Q: What happens if Nexus Hub goes down — does AI traffic stop?**

No. The three data-plane services (AI Gateway, Compliance Proxy, Desktop Agent) are
fail-open by default. Each service caches the last pulled configuration locally and
continues processing traffic using that snapshot. Hook errors and loss of control-plane
connectivity degrade enforcement (alerts fire) but do not block requests.

The macOS Network Extension applies an even stricter fail-open rule: `handleNewFlow` must
decide synchronously, and every async daemon callback has a hard 2-second passthrough
timeout, so a Hub outage cannot hang the Mac's entire outbound network stack — including
DNS, DHCP, NTP, and Apple Push notifications.

The Emergency Passthrough feature (E48) provides an explicit fail-open envelope when the
compliance pipeline itself is the problem, with a maximum 8-hour expiry (default 1 hour)
and automatic Hub reconciliation every 60 seconds. When active, requests bypass hooks
but the audit trail is still emitted.

**Q: What is the production state today? Is Nexus GA?**

Nexus Gateway is in production and serving real traffic, but it is labelled pre-GA
because it currently targets single-tenant on-premises/self-hosted deployment. The full
5-service architecture is shipped: AI Gateway (5 providers, 35 models, 4 ingress shapes),
Compliance Proxy (MITM TLS on production domains), Hub (Thing Registry, shadow, audit,
jobs), Control Plane (admin API + UI with IAM, SSO, analytics, alert rules), and Desktop
Agent (macOS/Linux/Windows). Open roadmap items are extensions of already-shipped
systems, verification coverage for already-coded adapters, and quality/productization
work — not first-build architecture gaps.

**Q: Is high-availability supported?**

Not yet in the current release. Today's production topology is a single node co-locating
all server-side services. The stateless service tier and pull-only config-sync model are
designed to support multi-instance deployment without changing data-plane contracts — the
Hub Thing Registry already handles multiple instances of the same service kind in its
schema. Multi-instance clustering and HA with documented RTO/RPO SLOs are planned under
E81. This is flagged in the roadmap as an uptime risk that should land before E78
(self-hosted local inference) to avoid concentrating risk on one host.

**Q: Can Nexus run in air-gapped mode?**

The server-side services (Hub, Control Plane, AI Gateway, Compliance Proxy) have no
required outbound internet dependencies at runtime — all provider calls go through the
AI Gateway to configured upstream endpoints, which can be private or on-premises
inference servers (including local models via any OpenAI-compatible server). The Desktop
Agent requires connectivity to the Hub (which is on-premises) but not to the internet
directly.

Architecture is compatible with air-gapped deployments. The [Air-Gapped Deployment Runbook](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/air-gapped-deployment.md) covers the full procedure: pre-bundling Go binaries and the CP UI on a connected build host, transferring artifacts via an approved channel, bringing up PostgreSQL + Valkey + NATS on-premises, deploying the four Go services, applying Prisma migrations offline, and configuring provider credentials for either an internal LLM mirror or a self-hosted model server (Ollama, vLLM, llama.cpp). Build-time Go module downloads require internet access or a private GOPROXY; `go.work` keeps the build reproducible once the workspace is present.

**Q: What backing infrastructure does Nexus require?**

PostgreSQL 16, Valkey 8 (Redis-wire-compatible, BSD-3-Clause licensed), and NATS
JetStream 2+ are the three required backing services. For local development all three
start via `docker-compose.yml`. In production, the current reference deployment
co-locates all server-side services on a single host fronted by nginx. Valkey is used
instead of Redis to avoid the SSPL license dependency that Redis 7.4+ introduced for
self-hosted deployments.

---

## Desktop Agent and compliance behavior

**Q: Does the Desktop Agent see request bodies on macOS?**

The macOS Network Extension (`NETransparentProxyProvider`) operates at the network layer
and surfaces connection metadata — host, IP, port, and process bundle ID — to the Go
agent daemon. The NE extension forwards inspect-mode flows to a localhost bridge socket
where the Go daemon's `tlsbump` package performs TLS MITM; content-aware hooks run
end-to-end on that path.

However, the NE architecture has structural coverage gaps: QUIC/UDP blind spots (apps
using HTTP/3 bypass the NE TCP intercept), raw-socket bypass by some apps, per-process
attribution drift for helper-process designs (Chrome helpers, Electron utility
processes), and pass-throughs caused by the 2-second async-callback fail-open timeout.
A `pf`-based transparent intercept that closes all five gap classes is planned under E74.

Full content-aware hooks with broader coverage are active today on Linux (`pf`/iptables
transparent redirect) and Windows (WinDivert kernel-mode capture).

**Q: Can the gateway block a request after the response has started streaming?**

It depends on the configured streaming mode. In `buffer_full_block` mode the proxy holds
the entire upstream response before forwarding a single byte; it can return HTTP 451 and
drop the upstream body entirely if any hook rejects the response content. In
`chunked_async` mode bytes are already flowing to the client when the response hook
evaluates, so a hard reject cannot recall bytes already delivered — the hook triggers
post-hoc alerting and audit instead.

Choose `buffer_full_block` when the ability to suppress a response before the client sees
it is required; accept the added latency for long completions (the full response must be
buffered before forwarding begins). For interactive chat applications where latency
matters more than suppression, `chunked_async` with alerting is the typical choice.

**Q: How large can a request body be before it spills to S3?**

Bodies up to 256 KiB are written inline into the `traffic_event_payload` JSONB column in
PostgreSQL. Bodies larger than 256 KiB are written through the pluggable `SpillStore`
backend (S3 in production, local filesystem in development) and the audit row stores a
content-hashed reference. Operators retrieve the original bytes from the admin UI by
following the reference link, which resolves to a presigned S3 URL with a short expiry.
The 256 KiB threshold keeps the PostgreSQL audit hot path bounded while the spillstore
handles large prompts and full completion bodies losslessly.

---

## Fleet management and multi-platform support

**Q: How does Nexus manage a fleet of Desktop Agents across the organization?**

Agents enroll with Nexus Hub using one-time enrollment tokens. Hub runs a self-issued
ECDSA P-256 Certificate Authority and signs each agent's CSR to issue an mTLS client
certificate. After enrollment, agents are visible in the admin UI (Infrastructure →
Nodes) with their status (online/degraded/offline), operating system, version, and
config-sync state (target config vs. applied config).

Policy and configuration changes propagate automatically: the admin saves a hook config
or routing rule in the Control Plane UI; Hub updates the target config; the agent
receives a change signal over its persistent WebSocket and pulls the updated config.
No agent restart is required. Config-sync state (whether the agent has applied the
latest target) is visible per-node in the Infrastructure → Config Sync page.

**Q: What operating systems does the Desktop Agent support?**

macOS, Linux, and Windows are all supported. The agent is dev-complete on all three
platforms. macOS uses `NETransparentProxyProvider` for network-layer interception.
Linux uses `pf`/iptables transparent redirect. Windows uses WinDivert kernel-mode
capture. macOS has the most content-aware coverage gaps today (see the macOS section
above); full cross-platform end-to-end verification is planned under E75.

---

## Performance, caching, and routing

**Q: What is the gateway overhead in practice?**

Based on a benchmark of 28,591 requests through the AI Gateway to OpenAI (`gpt-4o`,
mixed cache/no-cache workload), gateway-only overhead at p95 on a cache-miss is **2 ms**
on top of an ~11-second upstream call. Cache hits (no upstream call at all) complete
end-to-end at **4–5 ms** p95. Long-context requests (16K tokens) show **28 ms** p95
gateway-only overhead — under 1% of total wall time. Zero gateway errors occurred across
all 28,591 events. Full methodology and raw numbers are in
[`docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/operators/ops/runbooks/perf-2026-05-20-nexus-traffic-event.md).

**Q: What cache hit rates are realistic?**

Under a mixed-workload benchmark, the cache hit rate was **70.5%**, corresponding to a
**61.7%** reduction in upstream provider cost. Results vary significantly with workload:
embeddings and deterministic structured extractors tend toward higher hit rates;
creative generation with temperature > 0 tends toward lower. The multi-tier cache
cooperates: L1 process-local LRU, L2 Valkey shared semantic vector cache (exact-match
+ cosine similarity), L3 provider-native prompt cache (Anthropic `cache_control`
auto-injected, Gemini cached content), and L4 semantic vector cache. An in-flight broker
coalesces concurrent identical prompts into one upstream call, reducing costs further
for high-concurrency identical prompts.

**Q: How does routing work? Can I route different models to different providers?**

Yes. Routing rules are declarative configurations in the AI Gateway, evaluated by priority
order — the first matching rule wins. Seven strategy types are available: Single (one
provider+model), Fallback (try in order on failure), Load Balance (weighted random
across providers), Conditional (match on model name, model type, virtual key, or
project), A/B Split (weighted random for experimentation), Policy Narrowing (restrict
eligible candidates), and Smart (LLM-dispatch — an LLM picks the optimal target from
canonical request semantics). Routing rules are configured through the admin UI without
code changes or service restarts.

**Q: What happens if an upstream provider returns an error?**

Provider errors are normalized by the spec adapter's error normalizer into a canonical
error shape and returned to the caller with an appropriate HTTP status code. In a
Fallback routing strategy, a configured provider error triggers the fallback chain —
the gateway transparently retries with the next configured target. Errors are recorded
in the `traffic_event` row for audit and alerting. Provider-specific HTTP rate-limit
headers and retry-after semantics are handled by each adapter; the canonical error shape
exposes `available_capabilities` for client self-debugging when a request is rejected
due to a capability mismatch (e.g., routing an embedding request to a chat-only model).

**Q: How does quota enforcement work?**

Quotas are tracked per-organization, per-project, and per-virtual-key. The AI Gateway
checks the quota tier before routing and reconciles on completion using PostgreSQL as the
source of truth with Valkey-accelerated sliding-window counters for hot-path performance.
Burst allowance and overage handling (hard block vs. soft alert) are configurable per
quota policy. Organization hierarchies propagate quotas through ancestor paths — a
parent-org cap constrains all child orgs. Quota counters are visible in the admin UI
analytics surface alongside cost and token usage.

**Q: Can I restrict which models a developer or team can use?**

Yes, through virtual keys. Each virtual key can be configured with an allowed-model list
at creation time. A request using that key to a model outside the allowed list is
rejected at the gateway before it reaches any upstream provider. Virtual keys are
project-scoped and can also carry per-key quotas and budget caps, so different teams
can have different model access profiles and spending limits with no code changes on the
application side.

**Q: Does Nexus support SSO and external identity providers?**

Yes. The Control Plane supports OIDC and SAML federation with external IdPs (Okta,
Azure AD, Google Workspace, or any generic OIDC/SAML provider). Nexus is the SP
(Service Provider); on first successful federation a Nexus user is provisioned
just-in-time and mapped to roles via IdP assertion claims. Local Nexus accounts remain
available as a fallback for break-glass scenarios. SAML runtime is currently planned
under E87 — the IdP type enum stub and config structs are already shipped in code.
OIDC federation is fully operational in production today.

**Q: What does the async audit pipeline look like — does it add latency?**

The request hot path never blocks on database writes. After the response is forwarded to
the caller, audit events (token usage, hook decisions, latency phases, cost, and data
classification) are written to a NATS JetStream stream via the `shared/mq` interface.
A Hub-side consumer drains the stream into PostgreSQL asynchronously. Bodies larger than
256 KiB are spilled to S3 without blocking the caller. This design keeps the gateway's
per-request overhead at 2 ms p95 while producing a complete durable audit trail. NATS
JetStream provides at-least-once delivery with acknowledgements, so audit rows are not
silently lost even if the Hub consumer restarts mid-drain.

**Q: Is there a way to test without paying for upstream AI calls?**

Yes. Every ingress (chat completions, responses, messages, embeddings) supports a
dry-run mode via the `x-nexus-dry-run: true` request header. In dry-run mode the
gateway runs the full compliance pipeline, estimates token counts from the canonical
request, stamps a `dry_run_cost_usd` estimate in the response, and writes an audit row
— but never calls the upstream provider. This is useful for verifying routing rules,
quota policies, hook configurations, and cost estimates without incurring provider
charges. Dry-run is also available via the Control Plane UI's routing-rule test form.

---

## License and security reporting

**Q: What license is Nexus Gateway under?**

Apache License 2.0. See [`LICENSE`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/LICENSE)
for the full text. Commercial use, modification, distribution, and private use are all
permitted. Attribution (preserving the license and NOTICE file) is required. The project
has no CLA requirement for contributions. The cache backing service is Valkey 8
(`valkey-bundle` Docker image), which is BSD-3-Clause licensed — there is no Redis SSPL
dependency in the stack.

**Q: How do I report a security vulnerability?**

Email **security@alphabitcore.com** with a description of the vulnerability, the affected
component (`packages/ai-gateway`, `packages/agent`, `packages/compliance-proxy`,
`packages/nexus-hub`, `packages/control-plane`, or `packages/control-plane-ui`), the
version or commit SHA, and reproduction steps. If encrypted communication is preferred,
request the PGP public key at the same address first.

Alternatively, use
[GitHub Private Vulnerability Reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository. Do not open a public GitHub issue for security vulnerabilities.

Initial acknowledgement target is within 3 business days; triage and severity assessment
within 7 business days. A coordinated disclosure window of 90 days (negotiable) is
offered after a fix is ready. Reporters who request credit are named in the advisory;
anonymity is honored on request. There is no paid bug-bounty program at this time. Full
scope, disclosure policy, and response timeline are in
[`SECURITY.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/SECURITY.md).

---

## Canonical docs

- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — product overview, value proposition, trinity consistency model
- [`docs/users/product/features.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/features.md) — full feature catalog with deployment-mode annotations and streaming compliance mode details
- [`docs/developers/roadmap.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/roadmap.md) — production state, active epics, queued work, precise definition of "production-validated"
- [`SECURITY.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/SECURITY.md) — vulnerability reporting, disclosure policy, supported versions, scope

**Adjacent wiki pages**: [FAQ Comparisons](FAQ-Comparisons) · [Glossary](Glossary) · [Roadmap Active](Roadmap-Active) · [Production State](Production-State) · [What Is Nexus Gateway](What-Is-Nexus-Gateway)
