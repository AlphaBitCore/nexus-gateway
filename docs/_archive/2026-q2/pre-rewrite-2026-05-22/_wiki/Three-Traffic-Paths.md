# Three Traffic Paths

*Audience: evaluators mapping Nexus Gateway capabilities to their deployment environment, and contributors understanding the data flows.*

Nexus Gateway intercepts AI traffic through three independent, parallel paths. The same AI request never flows through more than one path — each path is self-contained from interception through enforcement to audit emission. All three share the same compliance hook configurations (delivered via Hub shadow) and the same audit timeline (NATS → Hub audit sink → PostgreSQL), but each sits at a different network layer and targets a different deployment scenario. Choosing the right path — or combining them — depends on what generates AI traffic and how much control the operator has over that endpoint's configuration.

---

## Path A — AI Gateway (explicit SDK proxy)

**Interception mechanism.** Applications send requests directly to the AI Gateway's `/v1/*` HTTP surface, using a Virtual Key bearer token in place of a raw provider API key. No TLS interception or OS-level redirection is involved. The operator changes the SDK's base URL and replaces the API key with a virtual key.

**Supported ingress endpoints:**

| Endpoint | Ingress format | Canonical routing |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | ✅ all providers |
| `POST /v1/messages` | Anthropic Messages | ✅ all providers (cross-format canonicalized) |
| `POST /v1/responses` | OpenAI Responses API | ✅ providers with `responses-api` capability |
| `POST /v1/embeddings` | OpenAI Embeddings | ✅ providers with embeddings capability |
| `GET /v1/models` | Model catalog | ✅ |
| `POST /v1/estimate` | Pre-flight cost estimate | ✅ dry-run mode |

**Request lifecycle:**
1. VK authentication — `hashed_secret` lookup; hydrate `RequestContext` with org/project/quota-policy.
2. Request-stage hook pipeline — PII detection, keyword blocking, IP access control, custom webhook hooks.
3. Routing engine — 7 strategies produce a `ResolvedRequest` with provider, model, and credential reference.
4. Credential decrypt — AES-256-GCM decrypt in memory for the request lifetime only.
5. Upstream call — forwarded to the provider via the matching provider adapter.
6. Response-stage hook pipeline — PII redaction, content safety, hard/soft reject decisions.
7. Traffic event emission — cost stamping + audit record to NATS → Hub audit sink.

**When to use Path A.** Direct SDK integration is the primary use case: applications that call the OpenAI, Anthropic, or Gemini SDKs are reconfigured to point at the AI Gateway. This is the only path that provides Virtual Key-based budget enforcement, declarative routing rules with model fallback, cost tracking, prompt caching, and response caching.

---

## Path B — Compliance Proxy (transparent TLS)

**Interception mechanism.** Applications configure their HTTPS proxy environment variable or system proxy setting (e.g., `HTTPS_PROXY=http://compliance-proxy:3040`). No SDK code changes are required — the application continues calling its normal provider URL. The proxy receives HTTP CONNECT requests, dynamically mints a leaf certificate from a local CA, and inspects the plaintext traffic.

**Certificate management.** The proxy uses a two-tier cert cache: an in-process LRU cache for hot domains and a Valkey-backed shared cache for multi-instance deployments. Certificates are signed by the local CA; operators must install the CA root certificate on client machines (or in SDK trust stores) to avoid TLS validation errors.

**Traffic classification.** The Compliance Proxy uses a two-tier normalizer:

- **Tier 1 — canonical normalization.** For standard JSON API traffic (OpenAI API, Anthropic API, Gemini API), full `NormalizedPayload` extraction runs via `packages/shared/transport/normalize/codecs/<format>`. Token counts, usage stats, and canonical text are all extracted.

- **Tier 2 — text-first normalizer.** For consumer-surface traffic (ChatGPT web, Claude.ai, Cursor IDE, GitHub Copilot web), the required output is readable text. Token and usage statistics are secondary. The Tier-2 `NonJSONDetector` framework (`packages/shared/transport/normalize/extract/detector.go`) handles binary protocols, Google `batchexecute`, and Connect-RPC + protobuf framing. Adding a new non-JSON format means adding a `NonJSONDetector` in that file — not a fresh per-host adapter.

