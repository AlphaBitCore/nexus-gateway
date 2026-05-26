# Competitive Landscape — AI Traffic Governance, 2026

*Prepared for sales / executive briefing. Snapshot as of May 2026. Refresh quarterly.*

---

## TL;DR

The "AI traffic governance" market in 2026 splits into three quadrants: **inline network/SASE inspection**, **endpoint capture**, and **programmatic LLM gateway**. Almost every vendor occupies one or two; **only Palo Alto Prisma AIRS (with its in-flight Portkey integration) and Nexus Gateway target all three at once**. Nexus's defensible edges are **IDE-protocol decoding** (Cursor gRPC, Copilot streaming), **off-corpnet endpoint capture** (macOS NE, Windows WinDivert, Linux iptables), and **gateway-and-security-as-one-product** (auto Anthropic `cache_control` injection + multi-provider routing + per-VK billing + S3 spillstore audit, in the same compliance pipeline).

The 12-18 month window matters: 2025 was a consolidation year (Cisco bought Lakera and Robust Intelligence; Palo Alto bought Protect AI for $500M+; SentinelOne bought Prompt Security for ~$250M; Cato bought Aim Security; F5 bought CalypsoAI for $180M). Standalone AI security pure-plays are running out of independent runway, while large security vendors are bolting AI features onto SWG/SASE platforms. Programmatic LLM gateway players (Kong, LiteLLM, Lunar.dev) are simultaneously adding security capabilities. The two camps are converging — Nexus already sits at the convergence point.

---

## The three-quadrant map

```
                  ┌─────────────────────────────────────┐
                  │  Programmatic LLM Gateway           │
                  │  (Nexus = AI Gateway leg)           │
                  │                                     │
                  │  Kong AI / LiteLLM / Portkey        │
                  │  Cloudflare AI GW / Lunar.dev       │
                  │  Cisco AI Defense Gateway           │
                  │  PANW Prisma AIRS + Portkey (WIP)   │
                  └─────────────────────────────────────┘
                        ↑                       ↑
                        │                       │
┌──────────────────────────┐         ┌──────────────────────────┐
│ Inline network / SASE    │         │  Endpoint capture        │
│ (Nexus = Compliance      │         │  (Nexus = Desktop Agent) │
│  Proxy)                  │         │                          │
│                          │         │  Prompt Security (browser│
│  Zscaler AI Security     │         │     extension via MDM)   │
│  Netskope SkopeAI        │         │  Polymer (SaaS connectors│
│  PANW Prisma Access      │         │  WitnessAI ("zero-touch")│
│  Cisco Secure Access     │         │  Aim Security (via Cato) │
│  Microsoft Purview       │         │                          │
│  Forcepoint ONE AI       │         │  ⚠️ True endpoint NE/    │
│  Symantec / Broadcom     │         │     iptables capture is  │
│  WitnessAI               │         │     largely unoccupied   │
└──────────────────────────┘         └──────────────────────────┘
```

---

## Top 7 competitors to watch (ranked by overlap pressure)

