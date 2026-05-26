---
doc: nexus-response-markers
area: cross-cutting
service: foundation
tier: 1
updated: 2026-05-21
---

# Nexus Response Markers — Reference

**Source of truth:** `packages/shared/traffic/markers.go` `ExposeHeaders` (22-entry slice — including the reserved `x-nexus-attestation` slot) + this document. The E59-S2 namespace cleanup (2026-05-19) made `markers.go` authoritative and collapsed the live set from ~30 entries to 22.
Headers were renamed (drop `aigw-` prefix for ai-gateway-only writers) and several
deleted entirely (constants like `aigw-mode`, opaque hashes like `allowlist-version`,
debug-only like `routing-rule`, duplicates like `aigw-request-id`, derivables like
`quota-remaining`; the timing trio was replaced by HTTP-standard `Server-Timing`).

## Overview

Every Nexus service that processes a request (intercepts, MITM-decrypts, or runs hooks)
emits response headers identifying itself and what it did. These "response markers" let
operators and developers understand:

- Which Nexus services were on the request path
- Whether and how hooks were executed (passed, transformed, or rejected)
- Which provider/model served the response (AI Gateway only)
- Whether the response came from cache (AI Gateway only)
- The request id for correlating the response to audit logs

Response markers use the `x-nexus-*` naming convention, all lowercase, and are emitted with
consistent semantics across Agent, Compliance-Proxy, and AI Gateway. They are readable in
browser DevTools Network tabs, `curl -i` output, and browser JavaScript (via CORS exposure).

## Via Chain

The `x-nexus-via` header encodes the request flow through Nexus services as a
comma-separated chain in **request flow order** (the order services processed the request,
first to last). Each service that processes the response **prepends** itself to the
existing header value, so the final value reads left-to-right in request order.

### Examples

**Browser → AI Gateway only (no Agent, no Compliance-Proxy)**
```
x-nexus-via: ai-gateway
```

**Browser → Compliance-Proxy → AI Gateway**
```
x-nexus-via: compliance-proxy, ai-gateway
```

**Browser → Agent → Compliance-Proxy → AI Gateway**
```
x-nexus-via: agent, compliance-proxy, ai-gateway
```

**Absence**: if `x-nexus-via` is missing from the response, no Nexus service processed the
request (e.g., request bypassed interception, or forwarding/CONNECT-tunnel path without
MITM).

## Reading Markers in Browser DevTools

1. Open the page in your browser and press **F12** to open Developer Tools
2. Click the **Network** tab
3. Send the request (reload page, or trigger the API call)
4. Click the request row in the list
5. In the details pane, click the **Response Headers** (or **Headers** → **Response
   Headers**) tab
6. Scroll to find headers starting with `x-nexus-`. Markers are always present if the
   request was processed by at least one Nexus service

All `x-nexus-*` headers in `ExposeHeaders` are automatically exposed (CORS
`Access-Control-Expose-Headers`).

## Reading Markers via curl

```bash
curl -i -X POST http://localhost:3050/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}'
```

Markers appear in the response headers section.

## Reading Markers from Browser JavaScript

```javascript
fetch('http://localhost:3050/v1/chat/completions', {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ model: 'gpt-4', messages: [{role:'user', content:'hello'}] })
})
  .then(r => {
    console.log('Via:',           r.headers.get('x-nexus-via'));
    console.log('Request ID:',    r.headers.get('x-nexus-request-id'));
    console.log('Routed model:',  r.headers.get('x-nexus-routed-model'));
    console.log('Cache:',         r.headers.get('x-nexus-cache'));
    console.log('Server-Timing:', r.headers.get('Server-Timing'));
    return r.json();
  });
```

---

# Header Reference

The full live set is `packages/shared/traffic/markers.go` `ExposeHeaders` (22 entries — including the reserved `x-nexus-attestation` slot). Per-header semantics below; one subsection per
header still in `ExposeHeaders` (no sections for deleted headers — see "Removed in E59-S2"
appendix at the bottom for the historical names).