**Request lifecycle:**
1. CONNECT receive — source-IP allowlist and domain allowlist check.
2. TLS bump — cert mint from local CA; separate TLS sessions with client and upstream.
3. Traffic adapter detection — `interception_domain` ruleset match selects the adapter.
4. Hook pipeline — same three stages as the AI Gateway.
5. Upstream forwarding.
6. Traffic event + audit emission.

**When to use Path B.** The Compliance Proxy captures AI traffic without modifying application code. It is the right choice for auditing team development tools and SaaS applications via a shared network proxy setting, or for capturing browser-based AI surfaces (ChatGPT web, Claude.ai) that Path A cannot reach.

---

## Path C — Desktop Agent (OS-level endpoint)

**Interception mechanism.** The Desktop Agent intercepts outbound traffic at the OS network layer. No proxy configuration and no SDK changes are required.

| Platform | Mechanism |
|---|---|
| macOS | `NETransparentProxyProvider` (Network Extension) |
| Linux | `iptables` transparent proxy |
| Windows | WinDivert |

Once a flow is intercepted, the agent runs a forwarding pipeline structurally identical to the Compliance Proxy: **intercept → req_hooks → upstream_ttfb → upstream_total → resp_hooks**. The agent is a full forwarding proxy — it measures upstream TTFB and total latency on its forward path. It is not a passive sniffer.

**Audit durability.** Audit events are written to a local SQLCipher database on the endpoint and drained over mTLS to Hub. This queue survives connectivity interruptions; the device picks up where it left off on reconnect, with no gaps in the audit timeline.

**Enrollment.** On first boot, the agent receives a one-time enrollment token from the admin UI, submits a CSR to Hub, and stores the issued ECDSA P-256 device certificate in the platform keystore (`platform.DefaultPaths()` — never hardcoded paths). All subsequent Hub connections use this cert for mutual TLS.

**macOS safety.** The macOS Network Extension sits in every outbound packet path on the host. A misbehaving provider takes down DNS, DHCP, mDNS, NTP, Apple Push, and VPNs — not just AI traffic. The five fail-open invariants (synchronous `handleNewFlow`, async daemon timeouts, file-only enforcement lists, no `isLikelyXyz = true` patterns, system-service list validation) are mandatory for every NE code change. See [Fail Open Posture](Fail-Open-Posture).

**When to use Path C.** The Desktop Agent captures AI traffic generated by desktop tools (Cursor, Claude Code, GitHub Copilot desktop, ChatGPT desktop) without requiring any proxy configuration on the endpoint. It is the right choice when endpoints are managed devices that can receive the agent package, and when per-endpoint audit trails that survive network splits are required.

---

## Attestation header and deduplication

An endpoint running the Desktop Agent may send traffic to an upstream destination that routes through the Compliance Proxy on the same network. Without coordination, the same AI request would appear twice in the audit timeline and enforcement would run twice.

The agent stamps each intercepted request with an `X-Nexus-Agent-ID` header carrying the device's `thing_id`. When the Compliance Proxy receives a CONNECT request carrying this header, it recognizes the traffic as already intercepted by a Nexus Agent and acts as a transparent relay — it skips its own hook pipeline and audit emission. This prevents double-enforcement and double-counting in the audit timeline regardless of network topology.

---

## Capability comparison

| Capability | Path A — AI Gateway | Path B — Compliance Proxy | Path C — Desktop Agent |
|---|---|---|---|
| Virtual Key auth | ✅ | ❌ | ❌ |
| Per-VK quota enforcement | ✅ | ❌ | ❌ |
| Routing rules + model fallback | ✅ | ❌ | ❌ |
| Prompt cache (Anthropic / OpenAI / Gemini) | ✅ | ❌ | ❌ |
| Response cache (exact-match) | ✅ | ❌ | ❌ |
| Cost tracking | ✅ | Partial (no VK scope) | Partial (no VK scope) |
| Hook pipeline (3 stages) | ✅ | ✅ | ✅ |
| Consumer-surface capture (ChatGPT web, Claude.ai) | ❌ | ✅ | ✅ |
| No SDK change required | ❌ | ✅ | ✅ |
| Per-endpoint local audit queue | ❌ | ❌ | ✅ |
| Audit survives network splits | ✅ | ✅ | ✅ (local SQLCipher queue) |
| Multi-platform (macOS / Linux / Windows) | ❌ (server-side) | ❌ (server-side) | ✅ |
| TLS content inspection | ❌ (cleartext API) | ✅ (MITM) | ✅ (MITM) |

