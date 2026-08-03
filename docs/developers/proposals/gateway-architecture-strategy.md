# Gateway Architecture Strategy — Capability × Processing-Shape × Competitive Position

Status: **PROPOSAL** (design synthesis, no code). Owner-gated for any implementation it recommends.
Scope: answers three linked questions raised while building the multimodal gateway (Epic e88):

1. Are image generation and the other modalities all solved with the same "parallel architecture" we chose for STT — or does each modality need its own answer?
2. What capabilities do competing AI gateways ship that we should weigh before committing a roadmap?
3. How do we choose the next investments **globally** (not locally-optimal per modality)?

This document is the single reference that ties the modality work to a whole-product roadmap. It supersedes the scattered per-modality architecture notes in `e88-s5-stt-transcription.md` (§Architecture Decision) by generalizing that decision into a rule that covers every capability, shipped or planned.

This is v2 — revised after a three-lens adversarial review (architecture / product-competitive / factual-traceability). The review corrected the taxonomy (image *edits* are not S1), reframed the moat priority (multi-ingress is the durable one), fixed several matrix miscounts, and reordered the roadmap away from a modality-in-flight local optimum. Change log at the end.

---

## 1. The core architectural rule: sort by request processing SHAPE, not by "modality"

The instinct when adding image/audio/video is to ask "is this generative?" or "is the response big?". Both are the wrong discriminator. The load-bearing question is: **what machinery does this request's processing shape require, and does that machinery fit the `ServeProxy` small-JSON pipeline or fight it?**

`ServeProxy` (the chat/embeddings pipeline) is built for a **JSON-body request the executor can hold as a byte slice and retry by re-reading**: admission → routing → quota → request hooks → cache → execute → respond, with an immutable `ResolvedRequest` view and `defer finalizeAudit()` covering every exit. It assumes the whole request body is a byte slice it can parse, canonicalize, cache-key, and re-encode. The *response* may be multi-MB, streamed (SSE), or carry unscannable binary leaves — that does not eject it from `ServeProxy`; only the **request** transport shape does.

**Scope of this taxonomy:** the four shapes below classify **inference calls** (a request that invokes a model and relays a result). Control-plane / management capabilities that create and store a stateful resource for later reference — a versioned **prompt registry**, an **MCP tool registry**, provider **Files APIs**, a **vector store** — are a *management-plane* concern with a CRUD lifecycle, not a request-processing shape. They are governed by IAM + admin-audit, not by this model. The roadmap's prompt-management (§5) and MCP-as-gateway (§5) are management-plane, and deliberately have no S-shape.

Four processing shapes cover every **inference** capability. Each maps to exactly one architecture. The discriminator is **request transport encoding**, not modality name:

| Shape | Request transport | Machinery needed | Architecture | Inference capabilities |
|---|---|---|---|---|
| **S1 JSON-body request** | Whole request is a JSON byte slice (may embed base64 leaves); response is synchronously relayable (small, streamed, or multi-MB) | Full `ServeProxy` pipeline (cache, hooks, canonical/codec, cross-format translation) | **Stays in `ServeProxy`** | chat (incl. SSE), embeddings, vision-in-chat, audio-in-chat (base64), **image generation**, TTS, rerank |
| **S2 multipart-binary upload** | Request body is a multi-MB opaque binary file part that cannot be JSON-parsed or content-scanned pre-processing | Bounded multipart reader, `io.Reader` streaming forward, no cache, no input hooks | **Parallel handler** (`sttproxy`) sharing only cross-cutting `admitCommon` | STT (`/audio/transcriptions`, `/audio/translations`), **image edits/variations** (`/images/edits`, `/images/variations`), multipart file/PDF upload |
| **S3a opaque-artifact async** | Provider accepts a job, returns later a single opaque binary artifact (mp4/png) via poll/webhook | Job store + webhook inbound + **artifact proxy/store with URL authz + fingerprint** | **§8 async subsystem (artifact leg)** | video generation, async image |
| **S3b bulk-S1 async** | Provider accepts a bundle of N inference items, returns a JSONL bundle of N inner responses later | Job store + webhook inbound + **fan-out of N results each through S1 governance** (per-item cost, scan, `traffic_event`) | **§8 async subsystem (fan-out leg)** | batch API |
| **S4 long-lived bidirectional** | A persistent socket carries interleaved frames both directions for the session lifetime | WebSocket face, per-session cap, directional streaming compliance, live-session registry | **WebSocket subsystem** | realtime voice, realtime multimodal |

