# FAQ Comparisons

Nexus Gateway occupies a position in the AI traffic governance market that overlaps with several categories of tools — programmatic LLM gateways, network proxy/SASE products, and endpoint agents. This page answers the most common "how does Nexus compare to X?" questions. Each answer summarizes the key structural differences; the full head-to-head capability matrix with sources is in the [Comparisons](Comparisons) page.

The market splits into three quadrants: programmatic LLM gateway (SDK changes `base_url`), inline network/SASE interception (transparent proxy), and OS-level endpoint capture. Most vendors occupy one quadrant; Nexus Gateway covers all three from a single product.

---

## Programmatic LLM gateways

**Q: How does Nexus compare to LiteLLM?**

LiteLLM is the OSS de-facto standard for programmatic LLM routing: 100+ providers, OpenAI-compatible API, configurable auto-injection of Anthropic `cache_control` (the only other product to do this outside Nexus), and flexible routing/fallback. Both products are OSS-first and self-hosted.

The structural differences: LiteLLM has no transparent TLS-MITM capture of SaaS web AI tools (ChatGPT web, Claude web, Cursor IDE gRPC streaming), no OS-level Desktop Agent for off-corpnet enforcement, and no enterprise compliance pipeline (PII hooks, S3 spillstore full audit, kill switch, data-classification labels per transaction). Nexus's PII detector today covers fewer categories and languages (4 categories in English regex vs. LiteLLM's plugin options).

For teams that only need programmatic SDK routing with broad provider coverage, LiteLLM is lower-friction and has a larger active community. For organizations that need to also capture browser-based and desktop AI tool traffic from managed workstations, Nexus adds the Compliance Proxy and Desktop Agent paths on top of the same SDK-proxy surface. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to Portkey?**

Portkey is a SaaS + OSS LLM gateway with 1,600+ providers, conditional routing, circuit breakers, 50+ guardrails, a semantic cache, and a strong observability surface (logs, traces, metrics). Portkey offers a generous free tier (10K requests/month) and is widely adopted in the developer community.

The primary structural differences: Portkey has no transparent TLS-MITM capture layer and no OS-level endpoint agent. Traffic from browser-based AI tools (ChatGPT web, Claude web), native IDE plugins (Cursor gRPC), and devices off corporate network is outside Portkey's scope. Nexus is self-hosted/on-premises by design; Portkey is primarily cloud-SaaS with an OSS core. For cloud-native teams comfortable with SaaS and focused exclusively on SDK traffic, Portkey is a strong choice. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to Bifrost?**

Bifrost is an OSS OpenAI-compatible gateway notable for extremely low latency overhead (11 µs at 5K RPS) and governance built-in. Like other programmatic gateways, Bifrost requires applications to change `base_url` and does not capture SaaS web AI traffic or deploy an endpoint agent.

Nexus is higher-overhead per request (2 ms p95 on cache-miss at the gateway layer, validated in a 28,591-request benchmark) due to the compliance hook pipeline, cost stamping, and audit drain. If raw per-request latency is the primary constraint and compliance pipeline is not required, Bifrost's overhead is lower. If compliance hooks, multi-provider routing, full audit trails, and per-VK billing are required, Nexus provides these out of the box. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to Helicone?**

Helicone is an OSS + SaaS observability-first LLM gateway with response caching and simple routing. As of early 2026, Helicone is in maintenance mode following acquisition by Mintlify; the official migration path points to LiteLLM or Portkey. Helicone has no TLS-MITM capture, no endpoint agent, and no compliance hook pipeline. If an existing deployment uses Helicone, migrating to Nexus covers the same SDK-proxy surface plus the Compliance Proxy and Desktop Agent paths, with a full compliance hook pipeline and audit trail.

**Q: How does Nexus compare to Cloudflare AI Gateway?**

Cloudflare AI Gateway is a SaaS edge product with provider routing, retry/fallback, edge caching, and content guardrails. It does not perform transparent TLS interception of SaaS web AI tools and does not deploy a desktop endpoint agent. Traffic goes through Cloudflare's global network — suitable for teams already in the Cloudflare ecosystem that need only programmatic SDK traffic governance and can accept cloud-SaaS data routing.

Nexus is a self-hosted product covering all three traffic paths — AI Gateway (SDK), Compliance Proxy (TLS-MITM), and Desktop Agent (OS-level). It does not depend on Cloudflare infrastructure, and all data stays within the operator's perimeter. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to Kong AI Gateway?**

Kong AI Gateway has the deepest PII sanitizer among pure-gateway products: 20 categories across 12 languages, pgvector semantic cache, strong enterprise community, and a mature plugin ecosystem. Kong's scope is the programmatic API gateway quadrant — it does not capture SaaS web AI traffic, does not deploy an endpoint agent, and does not auto-inject Anthropic `cache_control`.