## Hook pipeline parity across paths

All three paths share the same hook pipeline from `packages/shared/policy/hooks`. The same Go code runs in the AI Gateway, the Compliance Proxy, and the Desktop Agent. Hook configurations are pushed from the Control Plane to Hub, propagated to all Things via Cat B shadow change-signals, and applied atomically.

Hook stages per path:

| Stage | Path A | Path B | Path C |
|---|---|---|---|
| Request (pre-upstream) | ✅ | ✅ | ✅ |
| Response (post-upstream) | ✅ | ✅ | ✅ |
| Per-chunk (streaming) | ✅ (streaming compliance mode) | ✅ (streaming compliance mode) | ✅ |

Hook decisions at each stage: `Approve`, `Modify` (e.g., PII redaction), `Reject` (hard or soft), `Abstain`. The pipeline aggregates: the first hard reject wins and short-circuits remaining hooks; any soft reject without a hard reject yields a soft rejection. The highest data-classification label seen across all hooks in a stage is recorded on the traffic event regardless of the final decision.

Built-in hook types (all three paths):
- PII detection + redaction (text scan + redaction).
- Keyword blocking (request content match).
- Content-safety classification.
- IP access control (source IP allowlist/blocklist).
- Rate limiting (sliding-window counters in Valkey).
- Outbound webhook calls (send event to external system for custom logic).

**`applicableIngress` filtering.** Hook configurations carry an `applicableIngress` field that restricts which traffic paths the hook applies to. A hook configured for `ai-gateway` only runs on Path A; one configured for `all` runs on all three. This lets compliance teams apply different enforcement policies per path without deploying separate hook sets.

## Audit timeline unification

Audit events from all three paths land in the same unified audit timeline in PostgreSQL. The pipeline:

1. **Path A and Path B**: events are emitted to NATS JetStream. Hub's audit-sink consumer group drains the stream and writes to `traffic_event` + `admin_audit_log` in PostgreSQL.
2. **Path C (Desktop Agent)**: events are written to a local SQLCipher database on the endpoint. The agent's upload worker drains the local queue over mTLS to Hub, which writes to the same tables.