**The answer to question 1**: A modality is **not** a shape — it splits across shapes by *endpoint*, exactly as audio does (audio-in-chat is S1; STT upload is S2). Specifically:
- **Image generation** (`/v1/images/generations`) is **S1**: its request is small JSON (`{prompt, size, n}`). Although its *response* carries multi-MB base64, that response is still a single in-memory JSON blob our codec flattens and our cross-shape bridge translates (OpenAI-images ↔ Gemini `:generateContent`). Splitting it out would *rebuild* the codec, canonical bridge, and failover legs it reuses for free. Image gen stays in `ServeProxy`.
- **Image edits / variations** (`/v1/images/edits`, `/v1/images/variations`) are **S2**: multipart with a binary source-image part (up to ~25 MB), identical in shape to STT. They belong on the `sttproxy` parallel path, and `ingress-api.md` already groups them with the STT multipart siblings to ship together with the multipart work.
- **TTS** is **S1** (small-JSON request, raw-bytes response we pass through). **STT** is **S2** (multipart audio upload).

So only STT, image-edits (S2), video/batch (S3), and realtime (S4) leave `ServeProxy`. Chat, embeddings, image *generation*, TTS, and rerank stay. This is **globally consistent**: every inference capability sorts by request transport encoding into one shape, and each shape has one architecture. We are not making per-modality ad-hoc choices.

### What every path shares — and what it does NOT

