# Comparisons

This page compares Nexus Gateway against the tools most commonly evaluated alongside it. Each tool receives a capability matrix row and a "when to prefer" paragraph. Claims are sourced from [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md), which carries per-vendor citations and is refreshed quarterly.

---

## Capability matrix

The columns below cover the capabilities that most commonly differentiate products in this space. "Tri-modal" means the product has all three coverage paths: programmatic LLM gateway + transparent network proxy + endpoint/OS-level agent.

| Tool | Tri-modal | TLS-MITM + protocol decode | Off-corpnet endpoint | Auto `cache_control` inject | Multi-provider routing | Full prompt+response audit | Built-in PII pipeline | Open source |
|---|---|---|---|---|---|---|---|---|
| **Nexus Gateway** | ✅ | ✅ (Tier-1: Cursor gRPC, ChatGPT, Claude.ai, Gemini) | ✅ (NE / pf / WinDivert) | ✅ (Anthropic) | ✅ (47+ providers) | ✅ (S3 spillstore) | ✅ (4 categories) | ✅ Apache 2.0 |
| **LiteLLM** | ❌ | ❌ | ❌ | ✅ (configurable) | ✅ (100+ providers) | ✅ (self-host DB) | Basic | ✅ MIT |
| **Portkey** | ❌ | ❌ | ❌ | ❌ | ✅ (1600+ providers) | ✅ | 50+ guardrails (plugin) | ✅ core |
| **Bifrost (Maxim)** | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ | Governance built-in | ✅ OSS |
| **Helicone** | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ | Weak | ✅ (maintenance mode) |
| **Cloudflare AI Gateway** | ❌ | ❌ (Cloudflare edge CASB is separate) | ❌ | ❌ | ✅ (retry + fallback) | ✅ | Guardrails (content / violence) | ❌ SaaS |
| **AWS Bedrock + Guardrails** | ❌ | ❌ | ❌ | N/A (native) | ❌ (Bedrock-hosted only) | CloudWatch | PII + content filters | ❌ AWS-locked |

**Notes on checkmarks:**
- "Auto `cache_control` inject" means the gateway automatically inserts Anthropic prompt-cache breakpoints without client changes. LiteLLM offers this as a configurable; OpenRouter passes through client-supplied breakpoints but does not inject.
- "Full prompt+response audit" means the raw prompt and completion text are retained (not just metadata), with a storage backend for large payloads.
- Helicone is in maintenance mode following acquisition by Mintlify (March 2026); official migration path to LiteLLM and Portkey.

---

## LiteLLM

LiteLLM is the de-facto open-source standard for the programmatic LLM gateway space. It covers 100+ providers, has an active contributor community, auto-injects Anthropic `cache_control`, and runs as a self-hosted proxy or a hosted cloud service. The SDK compatibility surface is broad and well-tested.

**When to prefer LiteLLM over Nexus.** LiteLLM is the right choice for a team that needs a mature, well-documented programmatic gateway with wide provider coverage and a large plugin/integration ecosystem, and whose AI traffic comes entirely from code that can be pointed at a `base_url`. If the organisation has no endpoint AI tool governance requirement, no need for TLS-MITM network-level capture, and no compliance hook pipeline beyond basic logging, LiteLLM is simpler to operate.

**When to prefer Nexus over LiteLLM.** Nexus closes the coverage gaps that LiteLLM is architecturally unable to close: endpoint tool traffic (Cursor, Copilot, Claude Code) bypasses any SDK gateway including LiteLLM; network-intercepted SaaS web AI calls are invisible to it; and there is no endpoint agent for off-corpnet enforcement. Nexus also ships a built-in compliance hook pipeline (PII detection, data classification, hook-decision audit trail) as a first-class capability, whereas LiteLLM's compliance story depends on external plugins.

---

## Portkey

Portkey is a production-grade gateway with 1600+ provider integrations, 50+ guardrail plugins, strong routing and fallback logic, and a polished hosted SaaS offering. It is actively developed and positions itself as an enterprise AI gateway with multi-cloud LLM support.

**When to prefer Portkey over Nexus.** Portkey is a strong choice for teams that want an immediately available SaaS gateway with minimal infrastructure overhead, access to 1600+ providers without running their own adapter code, and out-of-the-box integration with many compliance and observability platforms via the guardrail plugin marketplace. Teams that have no requirement for transparent network capture or endpoint-level governance will find Portkey faster to start with.

**When to prefer Nexus over Portkey.** Nexus provides the transparent TLS-MITM Compliance Proxy path (zero code change, covers SaaS AI web UIs) and the Desktop Agent (OS-level enforcement for developer tools, off-corpnet). Portkey has no equivalent to either. Nexus's audit trail includes full prompt+response body storage in an S3-backed spillstore; Portkey's audit is log-level. For organisations that need the same compliance pipeline to cover SDK calls, SaaS AI, and endpoint tools from a single admin dashboard, Nexus is the only self-hostable option that does all three.

---

## Bifrost (Maxim)

Bifrost is an open-source LLM gateway that benchmarks well on raw overhead (11 µs p50 at 5K RPS), supports mainstream providers, and includes governance features. It is a newer entrant and less established than LiteLLM or Portkey.