The `source` column on `traffic_event` identifies the originating path: `ai-gateway`, `compliance-proxy`, or `agent`. The `trace_id` field stitches multi-hop flows across paths when the same request passes through more than one (e.g., a VK-authenticated SDK call that also transits the Compliance Proxy's network segment at the infrastructure level).

**Body storage.** Bodies smaller than 256 KB are stored inline in `traffic_event_payload`. Larger bodies overflow to the spillstore (S3 in production; local FS in development via `packages/shared/spillstore/`). The audit row stores a content-hashed reference; the CP UI fetches bodies via presigned URLs. This is a separate concern from hook decisions — hook enforcement runs regardless of whether the body is stored inline or in the spillstore.

## Deployment patterns and path selection

**Using Path A alone.** Direct SDK integration with Virtual Keys is the simplest deployment. Applications configure their base URL to point at `:3050` and replace their provider key with a virtual key. All compliance, routing, and cost tracking are centralized at the AI Gateway.

**Using Path B alone.** Network-level HTTPS proxy configuration with the Compliance Proxy captures all AI traffic from an application without any code changes. Useful for third-party SaaS tools or applications where SDK access is not available.

**Using Path C alone.** The Desktop Agent captures endpoint AI traffic at the OS layer, including desktop applications (Cursor, Claude Code, GitHub Copilot) that cannot be reconfigured to use a proxy.

**Combining paths.** Paths A and C can be combined: SDK calls go to Path A with full routing/cache/cost features, while desktop tools on the same machine are captured by Path C for audit-only coverage. The attestation header prevents double-counting when a Path C intercepted request also transits a Compliance Proxy on the network.

Paths A and B can also be combined: SDK calls use Virtual Keys (Path A) while browsers and miscellaneous tools on the same corporate network route through the Compliance Proxy (Path B). These are additive — both audit timelines merge.

## Config propagation to all three paths

Hook configurations and enforcement policies are authored once in the CP UI and propagate to all three paths via the Hub shadow model. The relevant shadow keys:

- `hooks` (Cat B) — the full hook configuration list, applied to all three data-plane services. Each hook carries an `applicableIngress` field that restricts which paths execute it.
- `agent_settings` (Cat B) — Desktop Agent-specific settings including `trafficUploadLevel` (`all`, `processed`, `blocked`; default `processed`) and the QUIC bundle ID list pushed to the NE provider via the daemon-written file.
- `routing_rules` (Cat B) — AI Gateway only. Defines the routing engine's match→strategy tree.
- `killswitch` (Cat A) — inline Cat A; bypasses enforcement across all AI Gateway instances within milliseconds.

When an admin changes a hook configuration, all three data-plane services receive the change-signal and apply the new config atomically within sub-second. The Config Sync page shows per-Thing per-key drift — if one AI Gateway instance is slow to apply, it is visible immediately.

## Traffic event schema

Every traffic event emitted by any of the three paths shares the same `traffic_event` schema in PostgreSQL. Key columns:

| Column | What it holds |
|---|---|
| `source` | `ai-gateway`, `compliance-proxy`, or `agent` — identifies the originating path |
| `trace_id` | Correlation ID for cross-path stitching |
| `virtual_key_id` | Present on Path A (VK-authenticated); null on Paths B and C |
| `provider` / `model` | Resolved provider and model IDs |
| `prompt_tokens` / `completion_tokens` | Token counts (extracted by the normalizer) |
| `cost_usd` | Total USD cost stamped by `metrics.CalculateCost` |
| `hook_decision` | Final aggregated hook decision (`approve`, `modify`, `reject_soft`, `reject_hard`) |
| `passthrough` | Boolean; `true` when emergency passthrough was active for this request |
| `cache_status` | `hit`, `miss`, `miss_evict` — response cache status (AI Gateway only) |
| `source_ip` | Client IP (useful for filtering by device / network segment in analytics) |

This unified schema means compliance queries run identically across all three paths. An analyst writing a query for "PII-flagged requests in the last 24 hours" does not need to join across separate tables for each path.

## Audit timeline reconstruction

All three paths contribute to a single unified `traffic_event` table. When reconstructing an incident timeline, the `source` and `trace_id` columns are the primary keys for cross-path correlation.

A typical investigative query pattern for a compliance incident:

```sql
-- Find all PII-flagged requests in a 1-hour window, across all paths
SELECT
    te.id, te.source, te.trace_id, te.created_at,
    te.provider, te.model,
    te.hook_decision, te.hook_reason,
    te.virtual_key_id,
    ta.device_id
FROM traffic_event te
LEFT JOIN thing_agent ta ON te.thing_id = ta.thing_id
WHERE te.hook_decision IN ('reject_hard', 'reject_soft', 'hook_error')
  AND te.created_at BETWEEN '2026-05-21T14:00:00Z' AND '2026-05-21T15:00:00Z'
ORDER BY te.created_at;
```

The `LEFT JOIN thing_agent` resolves the device identity for Path C events. For Path A events, `virtual_key_id` identifies the project and org. For Path B events, `source_ip` and `thing_id` (the Compliance Proxy instance) are the best identifiers.

Body content for flagged events is retrievable via the spillstore presigned URL:

```bash
cp_curl GET /admin/traffic/<event_id>/body
# Returns: { "presignedUrl": "https://s3.amazonaws.com/..." }
```

## Path B certificate trust distribution

For Path B (Compliance Proxy) to work without TLS errors in client applications, the Compliance Proxy's CA root certificate must be in the client's trust store. Operators handle this differently by environment:

| Deployment context | Trust distribution method |
|---|---|
| Corporate-managed macOS / Windows endpoints | MDM profile push (Apple Configurator, Intune, JAMF) |
| Containerized applications | Mount CA root in container; install via `update-ca-certificates` at image build time |
| Node.js SDK integration | Set `NODE_EXTRA_CA_CERTS` env var to the CA root PEM path |
| Python SDK integration | Set `REQUESTS_CA_BUNDLE` or `SSL_CERT_FILE` env var |
| Go SDK integration | Use `crypto/x509.SystemCertPool()` with CA appended, or set `SSL_CERT_FILE` |
| Browser (for consumer-surface traffic) | Install CA root via OS trust store; browsers pick it up automatically |

The CA root PEM is available for download from the CP UI (Settings → Compliance Proxy → CA Certificate). The admin who configured the Compliance Proxy can also export it via `cp_curl GET /admin/compliance-proxy/ca-certificate`.

Browsers that use certificate transparency (CT) log requirements will not accept custom CAs for CT-checked domains. This only affects HTTPS-Everywhere compliance; the Compliance Proxy CA is not in any CT log, which is intentional (the CA is private and should not be publicly discoverable).

## Cost accounting across paths

Cost tracking capabilities differ across the three paths because they depend on token extraction and Virtual Key scope:

**Path A (AI Gateway).** Full cost tracking: the canonical `Usage` struct extracted from the upstream response feeds `metrics.CalculateCost(usage, prices)`, producing a four-component USD breakdown per request (uncached input, cache read, cache write, output). Cost is attributed to the Virtual Key → Project → Organization hierarchy. Budget enforcement (per-VK cost limits via Valkey counters) is exclusive to Path A.

**Path B (Compliance Proxy).** Partial cost tracking. For Tier-1 JSON API traffic (OpenAI API, Anthropic API), token counts are extracted and cost is estimated. For Tier-2 consumer-surface traffic (ChatGPT web, Claude.ai), token extraction is best-effort or unavailable. No Virtual Key scope — cost is attributed to the source IP or device, not to a project budget.

**Path C (Desktop Agent).** Same partial-cost semantics as Path B: token counts when extractable, no VK scope. Cost attribution is to the enrolled device (and transitively to the `user_id` on the `thing_agent` row, which is the OS user who enrolled the device).

For compliance analytics use cases, the `cost_usd` column on `traffic_event` is populated across all three paths where extraction is possible and `NULL` where it is not. Analytics queries that want total cost should `SUM(COALESCE(cost_usd, 0))` to avoid NULL propagation.

## Path A — Routing engine and multi-provider strategies

Path A's routing engine is the most feature-rich of the three paths. It supports seven routing strategies, applied in order per routing rule:

| Strategy | Description |
|---|---|
| `single` | Route to exactly one (provider, model) pair. |
| `load-balance` | Distribute across a pool of (provider, model) pairs using weighted round-robin; stickiness optional. |
| `fallback-chain` | Try providers in sequence; move to the next on HTTP 5xx, rate-limit (429), or model capacity error. |
| `conditional` | Match against request metadata (org, project, model, user, header) and select a strategy per branch. |
| `a-b-split` | Route a percentage of traffic to an experimental (provider, model); the rest to production. |
| `policy-narrowing` | Apply IAM-style policy narrowing to restrict which (provider, model) pairs are accessible for this org/project. |
| `smart-llm-dispatch` | LLM-assisted routing that classifies request complexity and routes to the cheapest capable model. |

The routing engine produces a `ResolvedRequest` struct: `(provider, model, credential_ref, bypass_flags)`. This is what the executor uses — the routing strategy is opaque to the executor.

Routing rules are Cat B shadow keys. When an admin updates a routing rule, all registered AI Gateway instances receive the change-signal and apply the new rule within sub-second. The Config Sync page shows per-instance applied versions.

## Path B — TLS certificate lifecycle

The Compliance Proxy issues TLS certificates on demand for every domain it intercepts. The cert lifecycle:

1. Client sends `CONNECT api.openai.com:443`. Proxy accepts the tunnel.
2. Proxy checks domain allowlist (configured via `domain_predicates` Cat B shadow key). Unknown domains pass through as a raw CONNECT tunnel without TLS bump.
3. For domains in scope: proxy issues a leaf cert signed by the local Compliance Proxy CA. The cert is generated once per domain per session and cached:
   - **In-process LRU cache** — hot domains; zero roundtrip.
   - **Valkey-backed shared cache** — multi-instance or multi-process deployments; TTL-controlled expiry.
4. Proxy establishes two TLS sessions: client ↔ proxy (with leaf cert) + proxy ↔ upstream (standard TLS with real upstream cert).
5. Plaintext is available inside the proxy for hook pipeline and normalizer.

The CA root certificate must be installed on client machines for TLS validation to succeed. Operators distribute this via MDM or corporate cert trust policy. The CA root is generated on first proxy boot and stored on disk at the path returned by `platform.DefaultPaths().ProxyCARoot`.

## Path C — Platform-specific intercept mechanics

The Desktop Agent's OS-level intercept works differently on each platform:

**macOS — Network Extension.** The `NETransparentProxyProvider` runs as a system extension in the network stack. Every outbound TCP and UDP flow is offered to `handleNewFlow`. The NE provider claims flows matching the enforcement scope and relays them through the agent's forwarding pipeline. The NE provider is a separate process (`NexusAgentExtension`) from the main agent daemon (`NexusAgent`); they communicate via local IPC (`IPCProtocol.swift`). The NE binary requires a separate code-signing certificate and a `com.apple.developer.networking.networkextension` entitlement.

**Linux — iptables transparent proxy.** The agent installs `iptables` `TPROXY` rules that redirect outbound traffic to a local listener. The agent then relays the traffic. This requires root or `CAP_NET_ADMIN`. Unlike macOS, there is no OS-enforced separation between the intercept driver and the user-space agent.

**Windows — WinDivert.** The agent uses the WinDivert kernel driver to intercept outbound packets. WinDivert requires a signed kernel driver certificate in production. The agent relays matching flows.

On all three platforms, the forwarding pipeline steps are: intercept → apply request-stage hooks → forward upstream → measure TTFB → receive response → apply response-stage hooks → relay to application. The agent is a forwarding proxy, not a passive sniffer — it establishes a new TCP connection to the upstream on behalf of the application.

## Choosing a path: decision guide

| Situation | Recommended path(s) |
|---|---|
| You control the application code and its SDK configuration | Path A (explicit Virtual Key integration) |
| You want routing rules, prompt caching, cost tracking, or response caching | Path A (only path with these features) |
| You need to capture browser-based AI tools (ChatGPT web, Claude.ai) | Path B (Compliance Proxy) or Path C (Desktop Agent on that device) |
| You cannot modify application proxy settings | Path C (Desktop Agent on managed device) |
| You need to capture Cursor IDE, Claude Code, GitHub Copilot desktop | Path C (Desktop Agent) |
| You need per-endpoint audit trails that survive network splits | Path C (local SQLCipher queue) |
| You want both SDK-level controls and desktop-tool coverage | Path A + Path C in combination |
| You want network-level coverage for a shared team environment | Path B (shared proxy server) |
| You need to audit teams using corporate SaaS tools | Path B (HTTPS proxy setting pushed via MDM) |

The attestation header (`X-Nexus-Agent-ID`) ensures that combinations of paths do not produce double-counting. Operators do not need to filter or de-duplicate audit events; the gateway handles it.

---

## Canonical docs

- [`overview.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/overview.md) — §6 three traffic paths with per-path pipeline steps and the independent-parallel design rationale
- [`provider-adapter-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/ai-gateway/provider-adapter-architecture.md) — AI Gateway adapter framework and ingress format catalog
- [`agent-ne-fail-open-architecture.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/developers/architecture/services/agent/agent-ne-fail-open-architecture.md) — macOS NE safety constraints for Path C with code-line citations

**Adjacent wiki pages**: [Architecture Overview](Architecture-Overview) · [The Five Services](The-Five-Services) · [Fail Open Posture](Fail-Open-Posture) · [Canonical Vs Wire Format](Canonical-Vs-Wire-Format) · [Control Plane Vs Data Plane](Control-Plane-Vs-Data-Plane)