Splitting a path out of `ServeProxy` must never silently drop *admission-time* governance. The STT SDD review caught exactly this — the first parallel-path draft dropped RPM, the overload gate, and the kill switch. The fix is a shared **`admitCommon`** helper that every path (S1–S4) calls at admission: auth → RPM → generative caps → quota **pre-flight check** → route+credential resolve → kill-switch/passthrough check → open the audit record. What a split path drops is only the machinery its shape makes meaningless: **cache** (an audio blob or live socket has no reusable key), **request-content hooks** (binary can't be text-scanned pre-processing), **canonical/codec** (no cross-format pivot for raw bytes), and the **synchronous executor**.

**`admitCommon` is an admission-time floor only.** Three controls that are meaningful at admission for S1 do *not* fully carry into S2–S4, and the split subsystems must build the post-admission equivalents themselves — this is net-new, not shared:

| Control | S1 (ServeProxy) | S2/S3/S4 reality | Who provides it |
|---|---|---|---|
| Cost accounting | pre-flight estimate + admission-time cap rejection | **reconcile-only** (STT: post-transcription; realtime: in-band per `response.done.usage`; async: at job completion) | per-subsystem metering, not `admitCommon` |
| RPM / caps | per-request RPM | S4 real bound is a **per-VK session-count cap** + mid-session quota reconcile (RPM on one 60-min socket is a near-no-op) | realtime subsystem (G5), net-new |
| Kill switch | gates each new request | severing an **in-flight** S3 job / S4 session needs a **live-session registry / job-cancel** | net-new; realtime **RT-9** currently *signs this as an accepted residual* (a revoked VK keeps a paid session alive up to an hour) |

The honest statement is: `admitCommon` closes the *admission* silent-drop gap uniformly; **post-admission governance for S3/S4 (mid-session quota reconcile, live kill-switch severing, completion-time cost metering) is per-subsystem net-new work**, tracked in the realtime spike (G2/G5) and video/batch async design, and one piece of it (RT-9) is a knowingly-signed residual.

---

## 2. Competitive capability matrix — Nexus vs 7 gateways

Retrieved 2026-07-15. Legend: ● shipped · ◐ partial / passthrough-only / enterprise-gated · ○ absent · △ in-design (Nexus only). Competitor cells sourced from each vendor's official docs at that date; **every ○ / claim about a competitor means "no official source located in the surveyed docs," not "confirmed absent."** Point-in-time snapshot; vendors ship fast.

| Capability | **Nexus** | LiteLLM | Portkey | Cloudflare | Kong | TrueFoundry | Bifrost | OpenRouter |
|---|---|---|---|---|---|---|---|---|
| **Routing / fallback / retry** | ● | ● | ● | ● | ● | ● | ● | ● |
| Latency-based routing | ○ | ● | ● | ◐ | ● | ● | ◐ent | ● |
| LLM smart routing (`model:auto`) | ● | ◐ | ◐ | ○ | ○ | ○ | ○ | ● |
| Health/circuit-breaking | ● | ● | ● | ● | ● | ◐ | ● | ● |
| **Exact-match cache** | ● | ● | ● | ● | ◐ | ● | ● | ● |
| **Semantic (vector) cache** | ● | ● | ● | ○ | ● | ● | ● | ○ |
| **Per-key RPM/TPM limit** | ● | ● | ● | ● | ● | ● | ● | ◐ |
| **Cost-based quota / budgets** | ● | ● | ● | ◐ | ● | ● | ● | ● |
| Budget *alerts* (proactive) | ● | ● | ● | ○ | ○ | ◐ | ◐ | ○ |
| **Per-request cost accounting** | ● | ● | ● | ● | ● | ● | ● | ● |
| Cost/analytics dashboards | ● | ● | ● | ● | ◐ | ● | ● | ● |
| **Gateway overhead / throughput** | ●¹ | ◐ | ◐ | ● | ◐ | ◐ | ●¹ | ● |
| **PII redaction (text)** | ● | ● | ● | ● | ● | ● | ● | ● |
| **Moderation / content-safety** | ● | ● | ● | ● | ● | ● | ● | ◐ |
| **Prompt-injection detector** | ◐ | ● | ● | ● | ● | ● | ● | ● |
| AI-Guard judge-model | ● | ◐ | ● | ○ | ◐ | ● | ◐ | ◐ |
| Rule-packs (installable) | ● | ○ | ◐ | ○ | ◐ | ◐ | ◐ | ○ |
| **Guardrails on NON-text (image/audio/video/realtime)** | ●△ | ○ | ○ | ○ | ○ | ○ | ◐img | ○ |
| Output guardrails coexist with streaming | ● | ◐ | ◐ | ○ | ◐ | ○skip | ◐ | ○ |
| **`traffic_event` per-request capture** | ● | ● | ● | ● | ● | ● | ● | ● |
| OTel tracing export | ◐ | ● | ● | ○ | ● | ● | ● | ● |
| SIEM bridge | ● | ◐ | ● | ○ | ◐ | ◐ | ◐ | ○ |
| **Chat / embeddings** | ● | ● | ● | ● | ● | ● | ● | ● |
| **Image generation** | ● | ● | ● | ◐ | ● | ● | ● | ● |
| Cross-shape image codec (OpenAI↔Gemini) | ● | ○ | ○ | ○ | ○ | ○ | ◐ | ○ |
| **TTS** | ● | ● | ● | ○ | ● | ● | ● | ● |
| **STT** | △ | ● | ● | ◐ | ● | ● | ● | ● |
| Video generation | △ | ◐ | ○ | ○ | ◐ | ○ | ◐ | ● |
| **Realtime WS voice** | △ | ○ | ◐ | ○ | ● | ● | ○ | ○ |
| Reranking | ○ | ● | ● | ○ | ○ | ● | ● | ● |
| **Prompt management / versioning** | ○ | ● | ● | ◐ | ◐ | ● | ● | ● |
| Batch API | ○ | ● | ◐ | ○ | ◐ | ● | ● | ○ |
| Fine-tuning proxy | ○ | ● | ○ | ○ | ○ | ● | ○ | ○ |
| MCP as gateway feature | ◐ | ◐ | ● | ○ | ● | ● | ● | ● |
| RAG / vector store | ○ | ○ | ◐ | ● | ● | ◐ | ○ | ◐ |
| Structured output / tool calling | ● | ● | ● | ● | ◐ | ● | ◐ | ● |
| Model/provider breadth (raw count) | ◐² | ● | ● | ◐ | ● | ● | ● | ●(400+) |
| **Credential vault (AES-GCM at rest)** | ● | ● | ● | ● | ● | ● | ● | ● |
| KMS/HSM envelope custody | ◐³ | ◐ | ● | ● | ● | ◐ | ◐ | ● |
| **Virtual keys** | ● | ● | ● | ● | ◐ | ● | ● | ● |
| **IAM / RBAC (policy engine)** | ● | ◐ | ● | ● | ● | ● | ● | ◐ |
| SSO / SAML | ● | ◐ | ● | ● | ● | ● | ● | ◐ |
| Tamper-evident admin audit log | ● | ○ | ● | ◐ | ● | ● | ● | ◐ |
| **Self-hosted / air-gapped** | ● | ● | ● | ○ | ● | ● | ● | ○ |
| Compliance certs (SOC2/HIPAA/FedRAMP) | ⁴ | ○ | ◐ | ● | ● | ◐ | ◐claim | ◐ |
| In-deployment tenant isolation (BU/org) | ◐⁵ | ● | ● | ● | ◐ | ◐ | ◐ | ● |
| Multi-tenant SaaS (vendor-hosted) | ○(by design) | ● | ● | ● | ◐ | ◐ | ◐ | ● |
| Plugin/ecosystem composition | ◐ | ◐ | ◐ | ○ | ● | ◐ | ● | ○ |

Footnotes on Nexus cells:
1. **Overhead** — Nexus's p95 hot-path program benchmarked hooks-**off** above Bifrost per dimension (non-SSE ~6300 RPS, ~1.19× Bifrost with far lower memory); hooks-**on** trades throughput for compliance scanning (expected — the compliance *is* the product). Bifrost's public claim is ~11µs overhead @ 5k RPS. Both are "fast tier"; we should publish our own number rather than cede this axis.
2. **Model breadth** — Nexus ships ~11 native provider adapters + an OpenAI-compat family (fireworks/groq/mistral/xai/…); we route *providers*, not a 400+ curated model catalog like OpenRouter. Breadth is adequate for enterprise BYO-provider, not a headline count.
3. **KMS** — envelope-custody substrate (`shared/core/kms`, aws-kms/sops/age/vault) **is shipped and wired** for service root-secret custody and the compliance-proxy CA DEK. Only the *provider-credential master key* (`CREDENTIAL_ENCRYPTION_KEY`) remains env-only (`credentials-architecture.md §8` V2). So `◐`, not absent.
4. **Certs** — Nexus is **self-hosted / single-tenant**, so SOC2/HIPAA/FedRAMP attestations attach to the *deployer's* environment, not to shipped software. This is a **strength** for regulated buyers: air-gapped self-hosting is an *enabler* for FedRAMP-High / IL5 / HIPAA environments a SaaS gateway legally cannot enter. There is no Nexus-vendor SaaS attestation because there is no Nexus SaaS. State this explicitly in sales — do not leave the row blank.
5. **In-deployment tenant isolation** — the VK → user/project → **org** hierarchy already gives per-org quota / policy / audit / traffic scoping within one deployment (covers the multi-BU / MSP-embed need). `◐` because it is not marketed or hardened as a "tenant" boundary; see §4.

Nexus cells corrected from v1: semantic cache is **shipped** (not a gap); KMS is **◐** (v1 wrongly marked ○); added overhead, model-breadth, certs, and in-deployment-isolation rows (v1 omitted them).

---

## 3. Where we lead — the differentiation thesis (durable moat first)

### 3a. The DURABLE moat: one compliance pipeline across three ingress paths
The genuinely un-buyable-off-the-shelf asset is that Nexus enforces the *same* 11-hook pipeline across **three governance ingresses**: the API gateway (VK ingress), the **MITM forward proxy**, and the **endpoint agent** (macOS Network Extension + device MITM). A pure-API competitor (LiteLLM, Portkey, OpenRouter) cannot assemble this by integrating a vendor guardrail API — the endpoint agent and device-level MITM are multi-quarter OS-level engineering (Network Extension entitlements, launch constraints, notarization, fail-open packet-path safety). An enterprise that wants "the same PII/policy whether traffic comes through our SDK, a proxied desktop app, or a managed laptop" cannot get that from any single-surface router. **This is the reason-to-buy that a competitor cannot fast-follow in a sprint.**

### 3b. The FIRST-MOVER lead: full-modality compliance (real, but cash it before it's copied)
Every gateway surveyed — LiteLLM, Portkey, Cloudflare, Kong, TrueFoundry, Bifrost, OpenRouter, plus Databricks and Bedrock — runs guardrails on **text/chat only** *in the surveyed docs*; image/audio/video/realtime content appears ungoverned. Concretely (per those docs): Cloudflare's guardrails are documented as mutually exclusive with streaming; TrueFoundry explicitly skips output guardrails on streaming; Bifrost moderates image only via Bedrock and audio not at all; no image-generation compliance was found in Portkey's docs.

Nexus already ships (a) image-prompt hard-block (403 even in observe mode), (b) streaming redact→buffer that escalates on enforce, and (c) per-request `compliance_coverage` honesty. STT v1b (output-transcript redaction) and realtime directional compliance extend this into audio and live sessions.

**But be clear-eyed about durability:** non-text moderation is available as commodity APIs *today* (Azure AI Content Safety, AWS Rekognition/Bedrock, Hive, Google Video Intelligence). A competitor who decides to care *wires one in* — they don't build detectors. So full-modality compliance is a **first-mover lead, not a structural moat**; it is defensible mainly because demand is still concentrated in a small regulated segment. The strategic play is to **bank it now** (while it differentiates) and let it ride on the durable §3a rails (which *are* un-copyable). Avoid absolute marketing ("no one else does this, ever") — the honest claim is "none found in the surveyed gateways, and it rides on our unique multi-ingress engine."

### 3c. Other genuine strengths
- **Cost-based quota, single canonical price source** — hierarchical VK → user/project → org, five enforcement actions (`allow` / `reject` / **`downgrade` to cheaper model** / `notify-and-proceed` / `track-only`), fail-closed on unpriced models, reboot-consistent counters. Auto-`downgrade` is rare (most competitors only reject or cap). Note: downgrade should be **surfaced to the caller** (response header / `traffic_event` field), never a silent model swap — silently degrading answer quality under budget pressure is a trust hazard.
- **Cross-shape image codec** (OpenAI-images ↔ Gemini Nano Banana) — a real engineering asset almost no competitor has. Frame it as a **tactical asset with a half-life**: its value decays as providers converge on OpenAI-compatible image endpoints. Valuable now; not a permanent moat.

---

## 4. Where we lag — honest gaps, ranked by strategic weight

Competitor-parity items we lack, ranked by (buyer-frequency × effort-to-close). "Who has it" counts are reconciled to the §2 matrix.

| Gap | Who has it (per matrix) | Why it matters | Effort | Verdict |
|---|---|---|---|---|
| **Prompt management / versioning** | LiteLLM, Portkey, TrueFoundry, Bifrost, OpenRouter (5/7) | The **one table-stakes capability we lack** — appears in ~80% of mainstream gateway bakeoffs. A versioned registry (immutable versions, GitHub-style diff, playback against production traffic). Management-plane, not an S-shape. | Medium | **Defer** (owner decision 2026-07-15) — highest-frequency mainstream-eval gap, but serves the LLMOps buyer, not our core compliance buyer; deliberately off-thesis, revisit if a target deal hinges on it |
| **Reranking** (`/v1/rerank`) | LiteLLM, Portkey, TrueFoundry, Bifrost, OpenRouter (5/7) | Small, well-scoped S1 (JSON↔JSON): new codec + cost formula in `ServeProxy`. RAG-adjacent buyers ask for it. | Low | **Close early (P0 floor)** — cheapest parity win |
| **Latency-based routing** | LiteLLM, Portkey, Kong, TrueFoundry, OpenRouter (5 full) + Cloudflare, Bifrost (2 partial) | We rank by health **band**, not measured p95 — there is no live per-target p95 tracker today. Naive "route to fastest" oscillates (thundering-herd); stable routing needs a windowed per-target p95 + hysteresis + sample-density guards. | **Medium** (not Low — needs a new latency window) | **Close early (P0 floor)** |
| ~~Budget alerts (proactive)~~ | LiteLLM, Portkey (full); TrueFoundry, Bifrost (partial) | **Already shipped** (re-assessment 2026-07-15): the `quota.threshold` AlertRule + `quota-alert-check` job fires percent-of-limit alerts (default 80/95%) BEFORE the hard cap rejects, across the VK→user/project→org chain, delivered over the existing channels with hysteresis auto-resolve. v1 wrongly listed this as a gap. | — | **Done** — no work; matrix corrected to ● |
| **KMS/HSM for the credential KEK** | Portkey, Cloudflare, Kong, OpenRouter | Envelope-custody substrate already ships (§2 note 3); only the provider-credential master key is env-only. Procurement gate for some enterprises. | Low-Med (substrate exists) | **Close when a deal needs it (P1)** |
| **MCP as gateway feature** | Portkey, Kong, TrueFoundry, Bifrost, OpenRouter (5/7) | "MCP is now table-stakes." We only passthrough OpenAI-Responses `mcp` tool; cross-format 400s it. A gateway-native MCP registry + tool-access governance (Cedar/OPA-style) aligns with the §3a compliance thesis. Management-plane. | High | **Design-first (P2)** |
| **Batch API** | LiteLLM, TrueFoundry, Bifrost (3/7) | S3b (bulk-S1 async): shares the job envelope with video but needs its own **per-item governance fan-out** (each inner response gets cost/scan/`traffic_event`). The fan-out, not the job store, is the real cost. | Med | **Fold into S3 subsystem (P2)** |
| **Fine-tuning proxy** | LiteLLM, TrueFoundry (2/7) | Rare; low demand; multi-step resource lifecycle. | Med | **Defer** |
| **RAG / vector store** | Cloudflare, Kong (native) | Data-plane product, not gateway-core. Semantic cache ≠ RAG store. | High | **Defer** — out of gateway charter |
| **Native trace persistence / trace UI** | Helicone (Sessions) | Our OTel is export-only. SIEM bridge partly covers the need. | High | **Defer** |
| **Multi-tenant SaaS (vendor-hosted)** | OpenRouter, LiteLLM, Portkey, Cloudflare | Deliberate non-goal (single-tenant by design). See below — do not conflate with in-deployment isolation. | — | **Won't do** (by design) |

**Multi-tenant — split the question (do not cede the segment on a label):**
- *(a) Nexus-the-vendor runs a hosted SaaS for many orgs* — legitimately not our model. **Won't do.**
- *(b) One customer needs tenant isolation **within** their own deployment* (multi-BU enterprise; MSP/reseller embedding Nexus for downstream customers) — a **real recurring ask**. Our VK → user/project → **org** hierarchy already provides per-org quota / policy / audit / traffic isolation (§2 note 5). Position this as a **strength we already have**, not a gap — but harden and market it as a tenant boundary if the MSP/reseller segment is a target.

**Two capabilities to promote from "inspiration" to scored candidates** (both monetize the durable §3a moat directly, more on-thesis than video *generation*):
- **Guardrail-as-a-service** (Bedrock `ApplyGuardrail` model) — a standalone "evaluate this content against our guardrails" API. Lands our compliance engine in the buyer's hand **without requiring them to route all traffic through the gateway** (the biggest adoption barrier for a governance product). **Scored: P1 design-first.**
- **MCP tool-governance** — not just passthrough; governed tool registry with IAM. Folds into the MCP-as-gateway P2 work above.

---

## 5. Prioritized roadmap (global — reordered away from the modality-in-flight local optimum)

**Positioning statement (the load-bearing decision):** Nexus is a **compliance-first gateway** — we win regulated deals decisively on multi-ingress + full-modality governance (§3a/§3b). But "compliance-first" is *not* a license to lose mainstream bakeoves on table-stakes gaps before the compliance story is even heard. So the roadmap **closes the cheap parity floor in parallel with banking the near-done compliance lead (STT)**, and **defers the heavy multi-quarter modality builds (realtime S4, video S3) until that floor is banked** — because those were always owner-gated, serve the narrowest segment, and are the two largest unestimated builds in the program.

This reorders v1: v1 put all of STT+realtime+video ahead of prompt-management (a local optimum following the modality SDD already in flight). v2 pulls the parity floor up and pushes realtime/video down.

### P0 — Parity floor + bank the cheapest compliance proof (do in parallel)
Rationale: remove the deal-losing mainstream-eval gaps, and cash the full-modality-compliance lead via its cheapest, highest-frequency modality (audio transcription ≫ realtime/video in buyer frequency).
1. **STT v1a → v1b** (S2 parallel `sttproxy`) — SDD ratified, `admitCommon` seam designed. v1b (output-transcript redaction) is the differentiation payload; it proves §3b on the highest-frequency non-text modality at S2 cost (not S4). *Owner-gated to start; effort: Med.*
2. **Reranking** (S1) — new codec + cost formula in `ServeProxy`. *Effort: Low.* **SHIPPED 2026-07-15** (`f8bf1cad3`).
3. ~~**Budget alerts**~~ — **already shipped** (`quota.threshold` + `quota-alert-check`); v1 mis-assessment, no work needed.
4. **Latency-based routing** — add a `sort: latency` strategy **plus a windowed per-target p95 tracker with hysteresis** (the health ranker is band-based today). *Effort: Med.*

> **Prompt management / versioning — deferred (owner decision 2026-07-15).** It is the highest-frequency mainstream-eval gap, but it serves the LLMOps buyer, not our core compliance buyer, and is deliberately off our differentiation thesis. Kept out of P0 to stay focused. Revisit only if a target deal hinges on it (then gate behind its own brainstorm + SDD, scoped to a versioned store + playback, never a prompt IDE).

### P1 — Monetize the moat + close the procurement gate
6. **Guardrail-as-a-service** (`ApplyGuardrail`-style decoupled endpoint) — design-first; directly monetizes §3a without requiring full-traffic routing.
7. **KMS for the credential KEK** — complete the last env-only master key onto the already-shipped envelope-custody substrate.

### P2 — Heavy differentiating modalities + MCP (design-first, owner + segment-gated)
These are the multi-quarter builds. Sequence them **after** the parity floor + STT compliance proof are banked, gated on owner decision and segment evidence.
8. **Realtime voice** (S4 WS subsystem) — needs the WS transport face that does not exist yet + live-session registry + directional compliance (RT-1..RT-9). Largest build; narrowest segment. *Effort: High (multi-quarter).*
9. **Video generation** (S3a) + **Batch API** (S3b) — build the async job subsystem once; video rides the artifact leg, batch rides the S1-fan-out leg. *Effort: High (shared subsystem).*
10. **MCP-as-gateway** (management-plane) — registry + tool-access governance aligned with the compliance thesis. *Effort: High.*

### Defer / Won't-do
Prompt management / versioning (owner decision 2026-07-15 — off-thesis), fine-tuning proxy, RAG/vector store, native trace UI — **defer**. Multi-tenant *SaaS* — **won't do** (by design); in-deployment org isolation already exists (§4), harden only if MSP/reseller becomes a target.

---

## 6. Decisions this document asks the owner to confirm

1. **Confirm the positioning bet (§5):** "compliance-first, but close the parity floor before the heavy modalities." This is the single decision the roadmap order rests on. If you instead want the modality spine (realtime/video) finished first regardless of mainstream-eval cost, say so — that reverts toward the v1 order and is a deliberate narrow-segment bet.
2. **P0 floor items (rerank / budget-alerts / latency-routing) — recommend proceeding without a separate design gate** (each additive to an existing subsystem). Prompt-management is **deferred** (owner decision, 2026-07-15) — off our compliance thesis; revisit only if a deal hinges on it.
3. **Promote guardrail-as-a-service to a real P1 design spike** — it monetizes the durable moat and is more on-thesis than video generation.
4. **STT remains the active in-flight work** (task #12) — no change; it is correctly P0.

The takeaway: the roadmap is not "catch up on features." It is **close the cheap parity floor so we stop losing mainstream evals (P0), bank the full-modality-compliance lead via its cheapest modality STT (P0), monetize the durable multi-ingress moat directly via guardrail-as-a-service (P1), then invest the heavy multi-quarter modality builds only once the floor is banked (P2)** — while declining the scope-expansion items (RAG, fine-tuning, multi-tenant SaaS) that would dilute a focused compliance-gateway product.

---

## Change log (v1 → v2, from three-lens adversarial review)

**Architecture lens:** split image into gen (S1) vs edits/variations (S2); bounded the taxonomy to *inference* capabilities (prompt/MCP registries are management-plane, no shape); split S3 into S3a opaque-artifact (video) vs S3b bulk-S1 (batch); reframed `admitCommon` as an admission-time-only floor with an explicit post-admission-gap table (cost reconcile-only, session-count cap, RT-9 signed residual); renamed S1 by request transport (not "small-JSON ↔ small-JSON").

**Product/competitive lens:** inverted the moat priority (multi-ingress §3a is the durable moat; full-modality compliance §3b is a first-mover lead to bank); added a positioning statement and **reordered the roadmap** (parity floor + STT up to P0; realtime/video down to P2); added effort estimates + opportunity cost to the heavy builds; split multi-tenant into SaaS (won't-do) vs in-deployment isolation (already have); promoted guardrail-as-a-service from footnote to scored P1; added matrix rows for overhead, certifications, model breadth, in-deployment isolation.

**Factual lens:** KMS ○→◐ (envelope-custody substrate is shipped/wired; only the credential KEK is env-only); fixed §3c "five actions" to include `allow`; reconciled §4 "who has it" counts to the matrix (reranking 5/7, MCP 5/7, fine-tuning 2/7); stated a counting rule for latency (5 full + 2 partial); aligned budget-alerts prose with the ◐ cells; hedged §3b absolutes ("across the board" / "zero" / "no competitor") to "none found in the surveyed docs."

---

## Sources

Competitor capabilities retrieved 2026-07-15 from each vendor's official documentation (LiteLLM, Portkey, Cloudflare AI Gateway, Kong AI Gateway, TrueFoundry, Bifrost/Maxim, OpenRouter) plus a standout scan of Helicone, LangDB, Databricks Mosaic AI Gateway, and AWS Bedrock. Nexus capabilities traced to in-tree architecture docs (`routing-`, `response-cache-`, `hook-`, `quota-`, `credentials-`, `iam-identity-`, `sse-streaming-compliance-`, `ingress-api.md`), `e88-*` specs, the realtime-voice proposal, `shared/core/kms`, and the p95 hot-path perf program. All "absent" competitor cells = no official source located in the surveyed docs, not confirmed-absent.
