# Why Nexus

Nexus Gateway exists because the gap between an organisation having an AI policy and that policy actually applying to every AI call in the environment is wide and structural — not a configuration problem. Simpler proxies close part of it; none close all of it. This page explains the gap, what Nexus does to address it, and what Nexus explicitly does not do.

---

## The gap simpler proxies leave open

### SDK gateways alone leave network-invisible traffic ungoverned

Products like LiteLLM, Portkey, Braintrust, and Vercel AI Gateway solve the application-integration case well: teams that actively adopt AI add one `base_url` change and gain routing, caching, and logging. The gap is traffic that never reaches the gateway at all.

Cursor, Claude Code, GitHub Copilot, and every SaaS AI web UI (ChatGPT, Claude.ai, Gemini) are applications the developer runs directly — not SDKs a platform team controls. These tools open TLS connections without using the organisation's HTTP proxy configuration. A pure SDK gateway is structurally invisible to them.

### Network proxies and CASB alone cannot decode protocol bodies

Products like Zscaler, Netskope, and WitnessAI intercept at the TLS-bump layer. They see bytes flowing to `api.openai.com` and `api.anthropic.com`. What they cannot do is decode the application-layer protocol inside that TLS stream — the Cursor IDE speaks gRPC over HTTPS, GitHub Copilot uses a streaming HTTP/2 protocol, and Claude Code's completions API follows the Anthropic wire format. Network-tier products produce metadata events (domain, size, classification label from a static ruleset) but cannot extract the prompt text or the completion text from those protocols. Without prompt text, PII detection is impossible, prompt-injection detection is impossible, and the audit record contains "AI call to Anthropic" rather than the actual conversation.

Nexus's Compliance Proxy uses per-host Tier-1 adapters (`chatgpt.com`, `claude.ai`, `api.openai.com`, `api.anthropic.com`, Cursor IDE) that decode the wire format at the application layer, extract canonical prompt+completion text, and run the full hook pipeline against that text. The network-tier blob never reaches a compliance engine in competing products; Nexus extracts from it first.

### Off-corpnet endpoints have no coverage without an agent

Developer laptops on home Wi-Fi, conference networks, and VPN-off scenarios are outside the reach of corporate SWG / SASE infrastructure. A compliance posture that collapses when a developer works remotely is not a compliance posture. Nexus's Desktop Agent runs on the endpoint itself — it pulls policy from the Hub on boot and continues to enforce locally using a cached snapshot when the Hub is unreachable. AI calls made from a coffee shop go through the same hook pipeline as calls made on the corporate LAN.

---

## What Nexus closes

The three paths — AI Gateway + Compliance Proxy + Desktop Agent — are intentionally independent. Each closes one of the three gaps, and together they form a complete coverage boundary:

| Path | What it closes | Requires |
|---|---|---|
| AI Gateway | SDK and programmatic API calls | `base_url` change + virtual key |
| Compliance Proxy | HTTPS-speaking apps, SaaS web UIs, SDK clients that honour proxy settings | HTTP proxy configuration |
| Desktop Agent | OS-level endpoint capture — apps that ignore proxy settings, native tools, off-corpnet | OS-level install + enrollment |

The paths are not redundant — each has traffic the others miss. Running all three means no LLM traffic escapes governance. The Agent's E60 attestation header prevents double-processing in the case where Agent-captured traffic happens to route through the Compliance Proxy.

---

## The 5-service rationale

A simpler design would be one process. Nexus uses five services for specific operational reasons:

**Nexus Hub** centralises the Thing registry, configuration shadow, and audit sink. If it were merged into the Control Plane, a Control Plane restart would disrupt agent heartbeats, ongoing config-sync in-flight operations, and the real-time metrics pipeline. Hub is a pure operations center with no user-facing request traffic — its availability profile differs from the API services.

**Control Plane** is the admin API and BFF for the dashboard. It is deliberately stateless (proxies config writes to Hub, reads config from Hub). Statelessness enables horizontal scaling and safe restarts without traffic disruption.

**AI Gateway**, **Compliance Proxy**, and **Agent** are the three data-plane services. They are kept separate because they have fundamentally different runtime profiles: the AI Gateway is a high-throughput async HTTP server; the Compliance Proxy is a TLS-bumping CONNECT proxy; the Agent is an OS-extension process with platform-specific signing requirements. Merging them would create a monolith where a compliance-proxy panic takes down the AI Gateway, or where a macOS NE signing constraint applies to the server binary.

All five register with Hub as Things and pull configuration via the same `thingclient` WebSocket/HTTP mechanism. This uniformity means the same drift-detection, config-sync, and health-reporting infrastructure works for every service in the fleet.

---

## Non-goals

**Nexus is not a model router only.** Pure LLM routers (Martian, LM-Routing) solve prompt→model matching. Nexus does routing too, but routing is one capability among many — not the product. An organisation should not adopt Nexus as a routing shim and ignore the compliance pipeline.

**Nexus is not a pure SDK wrapper.** The SDK gateway path (AI Gateway) is Path A of three. Evaluating Nexus as "an alternative to LiteLLM" misses the compliance-proxy and agent legs, which are what differentiate it from every other gateway product on the market.

**Nexus is not a SASE cloud.** The Compliance Proxy and Desktop Agent run in the customer's own infrastructure. Nexus does not route traffic through a third-party cloud point of presence, does not require a network architecture change, and does not add latency for traffic not routed through its data-plane services.

**Nexus is not a model hosting service.** Inference compute stays with the upstream provider (OpenAI, Anthropic, Google, Azure, and the 47+ adapter targets). Nexus adds ≤2 ms gateway-only latency on a cache-miss request against a provider with an 11-second response time — measured at production scale with 28,591 requests through `gpt-4o` (see [`README.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/README.md) performance section).

---

## Canonical docs

- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — canonical product overview and value proposition
- [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md) — full competitive analysis with per-vendor breakdown
- [`docs/developers/architecture/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — system architecture, control vs data plane, five-service split

**Adjacent wiki pages**: [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Use Cases](Use-Cases) · [Comparisons](Comparisons) · [Three Traffic Paths](Three-Traffic-Paths) · [The Five Services](The-Five-Services)
