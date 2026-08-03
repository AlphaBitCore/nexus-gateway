# Nexus Headers

This is the registry of every HTTP header Nexus owns, in both directions.

**Response markers** (§2–§6): Nexus stamps a small set of `X-Nexus-*` response headers — **markers** — on every response that flows through one of its services. The client reads them to observe what Nexus did: which services the request traversed, whether the cache hit, what the routing layer decided, which hook pipeline ran (and whether it modified the body), where in the chain the request was rejected if it was, and what quota state applied. Markers are the only way clients see Nexus on a successful (200) response — there is no body envelope, no JSON wrapper, no header rename. The wire stays clean; the markers say "Nexus was here, and here is what it did".

**Request headers** (§8): the headers a client sends that Nexus itself reads — virtual-key carriers, correlation ids, feature flags — plus how the AI Gateway composes its CORS request allowlist from them. A request header missing from that registry is invisible to governance: it can drift into naming violations, get silently rejected at browser preflight, or survive as a ghost that nothing writes. Both directions live in this one file so neither can escape again.

Anchor packages:

- `packages/shared/traffic/markers.go` — the **single source of truth** for both direction registries: the response-marker allowlist (`ExposeHeaders`), the gateway-read request-header list (`AcceptHeaders`), the via-chain helper (`PrependVia`), the generic per-hop chain helper (`PrependChain`), the two CORS helpers (`SetExposeHeaders`, `MergeExposeHeaders`), and the canonical hook-outcome formatter (`FormatHookOutcome`).
- `packages/ai-gateway/internal/ingress/proxy/` — AI Gateway's direct `w.Header().Set(...)` call sites for every gateway-owned marker, split across per-stage files: `proxy_respheaders.go` (via / mode reservation / routing markers / `Server-Timing`), the cache paths (`proxy_cache.go`, `proxy_cache_hits.go`, `stage_cache.go`), the hook stages (`stage_hooks.go`, `stage_respond.go`), and `proxy_routing.go` (quota markers). See §6.1.
- `packages/shared/transport/tlsbump/markerhook.go` + `markercontext.go` — the shared marker-injection layer used by Compliance Proxy (`identity="compliance-proxy"`) and the Agent's tlsbump path (`identity="agent"`).
## 1. Naming convention

- **Both directions use mixed-case `X-Nexus-*`.** Request headers and response headers follow the same canonical mixed-case form (e.g. `X-Nexus-Request-Id`, `X-Nexus-Mode`, `X-Nexus-Hook`). HTTP header names are case-insensitive on the wire and Go's `http.Header.Set` / `Get` canonicalises automatically, but **all literal strings in the codebase use mixed-case** so code reads consistently with what observers see on the wire.
- **No per-service prefix.** Markers are unified — there is one `X-Nexus-Hook`, one `X-Nexus-Mode`, one `X-Nexus-Domain-Rule`, not separate `aigw-` / `cp-` / `agent-` variants. Multi-hop composition is handled by the via-chain alignment described in §4, not by prefixing.
- **`X-Nexus-Request-Id` is the single correlation header.** There is no separate `X-Nexus-Trace-Id` / `X-Nexus-Flow-Id` / `X-Nexus-Correlation-Id`. Every service that needs to thread a request across hops reads and writes this one header. AI Gateway's middleware generates the value if absent; CP and Agent honour the incoming value or generate one on their shared tlsbump path (the agent's per-flow id travels only as the audit row's connection id, not on this header); downstream services log under this single key.
- **The `X-` prefix** is intentional, not a typo. RFC 6648 deprecates `X-` for new header names; the family keeps it because consistency within `X-Nexus-*` matters more than RFC purity.

## 2. The 17-entry catalogue

`packages/shared/traffic/markers.go` defines a slice `ExposeHeaders` — the **authoritative list** of marker headers Nexus exposes to browser-side JavaScript via the `Access-Control-Expose-Headers` CORS header. Every marker either sits in this slice (always exposed) or is omitted entirely from the spec.

### 2.1 Cross-service correlation (2)

