# Control Plane UI — Compliance: interception and content inspection

This document covers the network-interception part of the COMPLIANCE sidebar section: **Interception Domains**, **AI Guard Backend**, **Streaming Compliance**, and **Payload Capture**. The hooks-and-policy part is in [compliance-hooks.md](./compliance-hooks.md), and the records part is in [compliance-records.md](./compliance-records.md). Sidebar labels and routes are defined in `packages/control-plane-ui/src/routes/shellRouteConfig.tsx`.

## Interception Domains

**Purpose.** Define the host-and-path rules that decide what the Compliance Proxy does with each network destination, and feed matching traffic through the right traffic adapter.

**List page.** Columns: name, host pattern, host match type, adapter id, priority, an enabled toggle, path count, and updated-at. Row actions are edit (opens the detail page) and delete. The toolbar has a search box and an enabled filter. Enabling a domain adds it to the proxy's domain allowlist.

**Create and detail.** The create form collects name, description, host pattern, host match type, adapter id (from the traffic-adapter catalog), an adapter config (JSON), the enabled flag, priority, a default path action, an on-adapter-error behavior, and a network zone. The detail page shows a summary card plus a nested Paths sub-table with add, edit, and delete. A path rule collects a path pattern (one per line), a match type, an action, a priority, a description, and an enabled flag; path rules are evaluated in priority order and fall back to the domain's default path action.

**Key concepts.** A host or path match type is `EXACT`, `PREFIX`, `GLOB`, or `REGEX`. The action — both the domain default and each path rule — is `PROCESS` (run the traffic through the compliance pipeline), `PASSTHROUGH` (tunnel it through uninspected), or `BLOCK` (reject it). The on-adapter-error behavior is `FAIL_OPEN` (the default — on an inspection/bump failure the flow is tunneled through uninspected so connectivity is preserved) or `FAIL_CLOSED` (the flow is refused on an inspection/bump failure rather than passing uninspected — use it for sensitive hosts where uninspected egress is unacceptable). The network zone is `PUBLIC` or `INTERNAL`.

**Where the data comes from.** `interceptionDomainApi` — `list`, `get`, `create`, `update`, `delete`, `createPath`, `updatePath`, `deletePath`, `listTrafficAdaptersCatalog`.

## AI Guard Backend

**Purpose.** A single configuration for the centralized AI content classifier that hooks and policies call to judge content.

**What you see.** One configuration form with a dry-run panel mounted below it.

**Controls.** The form selects a backend mode. In configured-provider mode it picks a provider and model. In external-url mode it collects an https external URL, a model id, and custom header rows (the only way to authenticate to the external classifier — for example an `Authorization` header), and shows a warning that data leaves the platform. The external classifier never reuses a stored provider credential. Both modes set a judge prompt template (resettable to the default), a timeout (1000 to 30000 ms), and a cache TTL (0 disables caching). The form also shows the read-only compliance webhook URL with a copy action. The dry-run panel picks a detector type, takes pasted content, runs it against the backend, and shows the decision, the judge latency, the cache hit or miss, and the request and response JSON.

**Key concepts.** The backend mode is `configured_provider` or `external_url`. Detector types are `prompt_injection`, `jailbreak`, `toxicity`, `secret_leak`, `tool_call_safety`, `hallucination`, `data_exfiltration`, and `custom`. A classifier decision is `approve`, `reject_hard`, `block_soft`, or `modify`.

**Where the data comes from.** `aiGuardApi` — `getConfig`, `saveConfig`, `dryRun`.

## Streaming Compliance

**Purpose.** Set the global default for how streaming (SSE) responses are handled. Per-host overrides live on Interception Domains and per-provider overrides live on Providers.

**What you see.** A single settings form.

**Controls.** A default mode select; numeric inputs for chunk bytes, hook timeout (ms), and max buffer bytes; a fail-behavior select; and switches for capture-request-body, capture-response-body, and raw-body-spill-enabled. An always-visible per-mode disclosure note states, in one plain sentence each, exactly what compliance enforcement every mode performs — including the wire risk an admin must see before choosing a real-time mode over buffering.

**Key concepts.** The default mode picks the enforcement-versus-latency trade-off for streamed responses:

- `passthrough` — streamed in real time; compliance scanning runs at storage time only, with no inflight redaction. Lowest latency, no inflight enforcement.
- `chunked_async` — real-time streaming with best-effort inflight redaction. High performance, but it carries a bounded-fragment risk: a complete sensitive value is never delivered, yet a short leading fragment may reach the client before redaction engages. Choose `buffer_full_block` for guaranteed redaction.
- `buffer_full_block` — the full response is buffered and scanned before any byte is delivered; redaction and hard-block are guaranteed (strong compliance), at the cost of higher time-to-first-byte.

The fail behavior is `fail_open` (continue on a hook error or timeout) or `fail_close` (block under `buffer_full_block`, audit-flag under `chunked_async`). A stream larger than the max buffer bytes spills to the spill store when raw-body-spill is enabled, or is truncated otherwise.

**Where the data comes from.** `systemApi` — `getStreamingComplianceConfig`, `updateStreamingComplianceConfig`.

## Payload Capture

**Purpose.** Control whether, and how much of, request and response bodies are captured into compliance records.

**What you see.** A single settings form.

**Controls.** Switches for store-request-body and store-response-body — turning one on raises a danger confirmation dialog, while turning one off applies immediately. Numeric inputs set the maximum inline body bytes (default 262144, i.e. 256 KiB), the maximum request bytes (default 10485760, i.e. 10 MiB), and the maximum response bytes (default 10485760). The response-body switch carries a note about streaming responses.

**Key concepts.** The maximum inline body bytes is the threshold below which a body is stored inline; a body above it spills to the spill store (configured on the Streaming Compliance tab). Enabling capture is gated behind an explicit compliance acknowledgement.

**Where the data comes from.** `systemApi` — `getPayloadCaptureConfig`, `updatePayloadCaptureConfig`.

## References

- `packages/control-plane-ui/src/routes/shellRouteConfig.tsx` — route registry and `nav: { sectionKey: 'compliance', ... }` blocks
- `packages/control-plane-ui/src/i18n/locales/en/nav.json` — sidebar labels
- `packages/control-plane-ui/src/pages/compliance/interception/` — Interception Domains list, detail, domain form, path form
- `packages/control-plane-ui/src/pages/compliance/aiguard/` — AI Guard Backend config and dry-run panel
- `packages/control-plane-ui/src/pages/compliance/streaming-compliance/SettingsStreamingComplianceTab.tsx` — Streaming Compliance settings
- `packages/control-plane-ui/src/pages/compliance/payload-capture/SettingsPayloadCaptureTab.tsx` — Payload Capture settings
- `packages/shared/storage/spillstore/` — spill storage for bodies above the inline threshold
- `packages/control-plane-ui/src/api/` — `interceptionDomainApi`, `aiGuardApi`, `systemApi`
- `tools/db-migrate/schema/compliance.prisma` — `InterceptionDomain`, `InterceptionPath`, `AIGuardConfig` models