## Cross-Service Headers

### `x-nexus-via`

**Producer:** Any Nexus service that processes the request (Agent, Compliance-Proxy, or
AI Gateway)

**When present:** On every response where at least one Nexus service executed (success,
reject, streaming).

**When omitted:** When no Nexus service intercepted the request, or on transparent
forwarding/CONNECT-tunnel flows (which are observable only via Nexus Hub flow inventory
and audit log).

**Semantics:** Comma-separated list of service names in request flow order. Each service
**prepends** itself on the response path, so the final value reads left-to-right as
request order.

**Example:**
```
x-nexus-via: agent, compliance-proxy, ai-gateway
```

### `x-nexus-request-id`

**Producer:** The Nexus service that owns the per-request ID (typically AI Gateway when
in the path; otherwise the most-upstream Nexus service).

**When present:** On every response where at least one Nexus service executed (success,
reject, streaming).

**Semantics:** The canonical per-request UUID used in audit logs, traffic_event rows, and
linked from the Admin UI request detail view. Replaces the per-service
`x-nexus-{aigw,cp}-request-id` headers that were deleted in E59-S2.

**Example:**
```
x-nexus-request-id: a1b2c3d4-e5f6-7890-abcd-ef1234567890
```

### `Server-Timing` (HTTP-standard, RFC 8674)

**Producer:** AI Gateway (and any service that records meaningful timing segments).

**When present:** On success and streaming responses. Replaces the deleted
`x-nexus-aigw-latency-ms`, `x-nexus-aigw-upstream-ttfb-ms`,
`x-nexus-aigw-upstream-total-ms` trio.

**Semantics:** Standard HTTP `Server-Timing` value. Browsers and DevTools render it
natively in the "Timing" tab.

**Example:**
```
Server-Timing: hook;dur=12, upstream;dur=1250, total;dur=1287
```

---

## AI Gateway Headers

### `x-nexus-cache`

**Producer:** AI Gateway

**When present:** Always on the AI Gateway origin path — success, reject, streaming.
For streaming requests the value reflects request-side classification only (`bypass` /
`miss` / `hit`); a final cache state is in the `traffic_event` row.

**Semantics:** Cache classification: `hit` (response served from cache), `miss` (lookup
ran, no entry), `bypass` (cache skipped due to header / freshness rule / kill), or
`disabled`.

**Example:**
```
x-nexus-cache: hit
```

### `x-nexus-routed-provider`

**Producer:** AI Gateway

**When present:** Only when smart routing substituted the upstream provider.

**Semantics:** The actual upstream provider chosen by routing rules.

**Example:**
```
x-nexus-routed-provider: azure-openai
```

### `x-nexus-routed-model`

**Producer:** AI Gateway

**When present:** Only when smart routing substituted the upstream model.

**Semantics:** The actual upstream model chosen by routing rules.

**Example:**
```
x-nexus-routed-model: gpt-4-turbo
```

### `x-nexus-attempts`

**Producer:** AI Gateway

**When present:** Always, on every proxied response (success or upstream-error
passthrough).

**Semantics:** Total upstream attempts for this request, counting the first try plus
every L2 retry and L3 failover. `1` = first attempt succeeded.

**Example:**
```
x-nexus-attempts: 2
```

### `x-nexus-coerced`

**Producer:** AI Gateway

**When present:** Only when the gateway rewrote request parameters (currently
`max_tokens → max_completion_tokens` for OpenAI reasoning models, and similar adapter
auto-fills).

**Semantics:** Comma-separated `<from>→<to>` pairs. Provides transparency when Nexus
modifies the customer's request due to vendor-specific differences.

**Example:**
```
x-nexus-coerced: max_tokens→max_completion_tokens
```

### `x-nexus-upgraded-to`