| # | Vendor | Quadrant | Why we track them | Where we beat them |
|---|---|---|---|---|
| 1 | **Palo Alto Prisma AIRS** | Big-vendor full-stack | Only large vendor publicly committed to runtime firewall + API intercept + LLM gateway (Portkey) — closest to Nexus's tri-modal model | No IDE protocol decoding (Cursor gRPC, Copilot streaming); requires PANW cloud + Prisma SASE for self-hosted / data-residency customers |
| 2 | **Kong AI Gateway** | Pure LLM gateway | Most complete gateway: multi-provider routing + multi-language PII Sanitizer (20 categories × 12 languages) + pgvector semantic cache | Doesn't capture SaaS web AI; doesn't capture endpoint traffic; no automatic Anthropic `cache_control` injection |
| 3 | **WitnessAI** | Network + endpoint | "Observe / Control / Protect" three-module split + claims "zero-install" coverage of native desktop apps — closest analog to our Compliance Proxy positioning | Routes via SASE NetFlow, **cannot MITM IDE protocol bodies**; no gateway-side routing / cache / VK value |
| 4 | **LiteLLM (BerriAI)** | OSS gateway standard | OSS de-facto standard; 100+ providers; **only other product that auto-injects Anthropic `cache_control`**; Helicone's official migration target post-acquisition | Pure programmatic gateway, no SaaS web / IDE traffic capture; no endpoint agent; no enterprise compliance pipeline (PII hooks + S3 spillstore) |
| 5 | **Aim Security** (now Cato Networks) | Endpoint + SASE | The only AI-security vendor explicitly naming Cursor + MCP + Copilot coverage; channel reach exploded post-acquisition | Traffic source limited to Cato SASE — **off-corpnet endpoints are a structural blind spot** that our Desktop Agent fills |
| 6 | **Zscaler AI Security Suite** | Big-vendor network | 18+ detectors in AI Guard (prompt injection / jailbreak / PII / secrets / toxicity / brand reputation); dual-mode (Proxy + DAS API); NVIDIA NeMo Guardrails integration | No LLM gateway, no IDE protocol decoding, no prompt cache / routing / per-VK billing, mandatory traffic detour through Zscaler cloud |
| 7 | **Lunar.dev** | Gateway + governance | Closest in messaging to "AI traffic governance control plane"; emphasises Egress + OWASP LLM + self-host + MCP gateway | Still requires apps to change `base_url` (not transparent TLS MITM); no endpoint agent |

---

## Big SSE / SASE / CASB vendors — feature matrix

| Vendor | 2026 product name | UI MITM | API path | IDE decode | Full audit storage | LLM gateway | Architecture | Verdict |
|---|---|---|---|---|---|---|---|---|
| **Netskope** | Netskope One AI Security / SkopeAI / AI Guardrails / Agentic Broker | Yes (SWG inline) | Partial (domain + DLP, no SDK decode) | No | Metadata + DLP hits, not full prompt+response | No (MCP broker only) | Cloud SWG + endpoint client | Partial overlap |
| **Palo Alto Networks** | Prisma AIRS 3.0 (Runtime Firewall + API Intercept + Model Sec + Red Teaming); Portkey AI Gateway integration in flight | Yes (Prisma Access) | Yes (API Intercept; app-initiated) | No | Yes (runtime logs) | In progress (Portkey) | Cloud SASE + API sidecar | **Full overlap** |
| **Cisco** | AI Defense (Runtime + Inspection API + AI Defense Gateway); DefenseClaw for agentic | Partial (Secure Access SWG) | Yes (Inspection API + Gateway mode) | No | Yes (runtime + audit) | Yes (AI Defense Gateway) | Cloud SASE + API + optional gateway | Partial overlap |
| **Microsoft** | Purview AI Hub + Defender for Cloud Apps GenAI + Entra Internet Access + Copilot governance | Yes (DCA + Edge for Business inline DLP) | Copilot/M365 native only; third-party via browser/network | No | Copilot native = full; third-party = metadata only | No | Cloud CASB + browser + Entra IA | Not really (M365-bounded) |
| **Forcepoint** | Forcepoint ONE AI (SSE + Data Security + DSPM); ARIA Assistant | Yes (SSE inline) | ChatGPT Enterprise Compliance API only (out-of-band pull) | No | ChatGPT Ent. = yes; others = metadata | No | Cloud SSE + endpoint | Partial (ChatGPT-Ent only) |
| **Proofpoint** | DLP Transform for GenAI + browser extension + Secure Agent Gateway (2026 phased) | Via browser ext + email/endpoint | Not directly proxied | No | Behaviour metadata + DLP hits | Secure Agent Gateway (agentic-only) | Endpoint + email + browser | Not really (no inline proxy) |
| **Symantec / Broadcom** | Cloud SWG (Express) + CloudSOC GenAI gatelet + DLP | Yes (Cloud SWG) | Domain / category block, no SDK decode | No | DLP hits + metadata | No | Cloud SWG + CASB | Not really (category-level only) |
| **Zscaler** | Zscaler AI Security Suite (AI Guard + AI Asset Mgmt + Secure Access to AI + Secure AI Infra) | Yes (ZIA SSL inspect) | DAS/API mode — **judges only, doesn't forward** | No | Metadata + DLP incidents | No (AI Guard ≠ LLM gateway) | Cloud SWG + Client Connector | Partial overlap |

