# What Is Nexus Gateway

Nexus Gateway is an enterprise AI traffic platform that governs, audits, and routes AI API calls across an organisation's entire surface area — SDK integrations, network-proxied applications, and developer endpoint tools. Unlike single-mode proxies that capture only one class of traffic, Nexus closes three structural coverage gaps simultaneously through three independent interception paths that share one compliance engine, one audit pipeline, and one control plane.

---

## The three coverage gaps

Enterprise AI adoption consistently exposes three blind spots that neither traditional network security nor a single SDK-layer proxy can close alone.

**Gap 1 — SDK calls bypass network controls.** Applications that directly instantiate an `openai.OpenAI()` client with a raw API key establish a TLS connection that bypasses corporate HTTP proxies entirely. SDK-layer traffic is invisible to Zscaler, Netskope, and any CASB that relies on HTTP CONNECT. A gateway at the SDK layer (Path A) closes this: applications point their `base_url` at the AI Gateway and present a virtual key instead of a raw provider credential.

**Gap 2 — Endpoint AI tools bypass both.** Cursor, Claude Code, GitHub Copilot, and browser-based AI chat interfaces run as native applications. They communicate over TLS direct to provider endpoints, without using the corporate HTTP proxy configuration, and without any application SDK the organisation controls. An OS-level desktop agent (Path C) intercepts at the network extension or packet-filter layer, below the application, before the traffic leaves the device.

**Gap 3 — Captured traffic lacks classification, routing, and audit.** Even when traffic does hit an organisation's proxy, most products inspect bytes and forward them — they produce metadata events or DLP hits, not a structured `traffic_event` record with resolved model, token counts, cost stamp, PII classification label, hook decisions, and a full prompt+response body in a spillstore. Nexus produces that record for every call, across all three paths.

---

## Three traffic paths

**Path A — AI Gateway (SDK proxy).** Applications send requests to `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, or `/v1/messages` using a virtual key. The gateway authenticates the key, runs the request-stage hook pipeline, resolves the target provider via declarative routing rules, decrypts the credential in memory, proxies upstream, runs the response-stage pipeline, and emits a structured `traffic_event` audit record. This path is the correct choice for application teams who actively integrate AI APIs — zero code change beyond pointing at the gateway and swapping the credential.

**Path B — Compliance Proxy (transparent TLS intercept).** Any HTTPS-speaking application configures the Compliance Proxy as its HTTP proxy. The proxy handles CONNECT tunneling, performs TLS bump (issuing dynamic certificates from an admin-trusted CA), extracts the canonical prompt and completion text from provider-specific wire formats, runs the same hook pipeline as the AI Gateway, and emits the same `traffic_event` record. This path requires no application code change — only an HTTP proxy setting. It covers SaaS web UIs (ChatGPT, Claude.ai, Gemini), REST clients, and any SDK that honours system proxy settings.

**Path C — Desktop Agent (OS-level intercept).** A Go binary installed on developer workstations (macOS, Windows, Linux) intercepts outbound AI traffic at the operating-system level using `NETransparentProxyProvider` on macOS, `pf` on Linux, and WinDivert on Windows. The agent enforces compliance locally with Hub-pushed hook configurations, captures full body content, and uploads audit events to the Hub over mTLS. This path covers applications that ignore proxy settings, private network connections, and off-corpnet scenarios where endpoint devices are not behind the corporate gateway.

All three paths share the same Go compliance hook implementations from `packages/shared`, the same data classification taxonomy (Public / Internal / Confidential / Restricted), and the same audit destination — so a policy configured once in the admin dashboard applies regardless of which path the traffic arrives through.

---

## What Nexus is not

Nexus Gateway is not a model API service — it does not host or run inference. Every request is proxied to a real upstream provider (OpenAI, Anthropic, Google, Azure, DeepSeek, or any of 47+ configured adapters). Nexus owns the policy enforcement, routing logic, credential management, and audit trail; the inference compute stays with the provider.

Nexus is not a pure observability tool. Langfuse, Helicone, and similar products record traces after the fact from SDK hooks or sidecars. Nexus is in the critical path of every request — it can block, redact, or reroute traffic, not just observe it.

Nexus is not a SASE or SSE platform. Unlike Zscaler or Netskope, it does not route all outbound traffic through a cloud point of presence. The Compliance Proxy runs on-premises or in the customer's own infrastructure; the Desktop Agent operates fully offline with Hub-pushed policy snapshots.

---

## Capability summary

The capability surface that applies to all three paths includes:

- Compliance hook pipeline — PII detection, keyword filtering, content safety, rate limiting, IP access filtering, request-size validation, webhook forwarding
- Data classification — Public / Internal / Confidential / Restricted label on every `traffic_event` row
- Body capture — prompt and response text stored inline (≤256 KiB) or in a pluggable spillstore (S3 / local FS) for larger payloads
- Full audit trail — every interaction recorded with hook decisions, routing trace, latency, token counts, cost stamp, and classification

Additional capabilities on the AI Gateway path include multi-provider routing, virtual key authentication, response cache, prompt cache, quota enforcement, and smart routing (LLM-dispatch).

---

## Canonical docs

- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — canonical product overview, value proposition, capability list
- [`docs/users/product/features.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/features.md) — per-feature detail for all capability areas
- [`docs/developers/architecture/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — system architecture, five-service split, storage layer

**Adjacent wiki pages**: [Why Nexus](Why-Nexus) · [Use Cases](Use-Cases) · [Three Traffic Paths](Three-Traffic-Paths) · [Architecture Overview](Architecture-Overview) · [Comparisons](Comparisons)
