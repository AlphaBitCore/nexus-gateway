# Use Cases

Nexus Gateway serves organisations that need governance over AI API traffic — not as a future compliance posture, but as an operational requirement today. The use cases below cover the scenarios that most commonly drive adoption. Each scenario identifies who needs it, what the gap looks like without Nexus, and which specific capabilities close it.

---

## Enterprise compliance audit

**Scenario.** A financial services firm or healthcare provider must demonstrate to auditors that every AI API call has a complete, tamper-evident record: who made the call, when, what was sent, what was returned, what data classification was applied, and which policy decisions were made. Retaining a claim that no PII was sent is not acceptable — the auditor wants the record.

**Who needs this.** Chief Compliance Officers, Information Security teams, and Risk Management functions at organisations operating under SOC 2, ISO 27001, HIPAA, or the EU AI Act (full enforcement August 2026). Also relevant for any organisation responding to an internal AI governance mandate.

**What Nexus does.** Every AI call through any of the three paths produces a `traffic_event` row with request and response bodies (inline ≤256 KiB, S3 spillstore for larger payloads), hook decisions, data classification label, routing trace, provider, model, token counts, cost, and source attribution. The audit log is non-modifiable via normal API paths, searchable from the admin dashboard, and forwardable to an external SIEM via the SIEM bridge. Per-stage hook recording means the exact pipeline decision (`request_hook_decision` and `response_hook_decision`) is captured independently, so an investigator can reconstruct exactly what the compliance engine saw and decided.

**Links.** [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Feature PII Redaction](Feature-PII-Redaction) · [Control Plane Audit Log](Control-Plane-Audit-Log)

---

## Multi-provider cost arbitrage

**Scenario.** An engineering team spends $40K/month on GPT-4o for a batch summarisation workflow that runs at low priority overnight. Moving the batch to DeepSeek-V3 or `gemini-2.5-flash` costs 90% less for comparable quality. The team does not want to rewrite the application to support a second provider SDK, and the platform team does not want 12 engineering teams each negotiating provider contracts and managing API keys.

**Who needs this.** Platform engineering teams building internal AI platforms. Application teams who want to move to a cheaper provider without a rewrite. Finance teams managing AI spend across a large engineering organisation.

**What Nexus does.** Applications speak the OpenAI SDK (`/v1/chat/completions`) — no code change. A routing rule in the admin dashboard targets a cheaper provider for the low-priority model type. The AI Gateway translates the canonical OpenAI-shaped request to the target provider's wire format (Anthropic, Gemini, DeepSeek, Moonshot, or any of 47+ adapters), forwards it, translates the response back, and returns it in the OpenAI shape the application expects. The response cache adds another saving layer: at 70.5% cache hit rate in production, 61.7% of upstream cost was eliminated in a measured 28,591-request benchmark run.

**Links.** [Feature Multi Provider Routing](Feature-Multi-Provider-Routing) · [Feature Response Cache](Feature-Response-Cache) · [Feature Cost Tracking](Feature-Cost-Tracking) · [AI Gateway Providers And Models](AI-Gateway-Providers-And-Models)

---

## Vendor lock-in escape

**Scenario.** An organisation standardised on a single AI provider two years ago. The provider changes pricing, reliability degrades, or a better model launches elsewhere. Switching requires finding every `OPENAI_BASE_URL`/`anthropic.Anthropic()` call in every application, negotiating a new contract, and testing against a different API surface — a multi-month project.

**Who needs this.** CTOs and architects who recognise that standardising on a single LLM provider is a structural liability. Platform teams building AI infrastructure that will outlast the current provider landscape.

**What Nexus does.** All applications send requests to the AI Gateway's `/v1` surface using virtual keys. Provider selection is a routing rule in the control plane — not a code decision. Switching providers is an admin config change, not an engineering sprint. The gateway owns the canonical↔wire translation for each provider (see `packages/ai-gateway/internal/providers/specs/<name>/`), so reasoning tokens, function calls, vision inputs, structured outputs, and streaming SSE are all preserved through the translation without application changes.

**Links.** [Feature Multi Provider Routing](Feature-Multi-Provider-Routing) · [AI Gateway Provider Adapters](AI-Gateway-Provider-Adapters) · [AI Gateway Routing Rules](AI-Gateway-Routing-Rules)

---

## AI dev-tool governance (Cursor, Copilot, Claude Code)

**Scenario.** Engineers use Cursor IDE, GitHub Copilot, and Claude Code on corporate laptops. These tools send code — including internal business logic, credentials accidentally left in files, and customer data from test fixtures — to external AI providers. The security team has no visibility into what leaves the device, cannot detect when a developer pastes a private key or a customer record into a prompt, and cannot enforce the AI usage policy they have written.

**Who needs this.** Information Security teams that have received reports of source code appearing in AI training data. Compliance teams whose AI usage policy exists on paper but cannot be enforced. Legal teams concerned about attorney-client privilege material being sent to LLM providers.

**What Nexus does.** The Desktop Agent installs on developer workstations and intercepts AI traffic at the OS level using `NETransparentProxyProvider` on macOS, `pf` on Linux, and WinDivert on Windows. Cursor IDE's gRPC protocol and Claude Code's HTTPS traffic are decoded by Tier-1 adapters in the Compliance Proxy — not just inspected as bytes, but parsed to extract the prompt text. The PII detection hook classifies the transaction (Public / Internal / Confidential / Restricted), applies the configured action (allow / alert / redact / block), and records the full transaction. Because the Agent runs on the endpoint itself, it continues to enforce policy when the developer is off-corpnet.