**Vendor-specific notes:**

- **Netskope** is strong on app discovery, DLP, DSPM, MCP brokering. Prompt-injection detection is a new module under AI Guardrails, but no public IDE-protocol decoding and no programmatic LLM gateway.
- **Palo Alto AIRS** is the only big-vendor product publicly aiming for the same three legs we cover. Coverage of consumer-grade SaaS web UIs via MITM is not deeply documented; AIRS Runtime API requires application changes.
- **Cisco AI Defense** Inspection API is "app calls security" mode rather than transparent proxy — unmodified SaaS UIs are not in scope. DefenseClaw is agentic/MCP-focused, decoupled from desktop AI clients.
- **Microsoft Purview** delivers full audit only inside Copilot / M365. ChatGPT, Claude, Gemini, Cursor traffic falls back to Defender for Cloud Apps' browser/network DLP, with no IDE protocol or SDK direct-call visibility, and tight M365 ecosystem lock-in.
- **Forcepoint** is the only vendor pulling ChatGPT Enterprise data via OpenAI's Compliance API (out-of-band, not inline) — but only for ChatGPT Enterprise SKU.
- **Proofpoint**'s strengths are endpoint + email + browser DLP; no inline network proxy means programmatic SDK traffic is invisible. Secure Agent Gateway is agentic-only and rolling out in phased GA through 2026.

---

## Pure-play AI security startups — current state

| Vendor | Status | Architecture | Captures | Detection | Gateway? |
|---|---|---|---|---|---|
| **Lakera** | Acquired by **Cisco** (May 2025) | `/v2/guard` API + SDK | Only your app's calls | Injection / jailbreak / PII / harmful (100+ languages) | No |
| **Protect AI** | Acquired by **Palo Alto** (Jul 2025, $500M+) | Model scanner + Guardian Runtime + Recon red-team; folded into Prisma AIRS | Model artefacts + your app API | Supply chain / 35+ format backdoors / runtime | No |
| **WitnessAI** | Independent | Network-layer SWG-style + "zero-install" 2.0 | Outbound SaaS AI (incl. Copilot/M365) + IDE | Observe / Control / Protect | No |
| **Prompt Security** | Acquired by **SentinelOne** (Aug 2025, ~$250M) | Browser extension via MDM + API + self-host | Browser-DOM SaaS GenAI (250+ models) | Injection / jailbreak / PII / shadow AI | No |
| **Aim Security** | Acquired by **Cato Networks** (Sep 2025) | Traffic-level engine wired into Cato SASE | SASE egress + Copilot / Cursor + MCP | Shadow AI / AI-SPM / agentic | No |
| **Lasso Security** | Independent | Pick one of: Gateway / SDK / browser ext | Depends on form factor | Injection / output harms / identity isolation | Gateway form is a basic LLM gateway |
| **Polymer** | Independent | SaaS connectors (Slack/Drive/GitHub) + ChatGPT module | SaaS data plane + ChatGPT | DLP / PII / PHI / source code | No |
| **Robust Intelligence** | Acquired by **Cisco** (Oct 2024); now Cisco AI Defense | Discover/Detect/Protect platform + AI Firewall | App-layer through AI Firewall | Red team + runtime guardrails | No |
| **HiddenLayer** | Independent | AISec 2.0 + MLDR; **model-agnostic, agentless, doesn't touch prompts** | Model I/O metadata only | Model reverse-engineering / adversarial inputs / AIBOM | No |
| **Cranium** | Independent | Code repo + ecosystem discovery (AI exposure mgmt) | Static assets / code only — no runtime traffic | AI asset inventory / exposure surface | No |
| **CalypsoAI** | Acquired by **F5** (Sep 2025, $180M) | Cloud-native guardrails + red team; merged into F5 ADC/WAF | Requests through F5 reverse proxy / gateway | Injection / jailbreak / compliance | No (delegated to F5 gateway) |
| **Skyflow** | Independent | Privacy Vault API/SDK + MCP Gateway reverse proxy | Field-level data through the vault | PII / PHI tokenisation | No |
| **Patronus AI** | Independent | Eval API + Python/TS SDK | Only at the SDK call site | Hallucination (Lynx) / PII / toxicity / RAG eval | No |
| **Knostic** | Independent | Out-of-band API on Copilot / Glean / Slack AI | M365 / Glean inference egress | Need-to-know RBAC, evaluated at inference | No |