**Producer:** AI Gateway

**When present:** Only when an auto-upgrade routing rule swapped the customer-requested
model for a successor model (E57 auto-upgrade flag).

**Semantics:** The customer-facing model code the response was actually produced with.

### `x-nexus-quota-used` / `x-nexus-quota-limit` / `x-nexus-quota-downgrade` / `x-nexus-quota-original-model` / `x-nexus-quota-warning`

**Producer:** AI Gateway

**When present:** On success responses where quota was evaluated. Streaming responses
omit `quota-used` (final count unknown until stream ends).

**Semantics:**
- `quota-used` — token usage reported by the upstream for this request.
- `quota-limit` — customer's quota limit for the billing period.
- `quota-downgrade` — present (value `true`) only when the requested model was downgraded
  due to quota.
- `quota-original-model` — present alongside `quota-downgrade`; the original model the
  customer requested.
- `quota-warning` — set (e.g., `critical`) when quota is critically low.

`quota-remaining` is derivable from `quota-limit − quota-used` and is not emitted as a separate header.

### `x-nexus-dry-run` / `x-nexus-estimate`

**Producer:** AI Gateway (conditional emission)

**When present:** Only on dry-run requests (`X-Nexus-Dry-Run: true` in the request) —
the gateway runs the codec + cost estimator but does not forward upstream.

**Semantics:** `x-nexus-dry-run: true` indicates the response was synthetic; `x-nexus-estimate`
carries the estimated cost / token breakdown JSON for the request.

---

## Hook Outcome Headers — service-prefixed

Three services run independent hook pipelines, so each prefixes its own hook outcome.

### `x-nexus-aigw-hook` / `x-nexus-cp-hook` / `x-nexus-agent-hook`

**Producer:** AI Gateway / Compliance-Proxy / Agent respectively.

**When present:** Whenever the producing service was in the path and ran (or skipped) a
hook pipeline. For Compliance-Proxy / Agent, only on MITM-intercepted responses; absent
on transparent forwarding.

**Semantics:** Standardised hook outcome format (see "Hook Outcome Format" below).

**Example:**
```
x-nexus-aigw-hook: passed:pii-redact,jwt-strip
x-nexus-cp-hook:   transformed:request-rewrite
x-nexus-agent-hook: rejected:malware-scan:virus-signature-match
```

---

## Compliance-Proxy / Agent identity headers

### `x-nexus-cp-domain-rule`

**Producer:** Compliance-Proxy

**When present:** On MITM responses (success, streaming, reject); absent on transparent
forwarding / CONNECT-tunnel flows.

**Semantics:** UUID of the matched interception-domains rule that triggered MITM for
this request.

### `x-nexus-agent-flow-id`

**Producer:** Agent

**When present:** On MITM responses (success, streaming, reject); absent on transparent
forwarding flows.

**Semantics:** Agent's internal flow UUID. Correlates to the local Agent audit log and
Agent metrics. Replaces the deleted `x-nexus-agent-domain-rule` (agent does not have a
separate domain-rule UUID — its flow ID is the only identifier needed).

---

## Reserved slot

### `x-nexus-attestation`

**Producer:** Reserved for E60 (traffic attestation).

**When present:** Not yet — the writer is unimplemented; the slot is reserved in
`ExposeHeaders` so browsers can already read the value once it ships, and so the doc +
code stay aligned.

---

# Hook Outcome Format

`x-nexus-{aigw,cp,agent}-hook` header values follow a standardised format. Built by
`traffic.FormatHookOutcome` in `packages/shared/traffic/markers.go`.

### `none`

No hook in the service ran for this request.

```
x-nexus-aigw-hook: none
```

### `passed:<hook1>,<hook2>,...`

One or more hooks ran and all passed without modifying the request body.

- `<hook*>` is the hook's stable slug (lowercase, kebab-case, e.g., `pii-redact`,
  `jwt-strip`, `rate-limit`).