| Marker | Set by | Value | Purpose |
|---|---|---|---|
| `X-Nexus-Via` | every Nexus service via `traffic.PrependVia` | comma-separated service names, idempotent dedup | The "via chain". Each hop prepends its own name iff not already present. See §4. |
| `X-Nexus-Request-Id` | AI Gateway middleware (`packages/ai-gateway/internal/platform/middleware/middleware.go`); echoed by CP and Agent via the shared tlsbump `stampMarkers` (`packages/shared/transport/tlsbump/markerhook.go`) | UUID string | The **only** correlation header. AI Gateway generates one if absent on the request; CP and Agent reflect the incoming value onto the response, generating one when absent. Same header on request and response. |

### 2.2 AI Gateway request outcome (5)

All set in `packages/ai-gateway/internal/ingress/proxy/`: routing markers in `proxy_respheaders.go`, the cache marker in the cache paths (`proxy_cache.go`, `proxy_cache_hits.go`, `stage_cache.go`), and hook / coercion markers in the hook stages (`stage_hooks.go`, `stage_respond.go`, `proxy_responses.go`, `stream_preamble.go`).

| Marker | Set by stage | Value | Purpose |
|---|---|---|---|
| `X-Nexus-Cache` | response-cache decision | `HIT` \| `MISS` (string-equal to `audit.CacheStatusHit` / `audit.CacheStatusMiss`) | Whether the gateway response cache served this request. |
| `X-Nexus-Routed-Model` | after routing-rule dispatch succeeds (when `result.Substituted`) | model code string | The actual upstream model the routing layer dispatched to. Present only when smart-routing substituted the requested model. |
| `X-Nexus-Routed-Provider` | after routing-rule dispatch succeeds (when `result.Substituted`) | provider name string | The upstream provider that handled the request. Present alongside `X-Nexus-Routed-Model`. |
| `X-Nexus-Attempts` | every gateway response | integer-as-string (≥1) | Number of routing attempts the gateway made. `1` = first-try success; `2+` = at least one L2 retry or L3 failover. Emitted on every response (no `> 0` gate) so observers always see the value. |
| `X-Nexus-Coerced` | after hook pipeline if hooks coerced fields | comma-separated field names | The set of canonical-payload fields the request-hook pipeline coerced (e.g. `temperature,top_p`). Empty when no coercion ran. |

### 2.3 AI Gateway quota (5)

Set by the quota-decision stage in `proxy_routing.go`. Present only on responses where the caller's virtual key has a budget configured.

| Marker | Value | Purpose |
|---|---|---|
| `X-Nexus-Quota-Used` | `"%.2f"` USD | Current-month cost usage for the caller's virtual key. |
| `X-Nexus-Quota-Limit` | `"%.2f"` USD | Budget limit configured for the caller's virtual key. |
| `X-Nexus-Quota-Downgrade` | `"true"` (only set when downgrade fires) | The routing layer fell back to a cheaper model because the quota policy demanded downgrade-on-threshold-exceeded. |
| `X-Nexus-Quota-Original-Model` | model code string | When `Quota-Downgrade=true`, the original model the client requested (before auto-downgrade). |
| `X-Nexus-Quota-Warning` | free-text message | Operator-facing warning from the quota decision (set when policy returns a warning even on `allow`). |

### 2.4 Per-hop chain markers (2)

These markers compose across hops via `traffic.PrependChain` so the value at index `i` corresponds to the via-chain entry at index `i` (the entry closest to the client is at position 0). See §4 for the alignment contract.