**Archetypal patterns:**

- Three dominant form factors: **(a) in-app SDK / API judging** (Lakera, Patronus, Skyflow, Lasso-SDK) — protects only "your own" AI apps, blind to ChatGPT-web / Cursor / Copilot; **(b) MDM-pushed browser extension** (Prompt Security, Lasso-Ext) — sees SaaS web UIs but blind to native desktop apps, IDE gRPC, and SDK calls; **(c) network/SASE reverse proxy** (WitnessAI, Aim+Cato, CalypsoAI+F5) — sees outbound HTTPS but requires SWG/SASE deployment buy-in.
- **Almost no pure-security vendor ships LLM-gateway routing + caching + billing.** They treat that as "the gateway vendor's job" (Portkey / Helicone / TrueFoundry / Kong territory). Nexus's "security + gateway value combined" is structurally rare in this camp.
- **2025 was the consolidation year**: 5 acquisitions in 5 months (Cisco-Lakera, PANW-Protect AI, SentinelOne-Prompt Security, Cato-Aim, F5-CalypsoAI). The remaining standalone window is closing — likely buyers from here are Zscaler / Netskope / Cloudflare.
- **Desktop / IDE traffic (Cursor gRPC/SSE, Copilot) is an industry-wide blind spot.** WitnessAI and Aim claim native-desktop coverage, but both go through SASE-egress NetFlow — they cannot MITM and decode the protocol body. Nexus's Compliance Proxy + Desktop Agent dual stack is the only architecture that gets to plain-text IDE protocol payloads.
- **"Full prompt + response S3 audit storage"** is a minority capability. Most vendors persist "events + metadata" or truncated snippets. Polymer / WitnessAI / Prompt Security come closest, but typically forward to SIEM rather than offer native tiered cold storage.
- **HiddenLayer / Cranium / Patronus aren't in our lane** — HiddenLayer is model reverse engineering and MLDR (MLOps side), Cranium is ASM-style static scanning, Patronus is eval-as-a-service.
- YC W25/S25 batches are 60%+ AI-tagged, but **none ship "full-stack tri-modal AI traffic governance"**; Asteroid (W25, agent supervision) and Confident AI (W25, eval/red-team) are adjacent.

---

## LLM gateway / AI proxy direct competitors — capability matrix