Nexus covers the same gateway quadrant plus the network proxy and endpoint quadrants from one product. The trade-off today: Kong has significantly broader PII detection coverage (20 categories × 12 languages vs. Nexus's 4 categories in English regex only). For organizations where multi-language PII is a hard procurement requirement today, Kong is stronger in that dimension. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to AWS Bedrock with Guardrails?**

AWS Bedrock is a SaaS managed inference service for AWS-hosted models. It does not expose an OpenAI-compatible API, and it cannot proxy traffic to non-AWS providers (OpenAI, Anthropic direct, Google Gemini direct). Bedrock Guardrails add PII detection and content filtering but only for traffic routed through Bedrock's own inference plane — direct API calls to openai.com or api.anthropic.com are outside its scope.

Nexus is provider-agnostic: it routes to any provider, captures traffic from any source (SDK, browser tool, desktop app), and runs the compliance pipeline on all three paths. It does not require AWS and does not lock traffic into one cloud provider. For organizations building on AWS exclusively and using only Bedrock-hosted models, native Bedrock Guardrails avoid an additional component. See [Comparisons](Comparisons) for the full matrix.

---

## Network and endpoint products

**Q: How does Nexus compare to a homegrown reverse proxy (nginx, Envoy, Caddy)?**

A homegrown reverse proxy can forward AI API calls and add TLS termination, but it does
not decode provider-specific wire formats (Anthropic's `messages` array structure,
Gemini's `contents[].parts[].text` nesting, Moonshot's `reasoning_content` field),
extract plain-text prompt content for audit, run a compliance hook pipeline, stamp cost
per request from a model price table, or integrate with a control plane for policy
configuration. These are non-trivial engineering problems — the provider codec alone
needs to handle streaming SSE deltas, function call serialization, vision inputs, and
reasoning token extraction for each provider's quirks.

Building compliance and audit on top of a generic proxy is a common starting point for
in-house AI platforms. The projects that persist beyond a prototype quickly discover they
have re-implemented a subset of what Nexus provides — at which point migrating to Nexus
avoids maintaining a non-differentiating infrastructure layer. Nexus is Apache 2.0 and
fully self-hostable. The homegrown route is justified only if the organization has
requirements that Nexus's architecture does not support or prefers full ownership of the
stack. See [Comparisons](Comparisons) for the full matrix.

**Q: How does Nexus compare to Palo Alto Prisma AIRS?**

Palo Alto Prisma AIRS is the only large enterprise security vendor publicly committed to
the same tri-modal coverage Nexus targets: runtime firewall (network intercept) + API
intercept + LLM gateway (via an in-progress Portkey integration). Of all competitors,
PANW AIRS has the closest strategic overlap.

The structural differences: PANW AIRS requires Prisma Access/SASE deployment for the
network leg — it is not self-hosted in the Nexus sense. The gateway leg (Portkey
integration) was still "in flight" as of the competitive landscape survey (May 2026).
Nexus's differentiators: IDE-protocol decoding (Cursor gRPC, Copilot streaming at the
protocol body level), off-corpnet endpoint capture (Desktop Agent continues enforcing
on home Wi-Fi, conference networks, without corporate SASE in the path), and
gateway-value combined with security (automatic Anthropic `cache_control` injection,
multi-provider routing, per-VK billing) in the same compliance pipeline. See
[Comparisons](Comparisons) for the full matrix.

---

## Structural position and differentiators

**Q: What is Nexus's structural claim to uniqueness?**

The competitive landscape document identifies four defensible structural positions:

1. **True tri-modal coverage in one product** — inline network proxy (Compliance
   Proxy) + OS-level endpoint capture (Desktop Agent) + programmatic OpenAI-compatible
   API (AI Gateway). Palo Alto AIRS is the only other vendor publicly heading this
   direction; they have not shipped the gateway leg yet.

2. **IDE-protocol decoding** — Cursor gRPC streaming and Copilot streaming are decoded
   at the protocol body level. Network-tier vendors inspect the SSL-layer byte stream
   but never recover the plain-text prompt. Nexus's Compliance Proxy Tier-1 normalizers
   decode these wire formats explicitly.

3. **Off-corpnet endpoint capture** — Aim Security, Prompt Security, and Lakera all
   depend on corporate network, browser extension, or SASE egress. The Desktop Agent
   enforces policy and audits on home Wi-Fi, coffee shop networks, and conference hotel
   Wi-Fi — wherever the managed device has connectivity to the Hub.

4. **Gateway value combined with security** — automatic Anthropic `cache_control`
   injection, multi-provider routing, per-VK billing, and S3 spillstore full audit are
   all in the same compliance pipeline. Pure-security vendors treat routing and caching
   as "the gateway vendor's job" (Portkey/LiteLLM territory). Pure-gateway vendors do
   not ship the compliance pipeline. Nexus covers both.

The documented gaps are: multi-language PII depth (Kong is stronger today), prompt-injection
detector depth (Zscaler ships 18+ detectors vs. Nexus's hook framework with shallower
detector coverage), and no MCP/agentic governance plane yet. See the full competitive
landscape for the current gap status.

---

## Canonical docs

- [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md) — full head-to-head capability matrix with sources, three-quadrant map, structural unique differentiators, and documented gaps
- [`docs/users/product/overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/overview.md) — Nexus product positioning, value proposition, and trinity consistency model

**Adjacent wiki pages**: [Comparisons](Comparisons) · [FAQ Product](FAQ-Product) · [Why Nexus](Why-Nexus) · [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Use Cases](Use-Cases)