**When to prefer Bifrost over Nexus.** Bifrost is worth evaluating for teams where gateway-only latency overhead is a primary constraint and the compliance pipeline requirements are minimal. At 11 µs per request it has a materially lower baseline than most gateways.

**When to prefer Nexus over Bifrost.** Bifrost is a pure programmatic gateway. It has no TLS-MITM proxy, no desktop agent, no endpoint capture, and no compliance pipeline comparable to Nexus's hook infrastructure. For any use case beyond cost/routing optimisation, Nexus provides more capability.

---

## Helicone

Helicone pioneered the "one-line SDK proxy" model for LLM observability. It provides good trace visualisation and dataset management. As of March 2026 Helicone is in maintenance mode following acquisition by Mintlify; the official migration path is to LiteLLM or Portkey.

**When to prefer Nexus over Helicone.** Helicone is maintenance-mode; new feature development has stopped. For any production AI governance use case, the migration path away from Helicone is the right choice, and Nexus is an option if the tri-modal coverage or compliance pipeline is relevant.

---

## Cloudflare AI Gateway

Cloudflare AI Gateway is a hosted SaaS gateway at the Cloudflare edge, supporting mainstream providers, automatic response caching, retry + fallback routing, and content guardrails (violence, hate speech, etc.). It requires no infrastructure to operate and benefits from Cloudflare's global network for latency reduction.

**When to prefer Cloudflare AI Gateway over Nexus.** Cloudflare AI Gateway is a strong choice for public-facing applications where Cloudflare is already the CDN/edge provider, where latency matters more than compliance depth, and where the team has no self-hosting requirement. The Cloudflare operator model (no infrastructure) is compelling for smaller teams.

**When to prefer Nexus over Cloudflare AI Gateway.** Cloudflare AI Gateway has no TLS-MITM proxy, no desktop agent, no off-corpnet enforcement, and no full prompt+response audit storage — it is a performance and availability gateway, not a compliance gateway. Guardrails are content-category based (not PII classification against custom policy). Data residency requirements or air-gapped deployment requirements cannot be met by a SaaS product. For any organisation that needs governance over developer endpoint tools or needs the full audit record for compliance purposes, Cloudflare AI Gateway is not in scope.

---

## AWS Bedrock + Guardrails

AWS Bedrock is a managed AI inference service. Bedrock Guardrails adds PII detection ($0.10/1K units), content filtering, and topic denial policies as a sidecar to Bedrock-hosted model calls. It is tightly integrated with the AWS ecosystem (CloudWatch, IAM, CloudTrail).

**When to prefer AWS Bedrock over Nexus.** If the organisation is fully committed to AWS and all AI workloads will use Bedrock-hosted models, Bedrock + Guardrails is the path of least resistance. The IAM integration with AWS roles, native CloudTrail audit, and SLA-backed managed service are meaningful advantages for AWS-native organisations.

**When to prefer Nexus over AWS Bedrock.** Bedrock's API is not OpenAI-compatible — applications must be rewritten to use the Bedrock SDK, or use a translation layer. Bedrock covers only models hosted on Bedrock (Anthropic, Cohere, Meta, AI21, Stability); direct OpenAI, Gemini, or Moonshot calls are outside its scope. There is no Compliance Proxy path (SaaS AI web UIs, Cursor, Copilot are invisible to Bedrock). There is no Desktop Agent. Bedrock Guardrails does not produce a full prompt+response body audit trail in customer-controlled storage — it logs decisions to CloudWatch. For multi-provider deployments, cross-cloud deployments, or endpoint governance requirements, Nexus is the appropriate choice.

---

## Structural differentiators

The competitive landscape document identifies four structural differentiators that no current competitor fully replicates:

1. **True tri-modal coverage in one product** — LLM gateway + transparent MITM proxy + endpoint agent, all sharing one compliance engine and one audit destination. Palo Alto Prisma AIRS is the only other vendor publicly targeting all three legs, and as of May 2026 has not delivered the programmatic gateway leg.

2. **IDE-protocol decoding** — Cursor's gRPC protocol and Copilot's streaming HTTP/2 are decoded at the application layer by Tier-1 adapters. Network-tier vendors (WitnessAI, Aim Security) intercept TLS bytes but cannot extract prompt text from these protocols.

3. **Off-corpnet endpoint capture** — The Desktop Agent continues to enforce policy and audit on home Wi-Fi and conference networks. Products that depend on corporate SWG/SASE infrastructure lose coverage the moment a developer steps off the corporate LAN.

4. **Security and gateway value combined** — Automatic Anthropic `cache_control` injection, multi-provider routing, per-VK billing, and S3 spillstore full audit are all in the same compliance pipeline. Pure-security vendors (Zscaler, WitnessAI) do not ship gateway value. Pure-gateway vendors (LiteLLM, Portkey) do not ship an enterprise compliance pipeline.

For identified gap areas (multi-language PII depth, prompt-injection detector depth, MCP/agentic governance plane, compliance certifications), see [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md) §"Where Nexus has structural gaps".

---

## Canonical docs

- [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md) — full competitive analysis, per-vendor citations, market positioning, structural gaps

**Adjacent wiki pages**: [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Why Nexus](Why-Nexus) · [Use Cases](Use-Cases) · [Production State](Production-State) · [Features Index](Features-Index)