| Tool | OSS / SaaS | API surface | Providers | Response cache | `cache_control` auto-inject | Routing / failover | Built-in security | Full audit | Pricing |
|---|---|---|---|---|---|---|---|---|---|
| **Portkey** | OSS + SaaS | OpenAI-compat | 1600+ | Simple + semantic | No | Yes (conditional + fallback + circuit breaker) | 50+ Guardrails | Yes | Free 10K/mo → $49/mo |
| **LiteLLM** | OSS-first | OpenAI-compat | 100+ | Yes | **Yes** (auto-inject configurable) | Yes | Basic guardrails, mostly via plugins | Yes (self-host DB) | OSS free |
| **Helicone** | OSS + SaaS | OpenAI-compat | 100+ | Yes (>1024 token auto) | No | Yes | Weak (no native PII redline) | Yes | Acquired by Mintlify; **maintenance mode**, official migration to LiteLLM/Portkey |
| **Cloudflare AI Gateway** | SaaS | API only | Mainstream | Edge cache | No | Yes (retry + fallback) | Guardrails (content / violence etc.) | Yes | Per-request |
| **Kong AI Gateway** | OSS + Enterprise | API only | Mainstream | Semantic cache (pgvector) | No | Yes | **PII Sanitizer (20 categories, 12 languages)** | Plugin-based | OSS free / Enterprise |
| **AWS Bedrock + Guardrails** | SaaS (AWS-locked) | Bedrock API (not OpenAI-shaped) | Bedrock-hosted models only | Anthropic 1h prompt cache | N/A (native) | Cross-model limited | Guardrails $0.10/1K (PII), content filters | CloudWatch | Per-token + Guardrails priced separately |
| **Vercel AI Gateway** | SaaS | OpenAI-compat | OpenAI / Anthropic / Google / Mistral / Cohere | Auto | No | Yes | None | Basic | Pass-through with no markup, $5/mo free tier |
| **Lunar.dev** | SaaS (self-host clusters) | API + Egress proxy | Mainstream | Yes | No | Yes (cost / task aware) | PII redline, egress control, OWASP LLM | Yes | Enterprise |
| **OpenRouter** | SaaS | OpenAI-compat | 290+ | Sticky routing hits upstream cache | **Pass-through** Anthropic breakpoint | Yes (price / availability) | None | Weak | Pass-through + 5.5% |
| **Tetrate Envoy AI Gateway** | OSS | OpenAI-compat (Envoy) | OpenAI / Bedrock first | Semantic cache | No | Yes (token rate-limit + fallback) | Via Envoy plugins | OpenInference tracing | OSS / managed |
| **F5 AI Gateway** | Commercial | Proxy architecture | Anthropic / ChatGPT / Azure / Ollama | Yes | No | Yes | **Strong**: PII, prompt injection, model DoS, info leak | Yes | Enterprise |
| **Apigee + AI** | Google Cloud SaaS | Generic API GW | Any LLM | Yes | No | Yes | OAuth / JWT / token rate-limit policies | Yes | Apigee plans |
| **Glama Lightport** | OSS + SaaS | OpenAI-compat | 77+ providers | Built-in | No | Load balance + fallback | Weak | Yes | Pass-through |
| **Martian** | SaaS | OpenAI-compat | Many | No (router) | No | **Speciality**: realtime prompt → optimal model | None | Basic | Free 2.5K + 5.5% |
| **Braintrust Gateway** | SaaS (legacy proxy deprecated) | OpenAI-compat | OpenAI / Anthropic / Google / AWS | <100ms auto | No | Yes | Eval-first; weak security | Yes (traces become datasets) | SaaS |
| **Vellum** | SaaS | Workflow + API | Mainstream | Yes | No | Yes | Weak | Yes | SaaS |
| **Langfuse** | OSS-leading + SaaS | **Not a gateway** — OTel observability | Any SDK | N/A | No | N/A | Depends on upstream gateway | **Strong** (PG + ClickHouse) | OSS / SaaS |
| **Databricks Unity AI Gateway** | SaaS (Lakehouse-locked) | Single API | Any LLM + MCP | Yes | No | Yes | Guardrails | Yes | Databricks plan |
| **Bifrost (Maxim)** | OSS | OpenAI-compat | Mainstream | Yes | No | Yes (11 µs overhead @ 5K RPS) | Governance built-in | Yes | OSS / Enterprise |

**Structural patterns:**

- **Almost every competitor is a pure programmatic API gateway** (listening on `/v1/chat/completions`); apps must change `base_url`. **None do TLS-MITM capture of SaaS web AI** (ChatGPT web, Claude web, Cursor IDE protocol). The closest analogues are Cloudflare CASB and Netskope — but those scan the SaaS configuration side, not the traffic content plane.
- **Auto-inject Anthropic `cache_control`** is shipped only by **LiteLLM** as a configurable; OpenRouter passes through client-supplied breakpoints; everyone else requires the client to mark the cache boundary.
- The market splits into **observability-first** (Helicone / Langfuse / Braintrust) vs **governance-first** (Kong / F5 / Lunar / Portkey). Vendors that bundle native PII redline + full audit + multi-provider routing into one product are limited to Kong, F5, Lunar, Portkey.
- **Endpoint proxy capture (NE / WinDivert / iptables) is completely absent** from this category — no gateway player does endpoint egress interception. That space belongs to SSE/CASB vendors, but those don't expose an OpenAI-compatible gateway.
- **Two-pole pricing**: ultra-low-overhead OSS (Bifrost 11 µs, Envoy AI GW, LiteLLM) vs governance SaaS (Portkey, Cloudflare, Vercel). Pricing is mostly pass-through + small markup or subscription.
- **Consolidation in this lane too**: Helicone went into maintenance mode after the Mintlify acquisition (Mar 2026); Braintrust deprecated its legacy AI proxy in favour of the unified Gateway; Vertex AI Extensions retired from the API hub (Sep 2025).
- **EU AI Act** (full enforcement Aug 2026) and NIST AI RMF are pushing "full prompt + response audit" from nice-to-have to procurement-mandatory — favourable for us.
- **MCP / Agentic governance** is the new battleground (Lunar, Databricks Unity, Tetrate ARS, Glama). Nexus has no MCP plane today — flagged as a strategic gap below.