**Links.** [Feature Desktop Agent](Feature-Desktop-Agent) · [Feature PII Redaction](Feature-PII-Redaction) · [Agent Overview](Agent-Overview) · [Compliance Proxy Overview](Compliance-Proxy-Overview)

---

## SDK gateway — drop-in OpenAI replacement

**Scenario.** A startup or mid-size company builds all AI features on the OpenAI SDK. The team needs credential centralisation (stop committing API keys), usage analytics, per-team quota enforcement, and basic audit trail — without adopting a complex enterprise platform.

**Who needs this.** Application teams who want to start with the SDK-proxy path only. Engineers evaluating whether Nexus's AI Gateway is a suitable drop-in for LiteLLM, Portkey, or a simple nginx reverse proxy.

**What Nexus does.** Point the OpenAI SDK's `base_url` at the AI Gateway (`:3050/v1`) and issue a virtual key from the admin dashboard. The AI Gateway serves `/v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, and `/v1/models` with full OpenAI API compatibility. Virtual keys are HMAC-SHA256 hashed at rest — raw keys never touch the database. Per-virtual-key model restrictions, token budgets, and USD quotas are configured in the dashboard. Traffic analytics, cost tracking, and the cache ROI dashboard surface usage data without any application instrumentation. The compliance proxy and desktop agent paths are independent — starting with the SDK gateway path does not require deploying the other two.

**Links.** [Feature Multi Provider Routing](Feature-Multi-Provider-Routing) · [AI Gateway Virtual Keys Quotas](AI-Gateway-Virtual-Keys-Quotas) · [Your First AI Request](Your-First-AI-Request)

---

## Regulated industry readiness (HIPAA, SOC 2, EU AI Act)

**Scenario.** A hospital system wants to use AI for clinical documentation. A financial institution wants to use AI for customer-service summarisation. Both face regulated data environments where AI adoption must be accompanied by demonstrable control evidence — data classification, access control, audit records, retention policy, and the ability to demonstrate the system behaved as intended.

**Who needs this.** Healthcare providers subject to HIPAA. Financial institutions subject to PCI DSS or local banking data regulations. Any organisation in scope for EU AI Act Article 17 (risk management) or NIST AI RMF. Compliance teams preparing for SOC 2 Type II audits that include AI systems in scope.

**What Nexus does.** The full audit trail covers every AI call with classification label and hook decisions. The spillstore retains full prompt+response bodies under configurable retention policy. Virtual keys restrict which models a given application or team may access. IAM policies with NRN resource scoping limit which admin users can change which configurations. The three-tier kill switch (organisation / provider / route) allows immediate traffic halting with Hub reconciliation. Emergency passthrough mode — with a maximum 8-hour expiry and automatic Hub revert — covers compliance-pipeline outage scenarios without requiring manual intervention. The architecture is compatible with air-gapped deployment (no external dependencies at runtime); an operator runbook for air-gapped installation is a documented gap (see [Production State](Production-State)).

**Links.** [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Feature IAM And SSO](Feature-IAM-And-SSO) · [Kill Switch](Kill-Switch) · [Emergency Passthrough](Emergency-Passthrough) · [Production State](Production-State)

---

## SIEM integration and security operations

**Scenario.** A security operations team already centralises log data in Splunk, Datadog, or a similar SIEM. AI API calls represent a new threat surface — prompt injection attempts, exfiltration via model outputs, misuse of corporate credentials embedded in prompts. The team wants AI traffic events to flow into the same platform as every other security signal, with the same alerting and investigation tooling.

**Who needs this.** SOC teams that need AI API calls in their existing SIEM workflow. Threat-hunting teams looking for anomalous AI usage patterns. Incident response teams who want to replay what was sent to an AI provider during a security incident.

**What Nexus does.** The SIEM bridge in Nexus Hub forwards `traffic_event` records (including hook decisions, data classification labels, and full prompt+response text via spillstore references) to configurable sinks — OTEL/webhook delivery is supported. Built-in alert rules fire on compliance metrics (rejection rates, error rates, credential health, agent offline, virtual-key expiry) and deliver to webhook or SIEM channels. The audit log schema is consistent across all three traffic paths, so SIEM queries can correlate AI calls from the SDK gateway, the compliance proxy, and endpoint agents in a single query.

**Links.** [Feature Audit And SIEM](Feature-Audit-And-SIEM) · [Control Plane SIEM Bridge](Control-Plane-SIEM-Bridge) · [Control Plane Alerting Rules](Control-Plane-Alerting-Rules)

---

## Canonical docs

- [`docs/users/product/features.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/features.md) — per-feature detail covering all capability areas with deployment mode notes
- [`docs/users/product/competitive-landscape.md`](https://github.com/AlphaBitCore/nexus-gateway/blob/main/docs/users/product/competitive-landscape.md) — competitive positioning per use-case quadrant

**Adjacent wiki pages**: [What Is Nexus Gateway](What-Is-Nexus-Gateway) · [Why Nexus](Why-Nexus) · [Comparisons](Comparisons) · [Production State](Production-State) · [Features Index](Features-Index)