| Marker | Set by | Value contract | Purpose |
|---|---|---|---|
| `X-Nexus-Hook` | every Nexus service that runs a hook pipeline (AI Gateway, CP, Agent) | per-hop value follows `FormatHookOutcome` (§3); multi-hop composed via `PrependChain` | Hook-pipeline outcome per hop. |
| `X-Nexus-Mode` | CP + Agent set non-empty values; AI Gateway stamps an empty value to reserve its position | `inspect` \| `deny` \| `""` (empty, AI Gateway's reserved-position value) | Per-hop service mode. `inspect` = hook pipeline ran (CP / Agent tlsbump). `deny` = synthetic reject response (CP / Agent tlsbump reject path). Empty = AI Gateway position holder (it has no mode concept) so outer hops keep 1:1 alignment with the via chain. Which service produced a given entry is read from the via-chain entry at the same index (§4) — never from the mode value itself. |

### 2.5 Compliance-proxy domain rule (1)

| Marker | Set by | Value | Purpose |
|---|---|---|---|
| `X-Nexus-Domain-Rule` | CP via tlsbump `stampMarkers` / `stampRejectMarkers` | UUID string of matched `Instance.Domain.ID` | The compliance-proxy interception-domain rule that matched the target host. Empty (header omitted) when no domain rule matched (passthrough traffic). CP-only — no chain semantics; only CP can set this header. |

### 2.6 Standard HTTP timing (1)

| Marker | Set by | Value | Purpose |
|---|---|---|---|
| `Server-Timing` | AI Gateway `proxy_respheaders.go` | RFC 8674-formatted timing list (e.g. `gw;dur=2, upstream-ttfb;dur=80, upstream-total;dur=140`) | Standard HTTP timing exposed for browser DevTools' Network → Timing panel. Native browser support. |

### 2.7 Attestation (1)

| Marker | Direction | Set by | Notes |
|---|---|---|---|
| `X-Nexus-Attestation` | **request only** in production paths; listed in response `ExposeHeaders` for reverse-proxy edge cases | Agent's attestation signer (`packages/agent/internal/identity/attestation/signer.go`) stamps it on every outbound CONNECT request | Compliance Proxy peeks this on every CONNECT and, if valid, tunnels the flow transparently (skipping MITM + hooks). In the response `ExposeHeaders` allowlist so reverse-proxy deployments that surface the request header back to the client do not strip it; the production response path does not put attestation values on responses. |

### Catalogue totals

| Category | Count |
|---|---|
| 2.1 Correlation | 2 |
| 2.2 AI Gateway outcome | 5 |
| 2.3 AI Gateway quota | 5 |
| 2.4 Per-hop chain (`X-Nexus-Hook`, `X-Nexus-Mode`) | 2 |
| 2.5 CP domain rule | 1 |
| 2.6 HTTP timing | 1 |
| 2.7 Attestation | 1 |
| **Total** | **17** |

The static `ExposeHeaders` slice in `markers.go` has exactly 17 entries — both the name set and the total count are enforced by `TestExposeHeaders_HasAllMarkers`.

## 3. `FormatHookOutcome` — the per-hop hook-outcome value format

Every per-hop entry in `X-Nexus-Hook` carries one of four shapes, rendered by `FormatHookOutcome` in `packages/shared/traffic/markers.go`:

| Input shape | Rendered value | Meaning |
|---|---|---|
| `Passed: []`, `Rejected: ""` | `none` | No hook ran (compliance disabled, no hook matched this route, or the pipeline was skipped). |
| `Passed: [a, b]`, `Transformed: false` | `passed:a,b` | Listed hooks all passed; none modified the request body. |
| `Passed: [a, b]`, `Transformed: true` | `transformed:a,b` | Listed hooks all passed; at least one modified the body. |
| `Rejected: hookSlug`, `RejectReason: <free text>` | `rejected:<hookSlug>:<sanitized-reason>` | The pipeline was rejected. Reason text is sanitised. |

**Reason sanitisation** (`sanitizeSlug` helper) lower-cases the input and keeps only `[a-z0-9-]`. Non-allowed character runs collapse into a single hyphen; leading and trailing hyphens are stripped. All-stripped or empty input produces `unknown`. This is what prevents header / log injection from hook reason strings that come from rule-pack metadata: an unsanitised reason like `"PII: SSN detected\r\nX-Evil: x"` would inject a forged header line; after sanitisation it becomes `pii-ssn-detected-x-evil-x`.

**Reject halts the pipeline.** When a hook returns `RejectHard` / `BlockSoft`, any previously-accumulated `Passed` hooks are discarded and the rendered value reports only the reject attribution. This keeps each per-hop entry single-purpose: "this hop reached this outcome", not "this hop did all this and then rejected".

**Multi-hop composition** is the caller's job — outer hops prepend their `FormatHookOutcome` to the existing `X-Nexus-Hook` chain via `PrependChain` (§4). The formatter renders only the single-hop value.

## 4. The via chain + per-hop chain alignment

### Via chain (`PrependVia`)

`PrependVia(h, "<service-name>")` adds the service to the front of the `X-Nexus-Via` header iff not already present (case-insensitive comma-split match). Idempotent: calling it twice with the same name is a no-op.

Production maximum is **2 hops**: any one of `{agent, compliance-proxy}` in front of `ai-gateway`, or a single service alone. There is no production traffic path where all three are chained.

| Path | Resulting `X-Nexus-Via` |
|---|---|
| Client → AI Gateway direct | `ai-gateway` |
| Client → Agent → AI Gateway | `agent, ai-gateway` |
| Client → Compliance Proxy → AI Gateway | `compliance-proxy, ai-gateway` |
| Client → Agent → upstream non-Nexus (passthrough through agent's transparent path) | `agent` |
| Client → Compliance Proxy → upstream non-Nexus | `compliance-proxy` |

The hop closest to the client comes at position 0, matching the order browsers display in dev tools and HTTP's standard `Via` header.

### Per-hop chain (`PrependChain`)

`PrependChain(h, key, value)` is the generic helper for `X-Nexus-Hook` and `X-Nexus-Mode`. Its contract:

- **First hop (header absent on entry):** `h.Set(key, value)`. The value lands as a single entry.
- **Later hop (header present on entry, even with empty value):** prepend with `, ` separator — `h.Set(key, value+", "+existing)`. The new value lands at position 0; the existing chain shifts right by one.

The "header present but empty" case is the one that preserves strict 1:1 alignment with `X-Nexus-Via`. AI Gateway has no mode concept, so it stamps `X-Nexus-Mode: ""` (empty value, header present) to reserve its position. When an outer hop (agent, CP) then prepends, the result is `X-Nexus-Mode: inspect, ` — trailing empty position for AI Gateway. Reader splits by `,` and trims to `["inspect", ""]`, which aligns 1:1 with `X-Nexus-Via: agent, ai-gateway` → `["agent", "ai-gateway"]`. Reader infers: agent's mode is `inspect`; ai-gateway has no mode (empty position).

Worked examples:

| Path | `X-Nexus-Via` | `X-Nexus-Mode` | `X-Nexus-Hook` |
|---|---|---|---|
| AI Gateway alone, no hook | `ai-gateway` | `` (empty value, header present) | `none` |
| AI Gateway alone, hook ran | `ai-gateway` | `` (empty) | `passed:rate-check` |
| Agent → AI Gateway, both ran hooks | `agent, ai-gateway` | `inspect, ` (trailing empty) | `transformed:redact, passed:rate-check` |
| CP → AI Gateway, CP rejected | `compliance-proxy, ai-gateway` | `deny, ` (trailing empty) | `rejected:pii-detector:contains-ssn, none` |
| CP → AI Gateway, CP inspect-passed | `compliance-proxy, ai-gateway` | `inspect, ` (trailing empty) | `transformed:redact, passed:rate-check` |

Reader parses by `strings.Split(value, ",")` then trims; the resulting slices align 1:1 with the via slice.

## 5. CORS `Access-Control-Expose-Headers` contract

Browsers block JavaScript from reading any response header by default unless the server lists the header name in `Access-Control-Expose-Headers`. Two helpers in `markers.go` populate this:

- **`SetExposeHeaders(h)`** — writes the full `ExposeHeaders` slice, *replacing* any existing value. Use this on synthetic responses (Compliance Proxy reject path's 403/451; AI Gateway origin responses where no upstream CORS state exists) where the marker layer is authoritative.
- **`MergeExposeHeaders(h, names...)`** — appends the given names to any existing value, deduping case-insensitively. Use this on the transparent-proxy success path where an upstream may have already set its own expose list that must be preserved (e.g. the upstream API exposes its own `X-Request-Id` and we must not stomp it).

The choice between the two is a correctness call: a transparent-proxy `Set` would strip upstream CORS state and silently break browser-side code that reads upstream headers; a synthetic-response `Merge` would carry forward irrelevant headers from a previously-cached state if the synthesizer reused an `http.Header` map. The wrong choice does not crash anything — it produces subtle browser-side bugs hard to attribute back to Nexus.

## 6. Per-service injection mechanics

Three distinct code paths produce markers.

### 6.1 AI Gateway — direct `w.Header().Set` calls

The AI Gateway sets every gateway-owned marker by calling `w.Header().Set("X-Nexus-...", value)` at the relevant pipeline stage. The call sites are split across per-stage files in `packages/ai-gateway/internal/ingress/proxy/` (`proxy.go` itself sets no markers):

- Cache decision → `X-Nexus-Cache` (`proxy_cache.go`, `proxy_cache_hits.go`, `stage_cache.go`)
- Routing-rule dispatch → `X-Nexus-Routed-Model`, `X-Nexus-Routed-Provider`, `X-Nexus-Attempts` (`proxy_respheaders.go`)
- Request-hook completion → `X-Nexus-Hook`, optionally `X-Nexus-Coerced` (`stage_hooks.go`, `stage_respond.go`, `proxy_responses.go`, `stream_preamble.go`)
- Quota decision → all five `X-Nexus-Quota-*` (`proxy_routing.go`)
- Response timing → `Server-Timing` (`proxy_respheaders.go`)
- Via stamp + Mode position reservation → `traffic.PrependVia(w.Header(), "ai-gateway")` + `w.Header().Set("X-Nexus-Mode", "")` in `proxy_respheaders.go` (also on the hook-reject path in `stage_hooks.go`); the empty Mode stamp is what preserves 1:1 alignment for outer-hop prepends — §4

`X-Nexus-Request-Id` is set by the middleware in `packages/ai-gateway/internal/platform/middleware/middleware.go` before any handler runs (generates a UUID if the client did not supply one).

### 6.2 Compliance Proxy + Agent (tlsbump path) — `stampMarkers` from context

`packages/shared/transport/tlsbump/markerhook.go` exposes `stampMarkers(ctx, h, identity)` and the `markerHook(ctx, identity)` wrapper. The handler in `forward_handler.go` stashes a `*CPMarker` into the request context (with the request ID, the matched domain-rule UUID, and the hook-outcome input); both the streaming-response and non-streaming-response write paths invoke `markerHook` (which reads back the `*CPMarker` via `CPMarkerFromContext`) before writing the response head.

`identity` is the per-call switch: `compliance-proxy` and `agent` both flow through the same shared infrastructure — same context contract, same `inspect` vs `deny` mode taxonomy, same `PrependChain` / `MergeExposeHeaders` call shape. The only difference is the value stamped on `X-Nexus-Via` (the identity string itself).

When `*CPMarker` is absent (compliance disabled fast-path, or a CONNECT tunnel that never went through the inspector), only `X-Nexus-Via` is stamped — downstream consumers treat the absence of `X-Nexus-Hook` / `X-Nexus-Mode` / `X-Nexus-Domain-Rule` entries as "no inspection ran".

### 6.3 Agent — shared bump-path markers (`identity="agent"`)

The agent's inspect path runs through the same shared marker layer as the
compliance proxy — `stampMarkers` in `packages/shared/transport/tlsbump/markerhook.go`
— constructed with `WithIdentity("agent")` so `agent` is prepended to the
`X-Nexus-Via` chain. It stamps the same canonical chain markers: the hook
outcome on `X-Nexus-Hook`, the mode (`inspect` / `deny`) on `X-Nexus-Mode`, and
`X-Nexus-Request-Id` for correlation (see §1), using the `traffic` chain helpers
(`PrependVia` / `PrependChain`) so multi-hop values stay aligned 1:1 with the via
chain. There is no agent-specific marker injector and no per-service header
prefix — every hop writes the canonical `X-Nexus-*` headers.

## 7. Operations

### Adding a new response marker

1. **Define** — add the mixed-case header name to `ExposeHeaders` in `packages/shared/traffic/markers.go`. Update the want-list in `markers_test.go`.
2. **Emit** — pick the right injection mechanic for your service (§6) and write the setter. Set the value as a string; never set an empty string except as a 1:1-alignment placeholder (the AI Gateway `X-Nexus-Mode` reservation is the canonical case).
3. **Document** — add a row to the right §2 sub-table in this doc.

The `markers_test.go` test (`TestExposeHeaders_HasAllMarkers`) is the gate that catches step 1 drift: a marker you set but did not add to the allowlist will show up in browser dev tools as "blocked by CORS"; a marker in the allowlist with no setter is dead-allowlist noise.

### Adding a new request header

1. **Name it** — mixed-case `X-Nexus-<Thing>` for Nexus-owned headers, no service segment (§1). The `X-Nexus-` prefix is hard-denylisted by the forward-header layer, so the new header is structurally guaranteed never to reach an upstream provider and an operator cannot re-allow it.
2. **Define** — add the name to `AcceptHeaders` in `packages/shared/traffic/markers.go` and to the want-list in `markers_accept_test.go`, and write the read site. A read site without the `AcceptHeaders` entry works for server-side clients but silently fails browser preflight; an entry without a read site is registry noise.
3. **Decide the trust model** — a client-supplied value is client-controlled. Either treat it as opaque (correlation ids), validate-and-reject, or strip it at ingress if it could impersonate an internal actor (Control Plane's `X-Nexus-Initiated-By` strip is the precedent).
4. **Document** — add a row to the §8 roster.

Never add a second correlation header — `X-Nexus-Request-Id` is the only one, by design (§1).

### Renaming or retiring a request header

A rename is a wire break for every caller on the old name, and the failure mode is usually silent (the header is ignored, not rejected). The pattern — see `X-Nexus-No-Cache` and `X-Nexus-Admin-Key` in §8.4 for the two live instances: the read site dual-reads canonical-then-deprecated, the deprecated name stays in `AcceptHeaders` so browser callers keep passing preflight, a test pins the alias so a cleanup cannot silently drop it, and the CHANGELOG carries the migration note. Retire the alias, its `AcceptHeaders` entry, its pinning test, and its doc row together, in one release that says so.

### Reading markers client-side

Browser JS (with CORS exposure in place):

```js
const resp = await fetch('https://api.example.com/v1/chat/completions', { ... });
console.log('via:', resp.headers.get('X-Nexus-Via'));
console.log('cache:', resp.headers.get('X-Nexus-Cache'));
console.log('hook chain:', resp.headers.get('X-Nexus-Hook')); // split by "," to align with via
console.log('mode chain:', resp.headers.get('X-Nexus-Mode')); // same alignment; trailing empty position is meaningful
```

Header name lookups are case-insensitive in browsers and in Go; the canonical form on the wire and in the codebase is mixed-case `X-Nexus-*`. A header that the server set but did not expose via CORS returns `null` from `resp.headers.get(...)` in cross-origin requests — the symptom of a server-side allowlist miss.

### Quick debug from a shell

```sh
curl -sI -X POST https://api.example.com/v1/chat/completions \
    -H "Authorization: Bearer $VK" \
    -H "Content-Type: application/json" \
    -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}' \
    | grep -iE '^x-nexus-|^server-timing'
```

The `-iE` flag matches case-insensitively so the canonical mixed-case headers all show up. A `curl -I` (HEAD) is not enough — the gateway code paths that set markers are tied to the POST handler. Use `-X POST` + a real body and grep with the `-i` flag for the marker prefix. Every Nexus-stamped response will show at minimum `X-Nexus-Via` and `X-Nexus-Request-Id`.

## 8. Request headers (client → Nexus)

### 8.1 AI Gateway data-plane roster

These are the headers the gateway's own read sites depend on. The authoritative list is `AcceptHeaders` in `packages/shared/traffic/markers.go`; `markers_accept_test.go` pins its membership.

| Header | Read by | Purpose | Status |
|---|---|---|---|
| `X-Nexus-Virtual-Key` | `packages/ai-gateway/internal/auth/vkauth/` | Explicit virtual-key carrier, honoured on every route. | Shipped |
| `Authorization: Bearer <vk>` | vkauth | Universal VK carrier — what OpenAI-compatible SDKs send natively. | Shipped |
| `x-api-key` | vkauth, Anthropic-shape routes only | VK carrier matching the Anthropic SDK convention. Vendor spelling kept. | Shipped |
| `x-goog-api-key` | vkauth, Gemini-shape routes only | VK carrier matching the Gemini SDK convention. The `?key=` URL carrier is deliberately not accepted (credentials must not enter access logs). | Shipped |
| `api-key` | vkauth, Azure-OpenAI-shape routes only | VK carrier matching the Azure OpenAI SDK convention. | Shipped |
| `X-Nexus-Request-Id` | AI Gateway middleware; every service | The single correlation header — honoured if present, generated if absent, echoed on the response (§1, §2.1). | Shipped |
| `X-Request-Id` | proxy pipeline setup | The caller's own request id, persisted to `traffic_event.external_request_id` so an external system can join gateway rows to its logs. Never rewritten. | Shipped |
| `X-Nexus-End-User-Id` | admission (`extractEndUserID`) | The caller's own end-user tag, persisted to `traffic_event.end_user_id` so an external system can join gateway traffic to its user table per user. The header is the only source, on every ingress shape. A provider's own end-user field (the OpenAI shape's top-level `user` or `safety_identifier`, the Anthropic shape's `metadata.user_id`) identifies the caller's end user to that provider and answers a different question, so it is not read here. Opaque and caller-asserted: never validated, never resolved against Nexus identities, never used for quota / routing / IAM — meaningful only within the caller's VK scope. Trimmed, capped at 256 bytes. **Stored verbatim and NOT redacted**: the compliance pipeline governs captured bodies, not this promoted column, so a caller that puts PII (an email) in its end-user ids is storing that PII queryably — use opaque ids. Both tags are extracted after the body is read, so rows rejected earlier (auth failure, rate limit, payload cap) carry NULL. | Shipped |
| `X-Nexus-Session-Id` | admission (`extractSessionID`) | The caller's session/conversation tag — the grain between the end-user tag and `X-Request-Id`, persisted to `traffic_event.session_id` so an external system can group one conversation's requests (cost per thread, dialogue replay). Header-only: chat protocols carry no reliable native session field. Same trust model as the end-user tag; trimmed, capped at 256 bytes. | Shipped |
| `X-Nexus-No-Cache` | cache stage (`noCacheRequested`) | Any non-empty value skips the response-cache lookup and, with it, the cache write. Operator emergency-passthrough `bypassCache` takes precedence and cannot be overridden by this header. | Shipped |
| `X-Nexus-Aigw-No-Cache` | cache stage (`noCacheRequested`) | Deprecated spelling of the above, dual-read for compatibility. | Deprecated alias |
| `Content-Type` | admission, routing | Body-format negotiation on JSON routes. | Shipped |

Headers the gateway relays to providers without reading them (`anthropic-beta`, `openai-organization`, `x-goog-user-project`, …) are not listed here — their registry is the forward-header allowlist (`packages/ai-gateway/internal/execution/forwardheader/defaults.yaml`, operator-overridable per its own contract).

### 8.2 CORS request-allowlist composition

The gateway composes `Access-Control-Allow-Headers` itself; it is not an operator-authored list. The wiring layer builds

```
AcceptHeaders  ∪  forward-header allowlist (request side, all formats)  ∪  cors.allowedHeaders (yaml)
```

via `middleware.UnionHeaderNames`, with the forward-header part derived at startup from the resolved allowlist (`Resolved.AllRequestHeaders`) so the two registries cannot drift. The union is taken across every ingress format because preflight happens before format dispatch — a per-format subset would reject at preflight a header the matching adapter would happily forward.

The yaml key **extends** the built-in set and cannot shrink it. Only the extend direction is exposed because an operator-authored replacement has exactly two failure modes: under-listing (the gateway's own reads or forwards silently fail preflight — the failure mode hand-maintained lists produce) and over-listing (harmless: the gateway ignores names it does not read, and the forward-header gate still default-denies them toward providers).

Every CORS response carries `Vary: Origin` — including the disallowed-origin response, whose distinguishing feature is the absence of CORS headers — so a shared cache never serves one origin's copy to another. A preflight from a disallowed origin receives a bare 204 with no allow-list readout.

### 8.3 Cross-service and control-plane headers

Outside the data plane, these Nexus-owned request headers exist. They are internal or admin-facing; none are CORS-relevant.

| Header | Direction | Purpose |
|---|---|---|
| `X-Nexus-Admin-Key` | operator/CLI → Control Plane | Admin API key carrier (HMAC-hashed lookup). The pre-namespace `x-admin-key` spelling is a deprecated dual-read alias; the canonical name wins when both arrive. |
| `X-Nexus-Actor-Id` / `-Actor-Name` / `-Actor-User-Id` / `-Actor-Token-Id` | Control Plane → Hub | Actor attribution on proxied admin calls. Internal hop only. |
| `X-Nexus-Initiated-By` | Control Plane internal | Marks self-dispatched requests. **Stripped at ingress** — a client-supplied value is discarded because it could forge internal attribution. The precedent for any identity-bearing request header. |
| `X-Nexus-Attestation` | Agent → Compliance Proxy | Attestation on CONNECT; §2.7. |
| `X-Thing-Id` | Agent → Hub | Thing self-identification on upload/diag calls; the Hub trusts the mTLS-resolved identity, not this header alone. |
| `X-Enrollment-Token` | Agent → Hub | One-time enrollment credential. |
| `X-RS-Token` | CP / AI Gateway → AI-Guard webhook | Shared-secret auth toward the judge backend (`packages/shared/identity/rstokenauth/`). |

### 8.4 Deprecated aliases

| Deprecated | Canonical | Read-side compat | Retire |
|---|---|---|---|
| `X-Nexus-Aigw-No-Cache` | `X-Nexus-No-Cache` | Dual-read in the cache stage; alias kept in `AcceptHeaders` so browser callers pass preflight; pinned by the cache-stage bypass test. | Together with its `AcceptHeaders` entry and pinning test, in a release whose CHANGELOG says so. |
| `x-admin-key` | `X-Nexus-Admin-Key` | Dual-read in Control Plane `AdminAuth`, canonical wins on conflict; pinned by the deprecated-header and precedence tests. | Together with the nexus CLI's last release that sent it. |

## References

- `packages/shared/traffic/markers.go` — both direction registries (`ExposeHeaders`, `AcceptHeaders`) + chain and CORS helpers
- `packages/shared/traffic/markers_test.go`, `packages/shared/traffic/markers_accept_test.go` — registry-pinning tests
- `packages/shared/transport/tlsbump/markerhook.go`, `packages/shared/transport/tlsbump/markercontext.go` — shared CP/Agent marker injection
- `packages/ai-gateway/internal/ingress/proxy/proxy_respheaders.go` — via / mode reservation / routing markers / `Server-Timing`
- `packages/ai-gateway/internal/ingress/proxy/proxy_routing.go` — quota markers
- `packages/ai-gateway/internal/ingress/proxy/stage_cache.go` — cache marker + `noCacheRequested` dual-read
- `packages/ai-gateway/internal/auth/vkauth/vkauth.go` — virtual-key carrier reads
- `packages/ai-gateway/internal/platform/middleware/middleware.go` — request-id middleware + CORS middleware (`UnionHeaderNames`)
- `packages/ai-gateway/cmd/ai-gateway/wiring/routes.go` — CORS request-allowlist composition
- `packages/ai-gateway/internal/execution/forwardheader/` — provider-forward allowlist (`Resolved.AllRequestHeaders`)
- `packages/control-plane/internal/platform/middleware/adminauth.go` — `X-Nexus-Admin-Key` dual-read
- `packages/agent/internal/identity/attestation/signer.go` — `X-Nexus-Attestation` request signer