---

## Where Nexus is structurally unique (4 pillars)

1. **True tri-modal coverage in one product**: inline network proxy + endpoint NE/WinDivert/iptables + programmatic OpenAI-compatible API. Palo Alto AIRS is the only other vendor publicly heading this direction, and they have not delivered the gateway leg yet.
2. **IDE-protocol decoding**: Cursor gRPC Tier-1 normaliser; Copilot streaming protocol decoding. Network-tier vendors stay at SSL-inspect byte-stream level and never recover the prompt plain-text.
3. **Off-corpnet endpoint capture**: Aim, Prompt Security, Lakera all depend on corporate network, browser extension, or SASE deployment. The Nexus Desktop Agent continues to enforce policy and audit on home Wi-Fi, coffee shop, conference networks.
4. **Gateway value combined with security**: automatic Anthropic `cache_control` injection + multi-provider routing + per-VK billing + S3 spillstore full audit — all in the same compliance pipeline. Pure-security vendors don't ship this. Pure-gateway vendors don't ship the compliance pipeline.

---

## Where Nexus has structural gaps (4 areas to close)

1. **Prompt-injection / jailbreak detector depth**: Zscaler ships 18+ detectors; Lakera, Prompt Security, Aim have multi-year detector libraries. The Nexus hooks framework is in place but the "model-judging-model" inflight detector layer is shallow.
2. **Multi-language PII**: Kong AI ships 20 categories × 12 languages. Nexus today has 4 categories (email / phone / SSN / credit card) in English regex only.
3. **MCP / Agentic governance plane**: Lunar, Databricks Unity, Tetrate ARS, Glama have all carved out MCP positioning. Nexus has no MCP control plane yet.
4. **Compliance certifications & framework mappings**: the audit market is moving "full prompt + response audit" into procurement RFPs as a hard requirement. Our S3 spillstore gives us the data; we need the **SOC 2 + ISO 42001** certifications and explicit EU AI Act / NIST AI RMF control mappings to close enterprise procurement loops.

---

## Strategic verdict

> **The big vendors are coming toward us (Palo Alto fastest), the pure gateways are adding security (Kong fastest), and the pure security vendors are adding SaaS coverage (WitnessAI fastest). Our window is "the three legs already grew together" plus "IDE protocol decoding + off-corpnet endpoint" as two genuine moats.**

**Sales priorities for the next 12-18 months:**

1. **Position aggressively against the three direct overlap competitors**: Palo Alto AIRS, Kong AI Gateway, WitnessAI. Lead with the leg they don't have.
2. **Close the two procurement-blocking gaps first**: multi-language PII (Kong-parity) and prompt-injection detector depth (Zscaler-parity). These are the items that show up in enterprise RFPs and are easiest to lose deals on without parity.
3. **Stake an MCP / agentic governance position** in the next two product cycles — this is the next contested territory.

---

## Sources