- Multiple hooks are comma-separated in execution order.
- "Passed" means the hook evaluated to permit and did not modify the request body.

### `transformed:<hook1>,<hook2>,...`

One or more hooks ran and at least one modified the request body.

### `rejected:<hook>:<reason-slug>`

The pipeline halted due to a rejection by one hook. Only one hook and one reason are
recorded.

- `<reason-slug>` is sanitised to `[a-z0-9-]+` to prevent header / log injection.
- Common reason slugs: `quota-exceeded`, `malware-detected`, `policy-violation`,
  `signature-mismatch`, `rate-limit-hit`, `suspicious-request`.

---

# Streaming Response Behavior

For SSE or other streaming responses, headers must flush before the first byte of the
body. Only **request-side determinable** information is included.

## Headers Present on Streaming Responses

Anything in `ExposeHeaders` that the producing service can stamp before flushing the
first chunk — typically:

- `x-nexus-via`, `x-nexus-request-id`
- `x-nexus-cache` (request-side classification)
- `x-nexus-routed-provider`, `x-nexus-routed-model`
- `x-nexus-attempts`, `x-nexus-coerced`, `x-nexus-upgraded-to`
- `x-nexus-aigw-hook` / `x-nexus-cp-hook` / `x-nexus-agent-hook` (request-side pipeline)
- `x-nexus-cp-domain-rule`, `x-nexus-agent-flow-id`

## Headers Omitted on Streaming Responses

- `x-nexus-quota-used` — final token count unknown until stream ends.
- `Server-Timing` — final segments unknown at header flush time; consult the
  `traffic_event` row for final latency.
- Response-side hook outcomes — currently out of scope but the slot stays in
  `x-nexus-aigw-hook` for future use.

## Accessing omitted dimensions

All dimensions omitted from streaming headers are still recorded in the `traffic_event`
audit table. Use `x-nexus-request-id` to query the Admin UI traffic detail page for
complete request/response metadata, including final cache status, true latency, and
token usage.

---

# Reject Path

When a hook rejects a request, the rejecting service (Agent, Compliance-Proxy, or AI
Gateway) constructs an HTTP/1.1 403 (Forbidden) response directly.

- **`x-nexus-via` chain**: includes only services actually entered. If Compliance-Proxy
  rejects before forwarding to AI Gateway, `x-nexus-via: compliance-proxy` (no
  `ai-gateway`).
- **Hook outcome header**: uses the `rejected:<hook>:<reason>` format exclusively.
- **CORS exposure**: reject responses include the same `Access-Control-Expose-Headers`
  list (via `traffic.SetExposeHeaders`), so browsers can read markers on 403 too.
- **Response body**: the JSON / HTML reject page structure is unchanged; markers are
  added as HTTP headers only.

---

# CORS Exposure

To make `x-nexus-*` headers readable from browser JavaScript, each Nexus service emits
`Access-Control-Expose-Headers` listing the marker names. The slice is shared via
`traffic.ExposeHeaders`.

## Emission patterns

### Transparent proxy success path

Compliance-Proxy / Agent merge the Nexus marker names into any upstream
`Access-Control-Expose-Headers` using `traffic.MergeExposeHeaders(h, names...)`:

- Case-insensitive deduplication
- Preserves upstream's existing entries
- Appends Nexus marker names

### Synthetic responses (reject path, AI Gateway origin)

Service directly sets the full marker list via `traffic.SetExposeHeaders(h)`:

```
Access-Control-Expose-Headers: x-nexus-via, x-nexus-request-id, x-nexus-cache,
  x-nexus-routed-model, x-nexus-routed-provider, x-nexus-attempts, x-nexus-coerced,
  x-nexus-upgraded-to, x-nexus-quota-used, x-nexus-quota-limit,
  x-nexus-quota-downgrade, x-nexus-quota-original-model, x-nexus-quota-warning,
  x-nexus-dry-run, x-nexus-estimate, x-nexus-aigw-hook, x-nexus-cp-hook,
  x-nexus-agent-hook, x-nexus-cp-domain-rule, x-nexus-agent-flow-id,
  Server-Timing, x-nexus-attestation
```

## Complete Marker List

The 22-entry live list (which includes the reserved `x-nexus-attestation` slot) lives in
`packages/shared/traffic/markers.go` (variable `ExposeHeaders`). Adding or removing a
header requires editing that slice in the same PR as the doc update — this doc tracks
the slice 1:1.

---

# Audit Cross-Reference

When response markers cannot include complete information (final latency on streaming
responses, final cache state on stream end, hook decisions after response-side hooks
ship), the authoritative source is the `traffic_event` audit table.

1. **Copy `x-nexus-request-id`** from the response headers.
2. **Open Admin UI** → **Traffic** (or **Analytics** → **Request Log**).
3. **Search or filter by request id**.
4. **Click the matched row** to open the traffic detail panel.
5. View the complete metadata, including final cache status, true latency, final token
   usage, hook outcomes, provider/model routing, and quota state after request completion.

---

# Removed in E59-S2 (historical reference)

These header names appeared in earlier drafts of this doc and shipped briefly in code; the
E59-S2 namespace cleanup deleted them. Each was deleted for one of the reasons listed in
`markers.go` (constant, opaque-hash, debug-only, duplicate, derivable, dead).

| Removed header                       | Reason                                                  |
|--------------------------------------|---------------------------------------------------------|
| `x-nexus-aigw-request-id`            | Duplicate of `x-nexus-request-id`                       |
| `x-nexus-cp-request-id`              | Same                                                    |
| `x-nexus-aigw-mode`                  | Constant (`proxied`)                                    |
| `x-nexus-cp-mode`                    | Constant (`mitm`)                                       |
| `x-nexus-agent-mode`                 | Constant (`mitm`)                                       |
| `x-nexus-trace-id`                   | Replaced by `x-nexus-request-id` as the single ID       |
| `x-nexus-aigw-provider`              | Equivalent to upstream-resolved provider — debug-only   |
| `x-nexus-aigw-model`                 | Customer-supplied; echoed everywhere already            |
| `x-nexus-aigw-routing-rule`          | Debug-only; UI surfaces it from the traffic_event row   |
| `x-nexus-aigw-stream`                | Constant when present; client already knows from content-type |
| `x-nexus-aigw-latency-ms`            | Replaced by `Server-Timing`                             |
| `x-nexus-aigw-upstream-ttfb-ms`      | Same                                                    |
| `x-nexus-aigw-upstream-total-ms`     | Same                                                    |
| `x-nexus-aigw-quota-period`          | Constant per VK plan                                    |
| `x-nexus-aigw-quota-remaining`       | Derivable (`limit − used`)                              |
| `x-nexus-aigw-body-format`           | Debug-only; recoverable from request side               |
| `x-nexus-aigw-no-cache`              | Request-side header, not a response marker              |
| `x-nexus-agent-domain-rule`          | Agent has only one identifier (`agent-flow-id`)         |
| `x-nexus-allowlist-version`          | Opaque hash; not useful client-side                     |

Anything not listed in §"Header Reference" above and not present in
`markers.go ExposeHeaders` is no longer emitted.

---

# Related Documents

- **OpenAPI schema:** `docs/users/api/openapi/ai-gateway/e31-s4-response-markers.yaml` —
  machine-readable marker header schemas for each service.
- **SDD document:** `docs/developers/specs/e31/e31-s4-response-markers.md` — user
  stories, acceptance criteria, and test plan.
- **E59-S2 spec:** `docs/developers/specs/e59/e59-s2-header-namespace-cleanup.md` — the
  rename / deletion list applied to land the current 22-entry surface.