### Big SSE / SASE / CASB
- [Zscaler AI Security Suite](https://www.helpnetsecurity.com/2026/01/27/zscaler-ai-security-suite/) · [Zscaler AI overview](https://www.zscaler.com/products-and-solutions/zscaler-ai) · [AI Guard solution brief](https://www.zscaler.com/resources/solution-briefs/zscaler-ai-guard.pdf) · [AI Guard + NeMo Guardrails](https://www.zscaler.com/blogs/partner/securing-genai-applications-zscaler-ai-guard-and-nvidia-nemo-guardrails)
- [Netskope One AI Security](https://www.netskope.com/solutions/netskope-one-ai-security) · [Netskope launch press](https://finance.yahoo.com/news/netskope-unveils-netskope-one-ai-130000349.html)
- [Prisma AIRS](https://www.paloaltonetworks.com/prisma/prisma-ai-runtime-security) · [PANW + Portkey integration](https://www.paloaltonetworks.com/blog/2026/04/securing-and-governing-ai-agents-at-scale-through-a-unified-ai-gateway/)
- [Cisco AI Defense data sheet](https://www.cisco.com/c/en/us/products/collateral/security/ai-defense/ai-defense-ds.html) · [Cisco AI Defense Inspection API](https://developer.cisco.com/docs/ai-defense-inspection/introduction/) · [Cisco DefenseClaw](https://newsroom.cisco.com/c/r/newsroom/en/us/a/y2026/m03/cisco-reimagines-security-for-the-agentic-workforce.html)
- [Microsoft Purview AI](https://learn.microsoft.com/en-us/purview/ai-microsoft-purview) · [Purview RSA 2026](https://techcommunity.microsoft.com/blog/microsoft-security-blog/secure-data-as-ai-scales-new-microsoft-purview-innovations-at-rsa-2026/4503665)
- [Forcepoint + ChatGPT Enterprise Compliance API](https://www.forcepoint.com/blog/insights/forcepoint-openai-chatgpt-enterprise-compliance-api) · [Forcepoint ARIA + Endpoint](https://www.forcepoint.com/newsroom/2026/forcepoint-secures-ai-adoption-and-data-everywhere-new-aria-ai-assistant-and-endpoint)
- [Proofpoint DLP Transform for GenAI](https://www.proofpoint.com/us/newsroom/press-releases/proofpoint-bolsters-information-protection-offering-cross-channel-dlp) · [Proofpoint Secure Agent Gateway](https://www.proofpoint.com/us/newsroom/press-releases/proofpoint-secures-collaboration-and-data-agentic-workspace)
- [Symantec/Broadcom CloudSOC ChatGPT gatelet](https://techdocs.broadcom.com/us/en/symantec-security-software/information-security/symantec-cloudsoc/cloud/gateway-home/full-gatelet/chatgpt.html)

### Pure-play AI security startups
- [Lakera Guard](https://www.lakera.ai/lakera-guard) · [Protect AI / PANW acquisition](https://www.paloaltonetworks.com/company/press/2025/palo-alto-networks-completes-acquisition-of-protect-ai)
- [WitnessAI](https://witness.ai/) · [Prompt Security browser extension](https://chromewebstore.google.com/detail/prompt-security-browser-e/iidnankcocecmgpcafggbgbmkbcldmno)
- [Cato acquires Aim Security](https://www.helpnetsecurity.com/2025/09/04/cato-networks-aim-security/) · [Lasso Security](https://www.lasso.security/) · [Polymer DLP for AI](https://www.polymerhq.io/data-loss-prevention/)
- [Cisco AI Defense + Robust Intelligence](https://www.cisco.com/site/us/en/products/security/ai-defense/robust-intelligence-is-part-of-cisco/index.html) · [HiddenLayer AISec 2.0](https://hiddenlayer.com/innovation-hub/hiddenlayer-unveils-aisec-platform-2-0-to-deliver-unmatched-context-visibility-and-observability-for-enterprise-ai-security/)
- [Cranium exposure mgmt](https://cranium.ai/exposure-management/) · [F5 acquires CalypsoAI](https://calypsoai.com/news/f5-to-acquire-calypsoai-to-bring-advanced-ai-guardrails-to-large-enterprises/) · [Skyflow LLM Privacy Vault](https://www.skyflow.com/product/llm-privacy-vault)
- [Patronus AI eval API](https://www.patronus.ai/announcements/patronus-ai-launches-industry-first-self-serve-api-for-ai-evaluation-and-guardrails) · [Knostic GenAI Knowledge Security](https://www.knostic.ai/the-genai-knowledge-security-platform) · [YC S25 AI batch profile](https://catalaize.substack.com/p/y-combinator-s25-batch-profile-and)

### LLM gateway competitors
- [Portkey AI Gateway](https://portkey.ai/features/ai-gateway) · [Portkey pricing 2026](https://www.truefoundry.com/blog/portkey-pricing-guide)
- [LiteLLM auto-inject cache_control](https://docs.litellm.ai/docs/tutorials/prompt_caching) · [LiteLLM Anthropic provider](https://docs.litellm.ai/docs/providers/anthropic)
- [Helicone changelog](https://www.helicone.ai/changelog) · [Helicone gateway review](https://nolist.ai/item/helicone-gateway) · [LLM gateway tools 2026](https://techsy.io/en/blog/best-llm-gateway-tools)
- [Cloudflare AI Gateway features](https://developers.cloudflare.com/ai-gateway/features/) · [Cloudflare Guardrails](https://developers.cloudflare.com/ai-gateway/features/guardrails/)
- [Kong AI Gateway 3.10 PII](https://konghq.com/blog/product-releases/ai-gateway-3-10) · [Kong AI Sanitizer plugin](https://developer.konghq.com/plugins/ai-sanitizer/) · [Kong AI Gateway 3.11](https://konghq.com/blog/product-releases/ai-gateway-3-11)
- [AWS Bedrock Guardrails](https://aws.amazon.com/bedrock/guardrails/) · [Bedrock pricing 2026](https://medium.com/@aiengineeringonaws/aws-bedrock-pricing-explained-what-youll-actually-pay-in-2026-39377a27cdbd)
- [Vercel AI Gateway](https://vercel.com/docs/ai-gateway) · [Vercel AI Gateway pricing](https://vercel.com/docs/ai-gateway/pricing)
- [Lunar.dev AI Gateway](https://www.lunar.dev/product/ai-gateway) · [Lunar OWASP LLM coverage](https://www.lunar.dev/post/securing-genai-addressing-the-top-owasp-llm-risks-with-lunars-ai-gateway)
- [OpenRouter prompt caching](https://openrouter.ai/docs/guides/best-practices/prompt-caching) · [OpenRouter pricing](https://openrouter.ai/pricing)
- [Envoy AI Gateway (Tetrate + Bloomberg)](https://aigateway.envoyproxy.io/) · [Tetrate Agent Router Service](https://tetrate.io/blog/announcing-tetrate-agent-router-service)
- [F5 AI Gateway data leakage](https://www.f5.com/company/blog/ai-gateway-receives-new-data-leakage-detection-and-prevention-functionality) · [F5 AI Gateway overview](https://www.f5.com/resources/articles/ai-gateway-overview)
- [Apigee AI solutions](https://cloud.google.com/solutions/apigee-ai) · [Apigee LLM token policies](https://docs.cloud.google.com/apigee/docs/api-platform/tutorials/using-ai-token-policies)
- [Glama Gateway](https://glama.ai/ai/gateway) · [Lightport repo](https://github.com/glama-ai/lightport)
- [Martian Router](https://route.withmartian.com/) · [Martian valuation 2026](https://medium.com/@sarawgiapoorvwork347/martian-the-san-francisco-based-startup-that-invented-the-first-llm-router-is-reportedly-nearing-4211dd768296)
- [Braintrust AI proxy deprecation](https://www.braintrust.dev/docs/guides/proxy) · [Langfuse + AgentGateway integration](https://agentgateway.dev/blog/2026-02-17-agentgateway-langfuse-integration/)
- [Top 5 enterprise AI gateways 2026](https://www.getmaxim.ai/articles/top-5-enterprise-ai-gateways-in-2026-5/) · [Cloudflare CASB AI](https://blog.cloudflare.com/casb-ai-integrations/) · [Netskope AI Gateway](https://www.netskope.com/products/ai-gateway)
